package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/loihoangthanh1411/profiler/internal/config"
	"github.com/loihoangthanh1411/profiler/internal/db"
	"github.com/loihoangthanh1411/profiler/internal/prometheus"
	"github.com/loihoangthanh1411/profiler/internal/watcher"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting vGPU profiler (event-driven)...")

	cfg := config.Load()

	// Parse labels
	trainKey, trainVal, err := cfg.WatchLabelParts()
	if err != nil {
		log.Fatalf("Invalid WATCH_LABEL: %v", err)
	}
	profileKey, profileVal, err := cfg.ProfilingLabelParts()
	if err != nil {
		log.Fatalf("Invalid PROFILING_LABEL: %v", err)
	}

	log.Printf("Prometheus URL     : %s", cfg.PrometheusURL)
	log.Printf("VRAM metric        : %s", cfg.VRAMMetric)
	log.Printf("Training label     : %s=%s", trainKey, trainVal)
	log.Printf("Profiling label    : %s=%s", profileKey, profileVal)
	log.Printf("Profiling duration : %s", cfg.ProfilingDuration)
	log.Printf("Health port        : %s", cfg.HealthPort)
	log.Printf("DB host            : %s:%d/%s", cfg.DBHost, cfg.DBPort, cfg.DBName)

	// ---- Prometheus client ----
	promClient, err := prometheus.NewClient(cfg.PrometheusURL)
	if err != nil {
		log.Fatalf("Failed to create Prometheus client: %v", err)
	}

	// ---- PostgreSQL store ----
	store, err := db.NewStore(cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode)
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	defer store.Close()
	log.Println("Connected to PostgreSQL")

	// ---- K8s pod watcher (two informers) ----
	w, err := watcher.New(trainKey, trainVal, profileKey, profileVal)
	if err != nil {
		log.Fatalf("Failed to create pod watcher: %v", err)
	}

	// ---- Graceful shutdown ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %s, shutting down...", sig)
		cancel()
	}()

	// ---- Startup orphan sweep ----
	// On (re)start, find any existing pods with the profiling label.
	// If they have been Running longer than ProfilingDuration, they are
	// orphans from a previous crash — delete them immediately.
	// If still within the window, resume the profiling goroutine.
	sweepOrphanPreRunPods(ctx, cfg, promClient, store, w, profileKey, profileVal)

	// ---- HTTP health server (liveness / readiness probes) ----
	go startHealthServer(cfg.HealthPort, store)

	// Start the watcher in the background
	go w.Run(ctx)

	log.Println("Watching for events... (idle)")

	// ---- Event loop ----
	for {
		select {
		case <-ctx.Done():
			log.Println("Profiler stopped.")
			return

		case event := <-w.CompletionEvents():
			handlePodCompletion(ctx, cfg, promClient, store, event)

		case event := <-w.PreRunEvents():
			// Spawn goroutine: wait → query → save → delete pod
			go handlePreRunProfiling(ctx, cfg, promClient, store, w, event)
		}
	}
}

// ---------------------------------------------------------------------------
// Training pod completion handler (existing feature)
// ---------------------------------------------------------------------------

func handlePodCompletion(ctx context.Context, cfg *config.Config, promClient *prometheus.Client, store *db.Store, event watcher.PodCompletionEvent) {
	log.Printf("=== Training pod completed: %s/%s (phase=%s, duration=%.0fs) ===",
		event.PodNamespace, event.PodName, event.Phase, event.Duration.Seconds())

	queryCtx, queryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer queryCancel()

	results, err := promClient.QueryPeakVRAM(queryCtx, cfg.VRAMMetric, event.PodName, event.PodNamespace, event.Duration)
	if err != nil {
		log.Printf("ERROR: querying peak VRAM for %s/%s: %v", event.PodNamespace, event.PodName, err)
		return
	}

	for _, r := range results {
		log.Printf("  Peak VRAM: %.2f MiB (container=%s, device=%s)", r.PeakValueMiB, r.ContainerName, r.DeviceUUID)
	}

	if err := store.InsertPeakUsage(results, event.StartTime, event.EndTime, event.Duration.Seconds(), string(event.Phase)); err != nil {
		log.Printf("ERROR: inserting peak VRAM for %s/%s: %v", event.PodNamespace, event.PodName, err)
		return
	}

	log.Printf("Saved %d peak VRAM record(s) for %s/%s. Back to idle.", len(results), event.PodNamespace, event.PodName)
}

// ---------------------------------------------------------------------------
// Pre-run profiling handler (new feature)
// ---------------------------------------------------------------------------

func handlePreRunProfiling(ctx context.Context, cfg *config.Config, promClient *prometheus.Client, store *db.Store, w *watcher.Watcher, event watcher.PreRunEvent) {
	podKey := fmt.Sprintf("%s/%s", event.PodNamespace, event.PodName)
	dur := cfg.ProfilingDuration

	// Compute how long to wait: account for time already elapsed since the
	// pod started (handles both normal events and orphan-sweep resumes).
	elapsed := time.Since(event.StartTime)
	remaining := dur - elapsed
	if remaining < 0 {
		remaining = 0
	}

	log.Printf("=== Pre-run profiling for %s — elapsed %s, waiting %s more ===",
		podKey, elapsed.Round(time.Second), remaining.Round(time.Second))

	// Step 1: Wait for the remaining profiling duration
	if remaining > 0 {
		select {
		case <-time.After(remaining):
			// continue
		case <-ctx.Done():
			log.Printf("Pre-run profiling for %s cancelled (shutdown)", podKey)
			return
		}
	}

	// Step 2: Query Prometheus for peak VRAM over the full profiling window
	queryCtx, queryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer queryCancel()

	results, err := promClient.QueryPeakVRAM(queryCtx, cfg.VRAMMetric, event.PodName, event.PodNamespace, dur)
	if err != nil {
		log.Printf("ERROR: pre-run query for %s: %v", podKey, err)
		// Fall through to delete — don't leave the pod running
	}

	for _, r := range results {
		log.Printf("  Pre-run peak VRAM: %.2f MiB (container=%s, device=%s)", r.PeakValueMiB, r.ContainerName, r.DeviceUUID)
	}

	// Step 3: Save to the pre-run profiling table (only if we got results)
	if len(results) > 0 {
		endTime := time.Now()
		if err := store.InsertPreRunProfile(results, event.StartTime, endTime, dur.Seconds()); err != nil {
			log.Printf("ERROR: inserting pre-run profile for %s: %v", podKey, err)
		} else {
			log.Printf("Saved %d pre-run profile record(s) for %s", len(results), podKey)
		}
	}

	// Step 4: Always delete the pod — don't leave it running and wasting GPU
	log.Printf("Deleting pod %s after pre-run profiling", podKey)
	err = w.Clientset().CoreV1().Pods(event.PodNamespace).Delete(ctx, event.PodName, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("ERROR: deleting pod %s: %v", podKey, err)
		return
	}

	log.Printf("Pod %s deleted. Pre-run profiling complete. Back to idle.", podKey)
}

// ---------------------------------------------------------------------------
// Startup orphan sweep
// ---------------------------------------------------------------------------

// sweepOrphanPreRunPods lists all pods cluster-wide with the profiling label.
// Any pod that has been Running longer than the profiling duration is an orphan
// left over from a previous crash — it is deleted immediately.
// Pods still within the profiling window get a resumed goroutine with the
// remaining time.
func sweepOrphanPreRunPods(
	ctx context.Context,
	cfg *config.Config,
	promClient *prometheus.Client,
	store *db.Store,
	w *watcher.Watcher,
	labelKey, labelVal string,
) {
	selector := fmt.Sprintf("%s=%s", labelKey, labelVal)
	log.Printf("Orphan sweep: listing pods with %s ...", selector)

	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	podList, err := w.Clientset().CoreV1().Pods("").List(listCtx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		log.Printf("WARNING: orphan sweep failed to list pods: %v", err)
		return
	}

	if len(podList.Items) == 0 {
		log.Println("Orphan sweep: no pre-run pods found. Clean start.")
		return
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

		// Only care about Running pods
		if pod.Status.Phase != "Running" {
			log.Printf("Orphan sweep: skipping %s (phase=%s)", key, pod.Status.Phase)
			continue
		}

		// Mark by UID so the informer won't emit a duplicate event.
		// Safe to always mark: UID is unique per pod instance, so a
		// new pod with the same name will have a different UID.
		w.MarkPreRunSeen(pod.UID)

		// Determine how long the pod has been running
		var startTime time.Time
		if pod.Status.StartTime != nil {
			startTime = pod.Status.StartTime.Time
		} else {
			startTime = pod.CreationTimestamp.Time
		}
		uptime := time.Since(startTime)

		if uptime >= cfg.ProfilingDuration {
			// Orphan: past the profiling window — delete immediately.
			log.Printf("Orphan sweep: %s uptime %s (>= %s) — deleting orphan",
				key, uptime.Round(time.Second), cfg.ProfilingDuration)

			if err := w.Clientset().CoreV1().Pods(pod.Namespace).Delete(listCtx, pod.Name, metav1.DeleteOptions{}); err != nil {
				log.Printf("WARNING: orphan sweep failed to delete %s: %v", key, err)
			} else {
				log.Printf("Orphan sweep: deleted %s", key)
			}
		} else {
			// Still within the profiling window — resume with a goroutine.
			remaining := cfg.ProfilingDuration - uptime
			log.Printf("Orphan sweep: %s uptime %s — resuming timer (%s remaining)",
				key, uptime.Round(time.Second), remaining.Round(time.Second))

			// Create a synthetic PreRunEvent and let the handler manage it.
			// The handler computes remaining time from event.StartTime.
			evt := watcher.PreRunEvent{
				PodName:      pod.Name,
				PodNamespace: pod.Namespace,
				StartTime:    startTime,
			}
			go handlePreRunProfiling(ctx, cfg, promClient, store, w, evt)
		}
	}

	log.Println("Orphan sweep: complete.")
}

// ---------------------------------------------------------------------------
// HTTP health server for K8s liveness / readiness probes
// ---------------------------------------------------------------------------

func startHealthServer(addr string, store *db.Store) {
	mux := http.NewServeMux()

	// Liveness: process is running
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Readiness: DB connection is alive
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(); err != nil {
			http.Error(w, "db ping failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Printf("Health server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("Health server error: %v", err)
	}
}

package watcher

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// ---------------------------------------------------------------------------
// Event types
// ---------------------------------------------------------------------------

// PodCompletionEvent is emitted when a training pod reaches Succeeded or Failed.
type PodCompletionEvent struct {
	PodName      string
	PodNamespace string
	Phase        corev1.PodPhase
	StartTime    time.Time
	EndTime      time.Time
	Duration     time.Duration
}

// PreRunEvent is emitted when a vram-profiling pod transitions to Running.
type PreRunEvent struct {
	PodName      string
	PodNamespace string
	StartTime    time.Time
}

// ---------------------------------------------------------------------------
// Watcher
// ---------------------------------------------------------------------------

// Watcher uses SharedInformers to watch for two kinds of pod events:
//  1. Training pods (job-type=training) that complete → PodCompletionEvent
//  2. Pre-run profiling pods (vram-profiling=true) that start → PreRunEvent
type Watcher struct {
	clientset kubernetes.Interface

	// Training completion watcher
	trainLabelKey   string
	trainLabelValue string
	completionCh    chan PodCompletionEvent
	completionSeen  sync.Map // key: pod UID (types.UID) → true

	// Pre-run profiling watcher
	profileLabelKey   string
	profileLabelValue string
	preRunCh          chan PreRunEvent
	preRunSeen        sync.Map // key: pod UID (types.UID) → true
}

// New creates a Watcher.
func New(trainLabelKey, trainLabelValue, profileLabelKey, profileLabelValue string) (*Watcher, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("getting in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	return &Watcher{
		clientset:         clientset,
		trainLabelKey:     trainLabelKey,
		trainLabelValue:   trainLabelValue,
		completionCh:      make(chan PodCompletionEvent, 100),
		profileLabelKey:   profileLabelKey,
		profileLabelValue: profileLabelValue,
		preRunCh:          make(chan PreRunEvent, 100),
	}, nil
}

// Clientset returns the underlying Kubernetes clientset so callers can
// perform operations like deleting pods.
func (w *Watcher) Clientset() kubernetes.Interface {
	return w.clientset
}

// CompletionEvents returns the channel for training pod completion events.
func (w *Watcher) CompletionEvents() <-chan PodCompletionEvent {
	return w.completionCh
}

// PreRunEvents returns the channel for pre-run profiling events.
func (w *Watcher) PreRunEvents() <-chan PreRunEvent {
	return w.preRunCh
}

// MarkPreRunSeen registers a pod UID so the informer won't emit a duplicate
// PreRunEvent for it. Used by the startup orphan sweep.
func (w *Watcher) MarkPreRunSeen(uid types.UID) {
	w.preRunSeen.Store(uid, true)
}

// Run starts both SharedInformers and blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	// --- Informer 1: training pods (completion) ---
	trainSelector := fmt.Sprintf("%s=%s", w.trainLabelKey, w.trainLabelValue)
	log.Printf("Starting training-pod watcher: %s", trainSelector)

	trainFactory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset, 30*time.Second,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = trainSelector
		}),
	)
	trainInformer := trainFactory.Core().V1().Pods().Informer()
	trainInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.checkCompletion(pod)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, ok1 := oldObj.(*corev1.Pod)
			newPod, ok2 := newObj.(*corev1.Pod)
			if ok1 && ok2 && oldPod.Status.Phase != newPod.Status.Phase {
				w.checkCompletion(newPod)
			}
		},
	})

	// --- Informer 2: pre-run profiling pods ---
	profileSelector := fmt.Sprintf("%s=%s", w.profileLabelKey, w.profileLabelValue)
	log.Printf("Starting pre-run profiling watcher: %s", profileSelector)

	profileFactory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset, 30*time.Second,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = profileSelector
		}),
	)
	profileInformer := profileFactory.Core().V1().Pods().Informer()
	profileInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.checkRunning(pod)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, ok1 := oldObj.(*corev1.Pod)
			newPod, ok2 := newObj.(*corev1.Pod)
			if ok1 && ok2 && oldPod.Status.Phase != newPod.Status.Phase {
				w.checkRunning(newPod)
			}
		},
	})

	// Start both factories
	trainFactory.Start(ctx.Done())
	profileFactory.Start(ctx.Done())

	trainFactory.WaitForCacheSync(ctx.Done())
	profileFactory.WaitForCacheSync(ctx.Done())

	log.Println("All pod watcher informers synced and running")
	<-ctx.Done()
	log.Println("Pod watchers stopped")
}

// ---------------------------------------------------------------------------
// Training pod completion logic
// ---------------------------------------------------------------------------

func (w *Watcher) checkCompletion(pod *corev1.Pod) {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return
	}

	if _, loaded := w.completionSeen.LoadOrStore(pod.UID, true); loaded {
		return // already processed
	}

	podKey := pod.Namespace + "/" + pod.Name
	startTime, endTime, duration := computePodDuration(pod)
	log.Printf("Training pod completed: %s [uid=%s] (phase=%s, duration=%.0fs)", podKey, pod.UID, pod.Status.Phase, duration.Seconds())

	w.completionCh <- PodCompletionEvent{
		PodName:      pod.Name,
		PodNamespace: pod.Namespace,
		Phase:        pod.Status.Phase,
		StartTime:    startTime,
		EndTime:      endTime,
		Duration:     duration,
	}
}

// ---------------------------------------------------------------------------
// Pre-run profiling logic
// ---------------------------------------------------------------------------

func (w *Watcher) checkRunning(pod *corev1.Pod) {
	if pod.Status.Phase != corev1.PodRunning {
		return
	}

	if _, loaded := w.preRunSeen.LoadOrStore(pod.UID, true); loaded {
		return // already assigned a timer
	}

	podKey := pod.Namespace + "/" + pod.Name
	startTime := time.Now()
	if pod.Status.StartTime != nil {
		startTime = pod.Status.StartTime.Time
	}

	log.Printf("Pre-run pod detected: %s [uid=%s] (Running)", podKey, pod.UID)

	w.preRunCh <- PreRunEvent{
		PodName:      pod.Name,
		PodNamespace: pod.Namespace,
		StartTime:    startTime,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// computePodDuration calculates the pod's lifespan.
func computePodDuration(pod *corev1.Pod) (start, end time.Time, dur time.Duration) {
	if pod.Status.StartTime != nil {
		start = pod.Status.StartTime.Time
	} else {
		start = pod.CreationTimestamp.Time
	}

	end = time.Time{}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.FinishedAt.Time.After(end) {
			end = cs.State.Terminated.FinishedAt.Time
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.FinishedAt.Time.After(end) {
			end = cs.State.Terminated.FinishedAt.Time
		}
	}

	if end.IsZero() {
		end = time.Now()
	}

	dur = end.Sub(start)
	if dur < time.Second {
		dur = time.Second
	}

	return start, end, dur
}

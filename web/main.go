package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	recommenderURL  string
	podTemplatePath string
	k8s             *k8sClient
	db              *sql.DB
}

// ---------------------------------------------------------------------------
// K8s REST client (zero external dependencies)
// ---------------------------------------------------------------------------

type k8sClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newK8sClient() (*k8sClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST/PORT not set")
	}

	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, fmt.Errorf("reading SA token: %w", err)
	}

	caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)

	return &k8sClient{
		baseURL: fmt.Sprintf("https://%s:%s", host, port),
		token:   strings.TrimSpace(string(token)),
		httpClient: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
	}, nil
}

func (c *k8sClient) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	port := getEnv("PORT", "8090")
	recommenderURL := getEnv("RECOMMENDER_URL", "http://recommender.profiler.svc.cluster.local/recommend")
	podTemplatePath := getEnv("POD_TEMPLATE_PATH", "/etc/pod-template/pod.json")

	dbHost := getEnv("DB_HOST", "profiler-postgresql.profiler.svc.cluster.local")
	dbPort := getEnv("DB_PORT", "5432")
	dbUser := getEnv("DB_USER", "profiler")
	dbPass := getEnv("DB_PASSWORD", "profiler")
	dbName := getEnv("DB_NAME", "profiler")
	dbSSL := getEnv("DB_SSLMODE", "disable")

	log.Printf("Web UI starting on :%s", port)
	log.Printf("Recommender API   : %s", recommenderURL)
	log.Printf("Pod template      : %s", podTemplatePath)
	log.Printf("DB host           : %s:%s/%s", dbHost, dbPort, dbName)

	s := &server{
		recommenderURL:  recommenderURL,
		podTemplatePath: podTemplatePath,
	}

	// PostgreSQL (read-only, for /db page)
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s", dbHost, dbPort, dbUser, dbPass, dbName, dbSSL)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Printf("WARNING: DB unavailable, /db page disabled: %v", err)
	} else if err := db.Ping(); err != nil {
		log.Printf("WARNING: DB ping failed, /db page disabled: %v", err)
		db.Close()
		db = nil
	} else {
		db.SetMaxOpenConns(3)
		s.db = db
		log.Println("PostgreSQL connected (read-only for /db)")
	}

	// In-cluster K8s client (optional — profiling disabled if unavailable)
	k8s, err := newK8sClient()
	if err != nil {
		log.Printf("WARNING: K8s client unavailable, Profile button disabled: %v", err)
	} else {
		s.k8s = k8s
		log.Println("K8s in-cluster client ready")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/recommend", s.handleRecommend)
	mux.HandleFunc("/api/profile", s.handleProfile)
	mux.HandleFunc("/api/db", s.handleDBAPI)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	mux.HandleFunc("/db", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, dbPageHTML)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 180 * time.Second, // long for streaming profile
	}

	log.Fatal(srv.ListenAndServe())
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *server) handleRecommend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(s.recommenderURL, "application/json", r.Body)
	if err != nil {
		log.Printf("ERROR proxying to recommender: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "Failed to reach recommender service",
			"details": err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleProfile streams NDJSON progress while running a full profiling cycle:
// create pod → wait for profiler to capture & delete → query recommendation.
func (s *server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	send := func(data map[string]interface{}) {
		b, _ := json.Marshal(data)
		w.Write(b)
		w.Write([]byte("\n"))
		flusher.Flush()
	}
	step := func(msg string) {
		log.Printf("Profile step: %s", msg)
		send(map[string]interface{}{"type": "step", "message": msg})
	}
	sendErr := func(errMsg, details string) {
		log.Printf("Profile error: %s — %s", errMsg, details)
		send(map[string]interface{}{"type": "error", "error": errMsg, "details": details})
	}

	if s.k8s == nil {
		sendErr("K8s client not available", "Web UI is not running inside a K8s cluster")
		return
	}

	// ---- 1. Read pod template ----
	step("Reading pod template...")
	podJSON, err := os.ReadFile(s.podTemplatePath)
	if err != nil {
		sendErr("Failed to read pod template", err.Error())
		return
	}

	var podMeta struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(podJSON, &podMeta); err != nil {
		sendErr("Failed to parse pod template JSON", err.Error())
		return
	}

	podName := podMeta.Metadata.Name
	podNS := podMeta.Metadata.Namespace
	podImage := ""
	if len(podMeta.Spec.Containers) > 0 {
		podImage = podMeta.Spec.Containers[0].Image
	}
	podPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", podNS, podName)

	step(fmt.Sprintf("Pod: %s/%s  Image: %s", podNS, podName, podImage))

	// ---- 2. Delete existing pod (ignore 404) ----
	step("Cleaning up any existing pod...")
	if resp, err := s.k8s.do("DELETE", podPath, nil); err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 202 {
			for i := 0; i < 30; i++ {
				time.Sleep(2 * time.Second)
				chk, err := s.k8s.do("GET", podPath, nil)
				if err != nil {
					break
				}
				chk.Body.Close()
				if chk.StatusCode == 404 {
					break
				}
			}
		}
	}

	// ---- 3. Create pod ----
	step(fmt.Sprintf("Creating profiling pod %s/%s...", podNS, podName))
	resp, err := s.k8s.do("POST", fmt.Sprintf("/api/v1/namespaces/%s/pods", podNS), bytes.NewReader(podJSON))
	if err != nil {
		sendErr("Failed to create pod", err.Error())
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		sendErr(fmt.Sprintf("K8s API returned %d creating pod", resp.StatusCode), string(respBody))
		return
	}

	// ---- 4. Wait for Running ----
	step("Waiting for pod to start running...")
	running := false
	for i := 0; i < 60; i++ {
		time.Sleep(2 * time.Second)
		chk, err := s.k8s.do("GET", podPath, nil)
		if err != nil {
			running = true
			break
		}
		bdy, _ := io.ReadAll(chk.Body)
		chk.Body.Close()
		if chk.StatusCode == 404 {
			running = true
			break
		}
		var ps struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		json.Unmarshal(bdy, &ps)
		if ps.Status.Phase == "Running" {
			running = true
			step("Pod is Running — profiler capturing VRAM for ~45 seconds...")
			break
		}
		if ps.Status.Phase == "Failed" || ps.Status.Phase == "Unknown" {
			sendErr("Pod failed to start", fmt.Sprintf("Phase: %s", ps.Status.Phase))
			return
		}
	}
	if !running {
		sendErr("Timeout", "Pod did not reach Running within 120 seconds")
		return
	}

	// ---- 5. Wait for profiler to delete the pod ----
	deleted := false
	for i := 0; i < 50; i++ {
		time.Sleep(2 * time.Second)
		chk, err := s.k8s.do("GET", podPath, nil)
		if err != nil {
			deleted = true
			break
		}
		chk.Body.Close()
		if chk.StatusCode == 404 {
			deleted = true
			break
		}
	}
	if !deleted {
		sendErr("Profiling timed out", "Pod was not deleted by profiler within expected time")
		return
	}

	step("Profiling complete! Fetching recommendation...")
	time.Sleep(3 * time.Second)

	// ---- 6. Query recommender ----
	if podImage == "" {
		sendErr("Cannot query recommender", "No container image found in pod template")
		return
	}
	reqBody, _ := json.Marshal(map[string]string{"image_url": podImage})
	client := &http.Client{Timeout: 30 * time.Second}
	recResp, err := client.Post(s.recommenderURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		sendErr("Recommender unreachable", err.Error())
		return
	}
	defer recResp.Body.Close()
	recBody, _ := io.ReadAll(recResp.Body)

	var result interface{}
	json.Unmarshal(recBody, &result)
	send(map[string]interface{}{"type": "result", "data": result})
	log.Printf("Profile: completed for %s/%s", podNS, podName)
}

// ---------------------------------------------------------------------------

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ---------------------------------------------------------------------------
// DB API handler
// ---------------------------------------------------------------------------

type dbRecord struct {
	ID              int64   `json:"id"`
	PodName         string  `json:"pod_name"`
	PodNamespace    string  `json:"pod_namespace"`
	PodUID          string  `json:"pod_uid"`
	ContainerName   string  `json:"container_name"`
	DeviceUUID      string  `json:"device_uuid"`
	DeviceType      string  `json:"device_type"`
	VDeviceID       string  `json:"vdevice_id"`
	Image           string  `json:"image"`
	ImageID         string  `json:"image_id"`
	MetricName      string  `json:"metric_name"`
	PeakValueMiB    float64 `json:"peak_value_mib"`
	StartTime       string  `json:"start_time"`
	EndTime         string  `json:"end_time"`
	DurationSeconds float64 `json:"duration_seconds"`
	CreatedAt       string  `json:"created_at"`
	Source          string  `json:"source"` // "peak_usage" or "prerun_profile"
}

func (s *server) handleDBAPI(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "Database not connected"})
		return
	}

	var records []dbRecord

	// Query peak usage
	rows1, err := s.db.Query(`SELECT id, pod_name, pod_namespace, pod_uid, container_name,
		device_uuid, device_type, vdevice_id, image, image_id, metric_name,
		peak_value_mib, pod_start_time, pod_end_time, duration_seconds, created_at
		FROM vgpu_peak_usage ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		log.Printf("ERROR querying vgpu_peak_usage: %v", err)
	} else {
		defer rows1.Close()
		for rows1.Next() {
			var rec dbRecord
			var startT, endT, createdT time.Time
			if err := rows1.Scan(&rec.ID, &rec.PodName, &rec.PodNamespace, &rec.PodUID,
				&rec.ContainerName, &rec.DeviceUUID, &rec.DeviceType, &rec.VDeviceID,
				&rec.Image, &rec.ImageID, &rec.MetricName, &rec.PeakValueMiB,
				&startT, &endT, &rec.DurationSeconds, &createdT); err != nil {
				log.Printf("ERROR scanning peak_usage row: %v", err)
				continue
			}
			rec.StartTime = startT.Format(time.RFC3339)
			rec.EndTime = endT.Format(time.RFC3339)
			rec.CreatedAt = createdT.Format(time.RFC3339)
			rec.Source = "peak_usage"
			records = append(records, rec)
		}
	}

	// Query prerun profiles
	rows2, err := s.db.Query(`SELECT id, pod_name, pod_namespace, pod_uid, container_name,
		device_uuid, device_type, vdevice_id, image, image_id, metric_name,
		peak_value_mib, profile_start, profile_end, duration_seconds, created_at
		FROM vgpu_prerun_profile ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		log.Printf("ERROR querying vgpu_prerun_profile: %v", err)
	} else {
		defer rows2.Close()
		for rows2.Next() {
			var rec dbRecord
			var startT, endT, createdT time.Time
			if err := rows2.Scan(&rec.ID, &rec.PodName, &rec.PodNamespace, &rec.PodUID,
				&rec.ContainerName, &rec.DeviceUUID, &rec.DeviceType, &rec.VDeviceID,
				&rec.Image, &rec.ImageID, &rec.MetricName, &rec.PeakValueMiB,
				&startT, &endT, &rec.DurationSeconds, &createdT); err != nil {
				log.Printf("ERROR scanning prerun_profile row: %v", err)
				continue
			}
			rec.StartTime = startT.Format(time.RFC3339)
			rec.EndTime = endT.Format(time.RFC3339)
			rec.CreatedAt = createdT.Format(time.RFC3339)
			rec.Source = "prerun_profile"
			records = append(records, rec)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"records": records,
		"count":   len(records),
	})
}

var indexHTML = strings.TrimSpace(`
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>vGPU VRAM Recommendation System</title>
<style>
  :root {
    --bg: #0f172a;
    --card: #1e293b;
    --border: #334155;
    --primary: #3b82f6;
    --primary-hover: #2563eb;
    --accent: #8b5cf6;
    --accent-hover: #7c3aed;
    --success: #22c55e;
    --warning: #f59e0b;
    --danger: #ef4444;
    --text: #f1f5f9;
    --muted: #94a3b8;
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
    background: var(--bg);
    color: var(--text);
    min-height: 100vh;
    display: flex;
    flex-direction: column;
    align-items: center;
  }
  .header {
    width: 100%;
    padding: 24px;
    text-align: center;
    border-bottom: 1px solid var(--border);
    background: var(--card);
  }
  .header h1 {
    font-size: 1.8rem;
    font-weight: 700;
    background: linear-gradient(135deg, #3b82f6, #8b5cf6);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
  }
  .header p { color: var(--muted); margin-top: 8px; font-size: 0.95rem; }
  .container {
    width: 100%;
    max-width: 800px;
    padding: 32px 16px;
  }
  .card {
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 24px;
    margin-bottom: 24px;
  }
  .card h2 {
    font-size: 1.1rem;
    margin-bottom: 16px;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .form-group { margin-bottom: 16px; }
  .form-group label {
    display: block;
    font-size: 0.85rem;
    color: var(--muted);
    margin-bottom: 6px;
  }
  .input-row {
    display: flex;
    gap: 12px;
  }
  input[type="text"] {
    flex: 1;
    padding: 12px 16px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    color: var(--text);
    font-size: 0.95rem;
    outline: none;
    transition: border-color 0.2s;
  }
  input[type="text"]:focus { border-color: var(--primary); }
  input[type="text"]::placeholder { color: #475569; }
  button {
    padding: 12px 24px;
    border: none;
    border-radius: 8px;
    color: white;
    font-size: 0.95rem;
    font-weight: 600;
    cursor: pointer;
    transition: background 0.2s;
    white-space: nowrap;
  }
  .btn-recommend { background: var(--primary); }
  .btn-recommend:hover { background: var(--primary-hover); }
  .btn-profile { background: var(--accent); }
  .btn-profile:hover { background: var(--accent-hover); }
  button:disabled { opacity: 0.5; cursor: not-allowed; }
  .spinner {
    display: inline-block;
    width: 16px;
    height: 16px;
    border: 2px solid transparent;
    border-top-color: white;
    border-radius: 50%;
    animation: spin 0.6s linear infinite;
    vertical-align: middle;
    margin-right: 6px;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  .btn-row {
    display: flex;
    gap: 8px;
    margin-top: 12px;
  }
  .btn-row p {
    font-size: 0.8rem;
    color: var(--muted);
    align-self: center;
  }

  /* Progress log */
  #progress { display: none; }
  .step-item {
    padding: 8px 12px;
    margin-bottom: 4px;
    border-left: 3px solid var(--accent);
    background: rgba(139,92,246,0.08);
    border-radius: 0 6px 6px 0;
    font-size: 0.85rem;
    font-family: 'Courier New', monospace;
    animation: fadeIn 0.3s ease;
  }
  @keyframes fadeIn { from { opacity: 0; transform: translateX(-8px); } to { opacity: 1; transform: translateX(0); } }

  /* Result */
  #result { display: none; }
  .result-header {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 16px;
  }
  .badge {
    display: inline-block;
    padding: 4px 12px;
    border-radius: 20px;
    font-size: 0.8rem;
    font-weight: 600;
    text-transform: uppercase;
  }
  .badge-ok { background: #166534; color: #bbf7d0; }
  .badge-profiling { background: #92400e; color: #fde68a; }
  .badge-error { background: #7f1d1d; color: #fecaca; }

  .stats-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
    gap: 12px;
    margin-top: 16px;
  }
  .stat-card {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 16px;
    text-align: center;
  }
  .stat-card .value {
    font-size: 1.6rem;
    font-weight: 700;
    color: var(--primary);
  }
  .stat-card .label {
    font-size: 0.75rem;
    color: var(--muted);
    margin-top: 4px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .stat-card.highlight .value { color: var(--success); }

  .info-row {
    display: flex;
    justify-content: space-between;
    padding: 10px 0;
    border-bottom: 1px solid var(--border);
    font-size: 0.9rem;
  }
  .info-row:last-child { border-bottom: none; }
  .info-row .key { color: var(--muted); }
  .info-row .val {
    color: var(--text);
    font-family: 'Courier New', monospace;
    word-break: break-all;
    text-align: right;
    max-width: 60%;
  }

  .message-box {
    padding: 12px 16px;
    border-radius: 8px;
    font-size: 0.9rem;
    margin-top: 12px;
  }
  .message-box.info { background: #1e3a5f; border: 1px solid #2563eb; }
  .message-box.warn { background: #422006; border: 1px solid #d97706; }
  .message-box.err  { background: #450a0a; border: 1px solid #dc2626; }

  .examples { margin-top: 12px; }
  .examples span {
    display: inline-block;
    padding: 4px 10px;
    margin: 4px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    font-size: 0.8rem;
    font-family: monospace;
    cursor: pointer;
    transition: border-color 0.2s;
  }
  .examples span:hover { border-color: var(--primary); }

  .footer {
    margin-top: auto;
    padding: 24px;
    text-align: center;
    color: var(--muted);
    font-size: 0.8rem;
    border-top: 1px solid var(--border);
    width: 100%;
  }
</style>
</head>
<body>

<div class="header">
  <h1>&#x1F680; vGPU VRAM Recommendation System</h1>
  <p>Get VRAM allocation recommendations based on historical profiling data</p>
</div>

<div class="container">
  <!-- Input card -->
  <div class="card">
    <h2>&#x1F50D; Query &amp; Profile</h2>
    <div class="form-group">
      <label>Container Image URL</label>
      <div class="input-row">
        <input type="text" id="imageInput" placeholder="e.g. docker.io/library/nginx:latest" />
      </div>
    </div>
    <div class="btn-row">
      <button class="btn-recommend" id="submitBtn" onclick="doRecommend()">&#x1F4CA; Recommend</button>
      <button class="btn-profile" id="profileBtn" onclick="doProfile()">&#x26A1; Profile</button>
      <p>Recommend = query existing data &nbsp;|&nbsp; Profile = run GPU pod, capture VRAM, get recommendation</p>
    </div>
    <div class="examples">
      <label style="font-size:0.8rem;color:var(--muted);">Try examples:</label>
      <span onclick="setImage('deepspeed/deepspeed:latest')">deepspeed/deepspeed:latest</span>
      <span onclick="setImage('pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime')">pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime</span>
      <span onclick="setImage('nvcr.io/nvidia/tensorflow:24.01-tf2-py3')">nvcr.io/nvidia/tensorflow:24.01-tf2-py3</span>
    </div>
  </div>

  <!-- Progress card (profiling only) -->
  <div class="card" id="progress">
    <h2>&#x23F3; Profiling Progress</h2>
    <div id="progressLog"></div>
  </div>

  <!-- Result card -->
  <div class="card" id="result">
    <div class="result-header">
      <h2>&#x1F4CA; Result</h2>
      <span class="badge" id="statusBadge"></span>
    </div>
    <div id="resultContent"></div>
  </div>
</div>

<div class="footer">
  vGPU VRAM Recommendation System &mdash; Profiler + Recommender on Kubernetes
  &nbsp;|&nbsp; <a href="/db" style="color:var(--primary)">View Database</a>
</div>

<script>
function setImage(img) {
  document.getElementById('imageInput').value = img;
}

function setButtons(disabled) {
  document.getElementById('submitBtn').disabled = disabled;
  document.getElementById('profileBtn').disabled = disabled;
}

// ---- Recommend (existing data) ----
async function doRecommend() {
  const input = document.getElementById('imageInput').value.trim();
  if (!input) { alert('Please enter an image URL'); return; }

  setButtons(true);
  const btn = document.getElementById('submitBtn');
  btn.innerHTML = '<span class="spinner"></span>Querying...';

  document.getElementById('progress').style.display = 'none';
  const resultDiv = document.getElementById('result');
  resultDiv.style.display = 'block';
  document.getElementById('resultContent').innerHTML = '<p style="color:var(--muted)">Loading...</p>';

  try {
    const resp = await fetch('/api/recommend', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ image_url: input })
    });
    const data = await resp.json();
    if (data.error) { showError(data); return; }
    if (data.status === 'ok') { showOK(data); }
    else if (data.status === 'profiling_required') { showProfilingRequired(data); }
    else { showError({ error: 'Unexpected response', details: JSON.stringify(data) }); }
  } catch (err) {
    showError({ error: 'Network error', details: err.message });
  } finally {
    setButtons(false);
    btn.innerHTML = '&#x1F4CA; Recommend';
  }
}

// ---- Profile (run pod, capture, recommend) ----
async function doProfile() {
  setButtons(true);
  const btn = document.getElementById('profileBtn');
  btn.innerHTML = '<span class="spinner"></span>Profiling...';

  const progressDiv = document.getElementById('progress');
  const progressLog = document.getElementById('progressLog');
  progressDiv.style.display = 'block';
  progressLog.innerHTML = '';
  document.getElementById('result').style.display = 'none';

  function addStep(msg) {
    const el = document.createElement('div');
    el.className = 'step-item';
    el.textContent = msg;
    progressLog.appendChild(el);
    el.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  }

  try {
    const resp = await fetch('/api/profile', { method: 'POST' });
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop();
      for (const line of lines) {
        if (!line.trim()) continue;
        try {
          const event = JSON.parse(line);
          if (event.type === 'step') {
            addStep(event.message);
          } else if (event.type === 'result') {
            addStep('Done!');
            const data = event.data;
            document.getElementById('result').style.display = 'block';
            if (data && data.status === 'ok') { showOK(data); }
            else if (data && data.status === 'profiling_required') { showProfilingRequired(data); }
            else if (data && data.error) { showError(data); }
            else { showError({ error: 'Unexpected result', details: JSON.stringify(data) }); }
          } else if (event.type === 'error') {
            addStep('ERROR: ' + event.error);
            document.getElementById('result').style.display = 'block';
            showError(event);
          }
        } catch(e) { /* ignore parse errors */ }
      }
    }
  } catch (err) {
    document.getElementById('result').style.display = 'block';
    showError({ error: 'Network error', details: err.message });
  } finally {
    setButtons(false);
    btn.innerHTML = '&#x26A1; Profile';
  }
}

// ---- Display helpers ----
function showOK(data) {
  const badge = document.getElementById('statusBadge');
  badge.className = 'badge badge-ok';
  badge.textContent = 'Recommendation Available';
  const rec = data.recommendation;
  document.getElementById('resultContent').innerHTML = ` + "`" + `
    <div class="stats-grid">
      <div class="stat-card highlight">
        <div class="value">${rec.recommended_vram_mib}</div>
        <div class="label">Recommended (MiB)</div>
      </div>
      <div class="stat-card">
        <div class="value">${rec.peak_vram_mib}</div>
        <div class="label">Peak Observed (MiB)</div>
      </div>
      <div class="stat-card">
        <div class="value">${rec.safety_buffer_percent}%</div>
        <div class="label">Safety Buffer</div>
      </div>
      <div class="stat-card">
        <div class="value">${rec.record_count}</div>
        <div class="label">Historical Records</div>
      </div>
    </div>
    <div style="margin-top:16px">
      <div class="info-row"><span class="key">Image URL</span><span class="val">${data.image_url}</span></div>
      <div class="info-row"><span class="key">Image Digest</span><span class="val">${data.image_digest}</span></div>
      <div class="info-row"><span class="key">Data Source</span><span class="val">${rec.source}</span></div>
    </div>
    <div class="message-box info">${data.message}</div>
  ` + "`" + `;
}

function showProfilingRequired(data) {
  const badge = document.getElementById('statusBadge');
  badge.className = 'badge badge-profiling';
  badge.textContent = 'Profiling Required';
  document.getElementById('resultContent').innerHTML = ` + "`" + `
    <div style="margin-top:8px">
      <div class="info-row"><span class="key">Image URL</span><span class="val">${data.image_url}</span></div>
      <div class="info-row"><span class="key">Image Digest</span><span class="val">${data.image_digest}</span></div>
    </div>
    <div class="message-box warn">
      <strong>&#x26A0; No Historical Data</strong><br>
      ${data.message}<br><br>
      Click the <strong>&#x26A1; Profile</strong> button to automatically run a profiling pod and capture VRAM usage.
    </div>
  ` + "`" + `;
}

function showError(data) {
  const badge = document.getElementById('statusBadge');
  badge.className = 'badge badge-error';
  badge.textContent = 'Error';
  document.getElementById('resultContent').innerHTML = ` + "`" + `
    <div class="message-box err">
      <strong>&#x274C; ${data.error}</strong>
      ${data.details ? '<br><small>' + data.details + '</small>' : ''}
    </div>
  ` + "`" + `;
}

document.getElementById('imageInput').addEventListener('keydown', function(e) {
  if (e.key === 'Enter') doRecommend();
});
</script>
</body>
</html>
`)

var dbPageHTML = strings.TrimSpace(`
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Database Viewer — vGPU VRAM</title>
<style>
  :root {
    --bg: #0f172a;
    --card: #1e293b;
    --border: #334155;
    --primary: #3b82f6;
    --success: #22c55e;
    --warning: #f59e0b;
    --text: #f1f5f9;
    --muted: #94a3b8;
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
    background: var(--bg);
    color: var(--text);
    min-height: 100vh;
  }
  .header {
    padding: 20px 24px;
    border-bottom: 1px solid var(--border);
    background: var(--card);
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .header h1 {
    font-size: 1.4rem;
    font-weight: 700;
    background: linear-gradient(135deg, #3b82f6, #8b5cf6);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
  }
  .header a {
    color: var(--primary);
    text-decoration: none;
    font-size: 0.9rem;
  }
  .header a:hover { text-decoration: underline; }

  .toolbar {
    padding: 16px 24px;
    display: flex;
    gap: 12px;
    align-items: center;
    flex-wrap: wrap;
  }
  .toolbar .count {
    color: var(--muted);
    font-size: 0.85rem;
    margin-left: auto;
  }
  .toolbar button {
    padding: 8px 16px;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--card);
    color: var(--text);
    font-size: 0.85rem;
    cursor: pointer;
    transition: border-color 0.2s;
  }
  .toolbar button:hover { border-color: var(--primary); }
  .toolbar button.active { border-color: var(--primary); background: rgba(59,130,246,0.15); }
  .toolbar input {
    padding: 8px 12px;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--bg);
    color: var(--text);
    font-size: 0.85rem;
    outline: none;
    width: 220px;
  }
  .toolbar input:focus { border-color: var(--primary); }
  .toolbar input::placeholder { color: #475569; }

  .table-wrap {
    padding: 0 24px 24px;
    overflow-x: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.82rem;
  }
  th {
    position: sticky;
    top: 0;
    background: var(--card);
    color: var(--muted);
    text-transform: uppercase;
    font-size: 0.7rem;
    letter-spacing: 0.05em;
    padding: 10px 8px;
    text-align: left;
    border-bottom: 2px solid var(--border);
    white-space: nowrap;
    cursor: pointer;
    user-select: none;
  }
  th:hover { color: var(--text); }
  th .sort-arrow { font-size: 0.65rem; margin-left: 3px; }
  td {
    padding: 8px;
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
    max-width: 240px;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  td.num { text-align: right; font-family: 'Courier New', monospace; }
  tr:hover td { background: rgba(59,130,246,0.05); }
  .badge-src {
    display: inline-block;
    padding: 2px 8px;
    border-radius: 10px;
    font-size: 0.7rem;
    font-weight: 600;
  }
  .badge-peak { background: #166534; color: #bbf7d0; }
  .badge-prerun { background: #92400e; color: #fde68a; }
  .loading {
    text-align: center;
    padding: 60px;
    color: var(--muted);
    font-size: 1rem;
  }
  .error-box {
    margin: 24px;
    padding: 16px;
    background: #450a0a;
    border: 1px solid #dc2626;
    border-radius: 8px;
    color: #fecaca;
  }
  .empty {
    text-align: center;
    padding: 60px;
    color: var(--muted);
  }
</style>
</head>
<body>

<div class="header">
  <h1>&#x1F4BE; Database Viewer</h1>
  <a href="/">&#x2190; Back to Dashboard</a>
</div>

<div class="toolbar">
  <button class="active" onclick="setFilter('all')">All</button>
  <button onclick="setFilter('peak_usage')">Peak Usage</button>
  <button onclick="setFilter('prerun_profile')">Pre-run Profile</button>
  <input type="text" id="searchInput" placeholder="Search pod, image, device..." oninput="applyFilter()">
  <span class="count" id="countLabel">Loading...</span>
</div>

<div class="table-wrap">
  <div id="content" class="loading">Loading data...</div>
</div>

<script>
let allRecords = [];
let currentFilter = 'all';
let sortCol = 'created_at';
let sortDir = -1; // -1 = desc

async function loadData() {
  try {
    const resp = await fetch('/api/db');
    const data = await resp.json();
    if (data.error) {
      document.getElementById('content').innerHTML = '<div class="error-box">' + data.error + '</div>';
      return;
    }
    allRecords = data.records || [];
    render();
  } catch (err) {
    document.getElementById('content').innerHTML = '<div class="error-box">Failed to load: ' + err.message + '</div>';
  }
}

function setFilter(f) {
  currentFilter = f;
  document.querySelectorAll('.toolbar button').forEach(b => b.classList.remove('active'));
  event.target.classList.add('active');
  render();
}

function applyFilter() { render(); }

function setSort(col) {
  if (sortCol === col) { sortDir *= -1; }
  else { sortCol = col; sortDir = -1; }
  render();
}

function render() {
  const search = (document.getElementById('searchInput').value || '').toLowerCase();
  let filtered = allRecords.filter(r => {
    if (currentFilter !== 'all' && r.source !== currentFilter) return false;
    if (search) {
      const hay = (r.pod_name + ' ' + r.pod_namespace + ' ' + r.image + ' ' + r.device_type + ' ' + r.container_name + ' ' + r.image_id).toLowerCase();
      if (!hay.includes(search)) return false;
    }
    return true;
  });

  filtered.sort((a, b) => {
    let va = a[sortCol], vb = b[sortCol];
    if (typeof va === 'number') return (va - vb) * sortDir;
    return String(va).localeCompare(String(vb)) * sortDir;
  });

  document.getElementById('countLabel').textContent = filtered.length + ' of ' + allRecords.length + ' records';

  if (filtered.length === 0) {
    document.getElementById('content').innerHTML = '<div class="empty">No records found.</div>';
    return;
  }

  const arrow = (col) => sortCol === col ? '<span class="sort-arrow">' + (sortDir > 0 ? '&#x25B2;' : '&#x25BC;') + '</span>' : '';

  let html = '<table><thead><tr>';
  const cols = [
    { key: 'id', label: 'ID' },
    { key: 'source', label: 'Source' },
    { key: 'pod_name', label: 'Pod' },
    { key: 'pod_namespace', label: 'Namespace' },
    { key: 'container_name', label: 'Container' },
    { key: 'image', label: 'Image' },
    { key: 'device_type', label: 'GPU Type' },
    { key: 'peak_value_mib', label: 'Peak MiB' },
    { key: 'duration_seconds', label: 'Duration (s)' },
    { key: 'created_at', label: 'Created At' },
  ];
  cols.forEach(c => {
    html += '<th onclick="setSort(\'' + c.key + '\')">' + c.label + arrow(c.key) + '</th>';
  });
  html += '</tr></thead><tbody>';

  filtered.forEach(r => {
    const srcClass = r.source === 'peak_usage' ? 'badge-peak' : 'badge-prerun';
    const srcLabel = r.source === 'peak_usage' ? 'Peak' : 'Pre-run';
    const created = r.created_at ? r.created_at.replace('T', ' ').substring(0, 19) : '';
    html += '<tr>';
    html += '<td class="num">' + r.id + '</td>';
    html += '<td><span class="badge-src ' + srcClass + '">' + srcLabel + '</span></td>';
    html += '<td title="' + r.pod_name + '">' + r.pod_name + '</td>';
    html += '<td>' + r.pod_namespace + '</td>';
    html += '<td>' + r.container_name + '</td>';
    html += '<td title="' + (r.image_id || r.image) + '">' + r.image + '</td>';
    html += '<td>' + r.device_type + '</td>';
    html += '<td class="num" style="color:var(--success);font-weight:600">' + r.peak_value_mib + '</td>';
    html += '<td class="num">' + (r.duration_seconds ? r.duration_seconds.toFixed(1) : '-') + '</td>';
    html += '<td>' + created + '</td>';
    html += '</tr>';
  });

  html += '</tbody></table>';
  document.getElementById('content').innerHTML = html;
}

loadData();
// Auto-refresh every 30 seconds
setInterval(loadData, 30000);
</script>
</body>
</html>
`)

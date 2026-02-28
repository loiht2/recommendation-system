package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	port := getEnv("PORT", "8090")
	recommenderURL := getEnv("RECOMMENDER_URL", "http://recommender.profiler.svc.cluster.local/recommend")

	log.Printf("Web UI starting on :%s", port)
	log.Printf("Recommender API: %s", recommenderURL)

	mux := http.NewServeMux()

	// API proxy to recommender
	mux.HandleFunc("/api/recommend", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(recommenderURL, "application/json", r.Body)
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
	})

	// Health check
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	// Serve static HTML
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
		WriteTimeout: 60 * time.Second,
	}

	log.Fatal(srv.ListenAndServe())
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
    background: var(--primary);
    border: none;
    border-radius: 8px;
    color: white;
    font-size: 0.95rem;
    font-weight: 600;
    cursor: pointer;
    transition: background 0.2s;
    white-space: nowrap;
  }
  button:hover { background: var(--primary-hover); }
  button:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
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

  .examples {
    margin-top: 12px;
  }
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
    <h2>&#x1F50D; Query Recommendation</h2>
    <div class="form-group">
      <label>Container Image URL</label>
      <div class="input-row">
        <input type="text" id="imageInput" placeholder="e.g. docker.io/library/nginx:latest" />
        <button id="submitBtn" onclick="doRecommend()">Recommend</button>
      </div>
    </div>
    <div class="examples">
      <label style="font-size:0.8rem;color:var(--muted);">Try examples:</label>
      <span onclick="setImage('deepspeed/deepspeed:latest')">deepspeed/deepspeed:latest</span>
      <span onclick="setImage('pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime')">pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime</span>
      <span onclick="setImage('nvcr.io/nvidia/tensorflow:24.01-tf2-py3')">nvcr.io/nvidia/tensorflow:24.01-tf2-py3</span>
    </div>
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
</div>

<script>
function setImage(img) {
  document.getElementById('imageInput').value = img;
}

async function doRecommend() {
  const input = document.getElementById('imageInput').value.trim();
  if (!input) { alert('Please enter an image URL'); return; }

  const btn = document.getElementById('submitBtn');
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner"></span>Querying...';

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

    if (data.error) {
      showError(data);
      return;
    }

    if (data.status === 'ok') {
      showOK(data);
    } else if (data.status === 'profiling_required') {
      showProfiling(data);
    } else {
      showError({ error: 'Unexpected response', details: JSON.stringify(data) });
    }
  } catch (err) {
    showError({ error: 'Network error', details: err.message });
  } finally {
    btn.disabled = false;
    btn.innerHTML = 'Recommend';
  }
}

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
      <div class="info-row">
        <span class="key">Image URL</span>
        <span class="val">${data.image_url}</span>
      </div>
      <div class="info-row">
        <span class="key">Image Digest</span>
        <span class="val">${data.image_digest}</span>
      </div>
      <div class="info-row">
        <span class="key">Data Source</span>
        <span class="val">${rec.source}</span>
      </div>
    </div>
    <div class="message-box info">${data.message}</div>
  ` + "`" + `;
}

function showProfiling(data) {
  const badge = document.getElementById('statusBadge');
  badge.className = 'badge badge-profiling';
  badge.textContent = 'Profiling Required';

  document.getElementById('resultContent').innerHTML = ` + "`" + `
    <div style="margin-top:8px">
      <div class="info-row">
        <span class="key">Image URL</span>
        <span class="val">${data.image_url}</span>
      </div>
      <div class="info-row">
        <span class="key">Image Digest</span>
        <span class="val">${data.image_digest}</span>
      </div>
    </div>
    <div class="message-box warn">
      <strong>&#x26A0; No Historical Data</strong><br>
      ${data.message}<br><br>
      To profile this image, create a pod with the label <code>vram-profiling: "true"</code>.
      The profiler will automatically capture VRAM usage during a 45-second window.
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

// Submit on Enter
document.getElementById('imageInput').addEventListener('keydown', function(e) {
  if (e.key === 'Enter') doRecommend();
});
</script>
</body>
</html>
`)

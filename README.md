# vGPU Recommendation System

A Kubernetes-native system that **profiles GPU workloads** and **recommends VRAM allocations** for container images. It consists of two microservices (profiler + recommender) and a shared PostgreSQL database, all running in the `profiler` namespace.

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                        │
│                                                                  │
│  ┌─────────────────┐    Prometheus     ┌──────────────────────┐  │
│  │  GPU Workload    │───(vGPU metrics)──│  kube-prometheus     │  │
│  │  Pods            │                   │  :9090               │  │
│  └─────────────────┘                    └──────────┬───────────┘  │
│                                                    │              │
│  Namespace: profiler                               │              │
│  ┌─────────────────────────────────────────────────┼───────────┐  │
│  │                                                 │           │  │
│  │  ┌──────────────┐    query peak VRAM    ┌───────┴────────┐  │  │
│  │  │  vgpu-       │◄────────────────────► │  Prometheus    │  │  │
│  │  │  profiler     │    max_over_time()    │  API           │  │  │
│  │  │              │                       └────────────────┘  │  │
│  │  │  • watches   │                                           │  │
│  │  │    pods via  │    insert records                         │  │
│  │  │    informers │──────────────┐                            │  │
│  │  │  • deletes   │              │                            │  │
│  │  │    profiling │              ▼                             │  │
│  │  │    pods      │    ┌─────────────────┐                    │  │
│  │  └──────────────┘    │  PostgreSQL 15  │                    │  │
│  │                      │  :5432          │                    │  │
│  │  ┌──────────────┐    │  ┌───────────┐  │                    │  │
│  │  │  recommender │    │  │peak_usage │  │                    │  │
│  │  │  :80 (API)   │────┤  │prerun_    │  │                    │  │
│  │  │              │    │  │profile    │  │                    │  │
│  │  │  POST        │    └──┴───────────┴──┘                    │  │
│  │  │  /recommend  │                                           │  │
│  │  └──────────────┘                                           │  │
│  └─────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

---

## Components

### 1. Profiler (`profiler/`)

An **event-driven** Go service that watches Kubernetes pods via SharedInformers and records peak GPU memory (VRAM) usage from Prometheus into PostgreSQL.

**Two watchers run concurrently:**

| Watcher | Label Selector | Trigger | Action |
|---------|---------------|---------|--------|
| Training completion | `job-type=training` | Pod reaches `Succeeded` or `Failed` | Query `max_over_time()` over pod's lifetime → save to `vgpu_peak_usage` |
| Pre-run profiling | `vram-profiling=true` | Pod reaches `Running` | Wait 45s → query `max_over_time()` → save to `vgpu_prerun_profile` → **delete pod** |

**Key features:**
- `sync.Map` deduplication keyed by **Pod UID** (globally unique, survives name reuse)
- Startup **orphan sweep**: on (re)start, finds leftover profiling pods from a previous crash — deletes orphans past the window, resumes timers for pods still within it
- HTTP health server on `:8081` (`/healthz` for liveness, `/readyz` for readiness with DB ping)
- All metric labels from Prometheus are captured: `pod_uid`, `device_type`, `image`, `image_id`, `ctrname`, `deviceuuid`, `vdeviceid`

**Source files (1,419 LOC):**

```
profiler/
├── cmd/main.go                          # Entry point, event loop, orphan sweep, health server
├── internal/
│   ├── config/config.go                 # Env-based configuration
│   ├── config/config_test.go            # Config unit tests
│   ├── db/store.go                      # PostgreSQL store, migrations, inserts
│   ├── prometheus/client.go             # Prometheus query client (max_over_time)
│   ├── prometheus/client_test.go        # Prometheus client unit tests
│   ├── watcher/watcher.go              # K8s SharedInformer pod watchers
│   └── watcher/watcher_test.go         # Watcher unit tests
├── deploy/                              # K8s manifests (01-08)
├── Dockerfile                           # Multi-stage: golang:1.23-alpine → alpine:3.20
└── go.mod                               # Module: github.com/loihoangthanh1411/profiler
```

### 2. Recommender (`recommender/`)

An **HTTP API** service that resolves container image tags to their SHA256 digest via the Docker Registry V2 API, then looks up historical VRAM profiling data to recommend an optimal GPU memory allocation.

**API:**

```
POST /recommend
Content-Type: application/json

{"image_url": "docker.io/deepspeed/deepspeed:latest"}
```

**Response (with data):**
```json
{
  "status": "ok",
  "image_url": "docker.io/deepspeed/deepspeed:latest",
  "image_digest": "sha256:589ec2555fe6...",
  "message": "VRAM recommendation calculated from historical profiling data.",
  "recommendation": {
    "peak_vram_mib": 2586,
    "safety_buffer_percent": 10,
    "recommended_vram_mib": 2845,
    "record_count": 2,
    "source": "prerun_profile"
  }
}
```

**Response (no data):**
```json
{
  "status": "profiling_required",
  "image_url": "...",
  "image_digest": "sha256:...",
  "message": "No historical VRAM data found for this image digest. Please run a profiling pod first."
}
```

**Key features:**
- Registry resolver supports Docker Hub, GHCR, Quay, GCR, NVCR, ECR, ACR, Harbor, localhost registries
- Bearer + Basic auth support
- Multi-arch (OCI Index) → drills down to `linux/amd64` manifest digest
- DB uses `LIKE '%' || $1` to match the recommender's `sha256:...` against the profiler's full `image_id` (e.g. `docker.io/repo@sha256:...`)
- Safety buffer configurable via `SAFETY_BUFFER_PERCENT` (default 10%)

**Source files (2,143 LOC):**

```
recommender/
├── cmd/main.go                                  # Entry point, HTTP server, graceful shutdown
├── internal/
│   ├── config/config.go                         # Env-based configuration
│   ├── db/store.go                              # PostgreSQL read-only queries (UNION ALL)
│   ├── handler/recommend.go                     # POST /recommend handler + recommendation logic
│   ├── handler/recommend_test.go                # Handler unit tests (mock store + resolver)
│   ├── registry/resolver.go                     # OCI/Docker V2 registry resolver
│   ├── registry/resolver_test.go                # ParseImageURL + helper unit tests
│   ├── registry/resolve_integration_test.go     # Fake registry integration tests
│   └── registry/live_test.go                    # Real registry live tests
├── deploy/                                      # K8s manifests (01-03)
├── Dockerfile                                   # Multi-stage: golang:1.23-alpine → alpine:3.20
└── go.mod                                       # Module: github.com/loihoangthanh1411/recommender
```

### 3. PostgreSQL 15

Dedicated PostgreSQL instance in the `profiler` namespace with persistent storage (PVC). Schema is auto-migrated by the profiler on startup.

---

## Database Schema

### `vgpu_peak_usage` — Training pod completion records

| Column | Type | Description |
|--------|------|-------------|
| `id` | BIGSERIAL | Primary key |
| `pod_name` | TEXT | K8s pod name |
| `pod_namespace` | TEXT | K8s namespace |
| `pod_uid` | TEXT | K8s pod UID (unique per pod instance) |
| `container_name` | TEXT | Container name from Prometheus `ctrname` label |
| `device_uuid` | TEXT | GPU device UUID from Prometheus `deviceuuid` label |
| `device_type` | TEXT | GPU model from Prometheus `device_type` label (e.g. `Tesla V100-PCIE-32GB`) |
| `vdevice_id` | TEXT | Virtual device ID from Prometheus `vdeviceid` label |
| `image` | TEXT | Container image reference from Prometheus `image` label |
| `image_id` | TEXT | Full image ID with digest from Prometheus `image_id` label |
| `metric_name` | TEXT | Prometheus metric name queried |
| `peak_value_mib` | DOUBLE PRECISION | Peak VRAM usage in MiB |
| `pod_start_time` | TIMESTAMPTZ | When the pod started |
| `pod_end_time` | TIMESTAMPTZ | When the pod completed |
| `duration_seconds` | DOUBLE PRECISION | Pod lifetime in seconds |
| `pod_phase` | TEXT | Final pod phase (`Succeeded` or `Failed`) |
| `created_at` | TIMESTAMPTZ | Record creation time |

### `vgpu_prerun_profile` — Pre-run profiling records

| Column | Type | Description |
|--------|------|-------------|
| `id` | BIGSERIAL | Primary key |
| `pod_name` | TEXT | K8s pod name |
| `pod_namespace` | TEXT | K8s namespace |
| `pod_uid` | TEXT | K8s pod UID |
| `container_name` | TEXT | Container name from Prometheus |
| `device_uuid` | TEXT | GPU device UUID |
| `device_type` | TEXT | GPU model |
| `vdevice_id` | TEXT | Virtual device ID |
| `image` | TEXT | Container image reference |
| `image_id` | TEXT | Full image ID with digest |
| `metric_name` | TEXT | Prometheus metric name |
| `peak_value_mib` | DOUBLE PRECISION | Peak VRAM usage in MiB |
| `profile_start` | TIMESTAMPTZ | Profiling start time |
| `profile_end` | TIMESTAMPTZ | Profiling end time |
| `duration_seconds` | DOUBLE PRECISION | Profiling window in seconds |
| `created_at` | TIMESTAMPTZ | Record creation time |

**Indexes:** Both tables have indexes on `(pod_name, pod_namespace)`, `(created_at DESC)`, and a conditional index on `image_id WHERE image_id != ''`.

---

## Kubernetes Deployment

All resources live in the `profiler` namespace.

### Profiler manifests (`profiler/deploy/`)

| File | Resource | Purpose |
|------|----------|---------|
| `01-namespace.yaml` | Namespace | `profiler` namespace |
| `02-secret.yaml` | Secret | DB credentials (`DB_USER`, `DB_PASSWORD`, `POSTGRES_USER`, `POSTGRES_PASSWORD`) |
| `03-configmap.yaml` | ConfigMap | Profiler config (Prometheus URL, metrics, labels, duration, DB host) |
| `04-postgresql-pvc.yaml` | PVC | 5Gi persistent volume for PostgreSQL data |
| `05-postgresql-deployment.yaml` | Deployment | PostgreSQL 15 with data volume |
| `06-postgresql-service.yaml` | Service | ClusterIP for PostgreSQL on port 5432 |
| `07-rbac.yaml` | SA + ClusterRole + Binding | `get`, `list`, `watch`, `delete` on pods (cluster-wide) |
| `08-profiler-deployment.yaml` | Deployment | Profiler with health probes, init container (wait-for-db) |

### Recommender manifests (`recommender/deploy/`)

| File | Resource | Purpose |
|------|----------|---------|
| `01-configmap.yaml` | ConfigMap | Recommender config (listen addr, DB host, safety buffer) |
| `02-deployment.yaml` | Deployment | Recommender with liveness/readiness probes, init container |
| `03-service.yaml` | Service | ClusterIP on port 80 → container port 8080 |

### Docker images

| Image | Tag | Base |
|-------|-----|------|
| `docker.io/loihoangthanh1411/profiler` | `v1.0` | `alpine:3.20` |
| `docker.io/loihoangthanh1411/recommender` | `v1.0` | `alpine:3.20` |

---

## Configuration

### Profiler environment variables

| Variable | Source | Default | Description |
|----------|--------|---------|-------------|
| `PROMETHEUS_URL` | ConfigMap | `http://kube-prometheus-stack-prometheus.prometheus.svc.cluster.local:9090` | Prometheus address |
| `VRAM_METRIC` | ConfigMap | `vGPU_device_memory_usage_real_in_MiB` | Metric to query |
| `WATCH_LABEL` | ConfigMap | `job-type=training` | Label selector for training pods |
| `PROFILING_LABEL` | ConfigMap | `vram-profiling=true` | Label selector for pre-run profiling pods |
| `PROFILING_DURATION` | ConfigMap | `45s` | How long to collect VRAM data before saving |
| `HEALTH_PORT` | ConfigMap | `:8081` | Health endpoint listen address |
| `DB_HOST` | ConfigMap | `profiler-postgresql.profiler.svc.cluster.local` | PostgreSQL host |
| `DB_PORT` | ConfigMap | `5432` | PostgreSQL port |
| `DB_NAME` | ConfigMap | `profiler` | Database name |
| `DB_SSLMODE` | ConfigMap | `disable` | SSL mode |
| `DB_USER` | Secret | `profiler` | Database user |
| `DB_PASSWORD` | Secret | `profiler` | Database password |

### Recommender environment variables

| Variable | Source | Default | Description |
|----------|--------|---------|-------------|
| `LISTEN_ADDR` | ConfigMap | `:8080` | HTTP server listen address |
| `DB_HOST` | ConfigMap | `profiler-postgresql.profiler.svc.cluster.local` | PostgreSQL host |
| `DB_PORT` | ConfigMap | `5432` | PostgreSQL port |
| `DB_NAME` | ConfigMap | `profiler` | Database name |
| `DB_SSLMODE` | ConfigMap | `disable` | SSL mode |
| `SAFETY_BUFFER_PERCENT` | ConfigMap | `10` | Buffer % added on top of peak VRAM |
| `DB_USER` | Secret | `profiler` | Database user |
| `DB_PASSWORD` | Secret | `profiler` | Database password |
| `REGISTRY_USER` | Secret | _(empty)_ | Docker registry user (optional) |
| `REGISTRY_PASSWORD` | Secret | _(empty)_ | Docker registry password (optional) |

---

## How It Works End-to-End

### Flow 1: Training Pod Completion Profiling

```
1. User submits a training pod with label: job-type=training
2. Pod runs training workload on GPU
3. HAMI DRA monitor exports vGPU_device_memory_usage_real_in_MiB to Prometheus
4. Pod completes (Succeeded/Failed)
5. Profiler's SharedInformer detects phase change
6. Profiler queries Prometheus: max_over_time(metric{podname="...",podnamespace="..."}[duration])
7. Profiler saves peak VRAM + all labels to vgpu_peak_usage table
```

### Flow 2: Pre-Run VRAM Profiling

```
1. User submits a pod with label: vram-profiling=true
2. Pod starts running on GPU
3. Profiler's SharedInformer detects Running phase
4. Profiler starts a 45-second timer (configurable)
5. After 45s, profiler queries Prometheus for peak VRAM over the window
6. Profiler saves results to vgpu_prerun_profile table
7. Profiler DELETES the pod (frees GPU for other workloads)
```

### Flow 3: VRAM Recommendation

```
1. Client sends POST /recommend with {"image_url": "docker.io/deepspeed/deepspeed:latest"}
2. Recommender parses the image reference
3. Recommender resolves the tag to a linux/amd64 sha256 digest via the registry API
4. Recommender queries PostgreSQL for records where image_id contains that digest
5. If records exist: return max(peak_value_mib) * (1 + safety_buffer/100), rounded up
6. If no records: return "profiling_required" status
```

### Image Digest Linkage

The profiler stores the `image_id` label from Prometheus, which comes from HAMI's monitor and looks like:
```
docker.io/deepspeed/deepspeed@sha256:589ec2555fe68022b7ae8804cbb8fb84c8c5f1a6ec7ad3448842d4bd9cb1962b
```

The recommender resolves the same image tag from the registry to get the platform-specific `sha256:...` digest. The DB query uses `LIKE '%' || $1` to match the digest suffix, bridging both formats.

---

## Robustness Features

### Duplicate Event Handling
Kubernetes SharedInformers can fire the same event multiple times (resync, restarts). Both watchers use `sync.Map` with `LoadOrStore` keyed by **Pod UID** (`types.UID`). Pod UID is globally unique — even if a new pod reuses the same name, it gets a different UID.

### Crash Recovery (Orphan Sweep)
On startup, the profiler lists all pods with `vram-profiling=true` across all namespaces:
- **Orphans** (uptime ≥ profiling duration): deleted immediately
- **In-progress** (uptime < profiling duration): timer resumed with remaining time

The UID is marked as "seen" in both cases to prevent duplicate events from the informer's initial list.

### Health Probes
| Component | Liveness | Readiness |
|-----------|----------|-----------|
| Profiler | `GET /healthz` → 200 OK | `GET /readyz` → DB ping |
| Recommender | `GET /healthz` → 200 OK | `GET /healthz` → 200 OK |

### Init Containers
Both profiler and recommender use a busybox init container that waits for PostgreSQL to be ready (`nc -z`) before starting the main container.

---

## Testing

### Profiler tests (14 tests)
```bash
cd profiler && go test ./internal/... -v
```
- `config_test.go`: Default values, env overrides, label parsing (valid + invalid)
- `client_test.go`: All Prometheus label extraction, empty result handling, min duration clamping
- `watcher_test.go`: Pod duration computation (start time, no termination, minimum, fallback), event struct validation

### Recommender tests (~40 tests)
```bash
cd recommender && go test ./internal/... -v
```
- `recommend_test.go`: Method validation, JSON parsing, error paths, profiling-required, recommendation calculation, safety buffer, mixed sources, content type, digest pass-through
- `resolver_test.go`: Image URL parsing for Docker Hub, GHCR, Quay, GCR, NVCR, ECR, ACR, localhost, private registries, edge cases
- `resolve_integration_test.go`: Fake registry server (anonymous, bearer, basic auth, manifest list, single manifest)
- `live_test.go`: Real public registry tests (Docker Hub, GHCR, Quay, GCR)

---

## Quick Start

```bash
# Deploy everything
kubectl apply -f profiler/deploy/
kubectl apply -f recommender/deploy/

# Verify all pods are running
kubectl get pods -n profiler

# Test the recommender API
kubectl run curl --rm -it --image=curlimages/curl -- \
  curl -s -X POST http://recommender.profiler.svc.cluster.local/recommend \
  -H 'Content-Type: application/json' \
  -d '{"image_url": "docker.io/deepspeed/deepspeed:latest"}'

# Submit a profiling pod (will be auto-deleted after 45s)
kubectl apply -f profiler/pod-hami-dra.yaml

# Check profiler logs
kubectl logs -n profiler deploy/vgpu-profiler -f

# Check DB records
kubectl exec -n profiler deploy/profiler-postgresql -- \
  psql -U profiler -d profiler -c "SELECT * FROM vgpu_prerun_profile;"
```

---

## Technology Stack

| Component | Technology | Version |
|-----------|-----------|---------|
| Language | Go | 1.23 (module) / 1.25.1 (system) |
| K8s client | client-go | v0.32.3 |
| Prometheus client | prometheus/client_golang | v1.22.0 |
| Database driver | lib/pq | v1.10.9 |
| Database | PostgreSQL | 15 |
| Container runtime | Docker | Multi-stage build |
| Container base | Alpine Linux | 3.20 |
| Orchestration | Kubernetes | With HAMI DRA vGPU plugin |
| Monitoring | Prometheus | kube-prometheus-stack |

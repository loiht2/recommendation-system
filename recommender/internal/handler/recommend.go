package handler

import (
	"encoding/json"
	"log"
	"math"
	"net/http"

	"github.com/loihoangthanh1411/recommender/internal/db"
	"github.com/loihoangthanh1411/recommender/internal/registry"
)

// DigestResolver resolves an image reference to its linux/amd64 digest.
type DigestResolver interface {
	ResolveDigest(ref *registry.ImageRef) (string, error)
}

// VRAMStore queries the database for historical VRAM records.
type VRAMStore interface {
	QueryByImageID(digest string) ([]db.VRAMRecord, error)
}

// RecommendHandler handles POST /recommend requests.
type RecommendHandler struct {
	store           VRAMStore
	resolver        DigestResolver
	safetyBufferPct float64
}

// NewRecommendHandler creates a new handler.
func NewRecommendHandler(store VRAMStore, resolver DigestResolver, safetyBufferPct float64) *RecommendHandler {
	return &RecommendHandler{
		store:           store,
		resolver:        resolver,
		safetyBufferPct: safetyBufferPct,
	}
}

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

type recommendRequest struct {
	ImageURL string `json:"image_url"`
}

type recommendResponse struct {
	Status         string          `json:"status"`           // "ok" or "profiling_required"
	ImageURL       string          `json:"image_url"`        // original input
	ImageDigest    string          `json:"image_digest"`     // resolved sha256 digest
	Message        string          `json:"message"`          // human-readable explanation
	Recommendation *recommendation `json:"recommendation,omitempty"`
}

type recommendation struct {
	PeakVRAMMiB        float64 `json:"peak_vram_mib"`          // observed max across all records
	SafetyBufferPct    float64 `json:"safety_buffer_percent"`   // applied buffer %
	RecommendedVRAMMiB float64 `json:"recommended_vram_mib"`    // peak * (1 + buffer/100), rounded up
	RecordCount        int     `json:"record_count"`            // how many historical records we used
	Source             string  `json:"source"`                  // "peak_usage", "prerun_profile", or "mixed"
}

type errorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// ServeHTTP implements http.Handler.
func (h *RecommendHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed, use POST"})
		return
	}

	var req recommendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body", Details: err.Error()})
		return
	}
	if req.ImageURL == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "image_url is required"})
		return
	}

	// Step 1: Parse the image reference
	ref, err := registry.ParseImageURL(req.ImageURL)
	if err != nil {
		log.Printf("ERROR: parsing image_url %q: %v", req.ImageURL, err)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid image_url", Details: err.Error()})
		return
	}

	log.Printf("Resolving digest for %s/%s:%s (registry=%s)", ref.Registry, ref.Repository, ref.Tag, ref.Registry)

	// Step 2: Resolve the exact linux/amd64 digest from the registry
	digest, err := h.resolver.ResolveDigest(ref)
	if err != nil {
		log.Printf("ERROR: resolving digest for %q: %v", req.ImageURL, err)
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error:   "failed to resolve image digest from registry",
			Details: err.Error(),
		})
		return
	}

	log.Printf("Resolved %q → %s", req.ImageURL, digest)

	// Step 3: Query database for historical VRAM records with this digest
	records, err := h.store.QueryByImageID(digest)
	if err != nil {
		log.Printf("ERROR: querying DB for digest %s: %v", digest, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error:   "database query failed",
			Details: err.Error(),
		})
		return
	}

	// Step 4: Build response
	if len(records) == 0 {
		// No historical data — profiling required
		writeJSON(w, http.StatusOK, recommendResponse{
			Status:      "profiling_required",
			ImageURL:    req.ImageURL,
			ImageDigest: digest,
			Message:     "No historical VRAM data found for this image digest. Please run a profiling pod first.",
		})
		return
	}

	// Compute recommendation from historical records
	rec := h.computeRecommendation(records)

	writeJSON(w, http.StatusOK, recommendResponse{
		Status:         "ok",
		ImageURL:       req.ImageURL,
		ImageDigest:    digest,
		Message:        "VRAM recommendation calculated from historical profiling data.",
		Recommendation: rec,
	})
}

// ---------------------------------------------------------------------------
// Recommendation calculation
// ---------------------------------------------------------------------------

func (h *RecommendHandler) computeRecommendation(records []db.VRAMRecord) *recommendation {
	var peakMax float64
	sourceSet := map[string]bool{}

	for _, r := range records {
		if r.PeakValueMiB > peakMax {
			peakMax = r.PeakValueMiB
		}
		sourceSet[r.Source] = true
	}

	// Determine source label
	source := "mixed"
	if len(sourceSet) == 1 {
		for k := range sourceSet {
			source = k
		}
	}

	// Apply safety buffer
	recommended := peakMax * (1.0 + h.safetyBufferPct/100.0)

	// Round up to nearest integer MiB
	recommended = math.Ceil(recommended)

	return &recommendation{
		PeakVRAMMiB:        peakMax,
		SafetyBufferPct:    h.safetyBufferPct,
		RecommendedVRAMMiB: recommended,
		RecordCount:        len(records),
		Source:             source,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ERROR: writing JSON response: %v", err)
	}
}

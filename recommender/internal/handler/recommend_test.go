package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loihoangthanh1411/recommender/internal/db"
	"github.com/loihoangthanh1411/recommender/internal/registry"
)

// ---------------------------------------------------------------------------
// Mock implementations
// ---------------------------------------------------------------------------

type mockResolver struct {
	digest string
	err    error
}

func (m *mockResolver) ResolveDigest(ref *registry.ImageRef) (string, error) {
	return m.digest, m.err
}

type mockStore struct {
	records []db.VRAMRecord
	err     error
	// Track the last digest we were queried with for assertions.
	lastDigest string
}

func (m *mockStore) QueryByImageID(digest string) ([]db.VRAMRecord, error) {
	m.lastDigest = digest
	return m.records, m.err
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRecommendHandler_MethodNotAllowed(t *testing.T) {
	h := NewRecommendHandler(&mockStore{}, &mockResolver{}, 10.0)

	req := httptest.NewRequest(http.MethodGet, "/recommend", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestRecommendHandler_EmptyBody(t *testing.T) {
	h := NewRecommendHandler(&mockStore{}, &mockResolver{}, 10.0)

	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "image_url is required" {
		t.Errorf("error = %q, want 'image_url is required'", resp.Error)
	}
}

func TestRecommendHandler_InvalidJSON(t *testing.T) {
	h := NewRecommendHandler(&mockStore{}, &mockResolver{}, 10.0)

	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestRecommendHandler_InvalidImageURL(t *testing.T) {
	h := NewRecommendHandler(&mockStore{}, &mockResolver{}, 10.0)

	body := `{"image_url": "nginx@sha256:abc123"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestRecommendHandler_RegistryError(t *testing.T) {
	h := NewRecommendHandler(
		&mockStore{},
		&mockResolver{err: fmt.Errorf("connection refused")},
		10.0,
	)

	body := `{"image_url": "ghcr.io/owner/repo:v1"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestRecommendHandler_DatabaseError(t *testing.T) {
	h := NewRecommendHandler(
		&mockStore{err: fmt.Errorf("db connection lost")},
		&mockResolver{digest: "sha256:abc123"},
		10.0,
	)

	body := `{"image_url": "quay.io/org/image:v1"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestRecommendHandler_ProfilingRequired(t *testing.T) {
	h := NewRecommendHandler(
		&mockStore{records: nil}, // no records
		&mockResolver{digest: "sha256:deadbeef"},
		10.0,
	)

	body := `{"image_url": "nvcr.io/nvidia/pytorch:24.01-py3"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp recommendResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.Status != "profiling_required" {
		t.Errorf("status = %q, want 'profiling_required'", resp.Status)
	}
	if resp.ImageDigest != "sha256:deadbeef" {
		t.Errorf("image_digest = %q, want 'sha256:deadbeef'", resp.ImageDigest)
	}
	if resp.Recommendation != nil {
		t.Error("expected nil recommendation for profiling_required")
	}
}

func TestRecommendHandler_WithRecords(t *testing.T) {
	now := time.Now()
	records := []db.VRAMRecord{
		{
			ID: 1, PodName: "train-1", PodNamespace: "default",
			PeakValueMiB: 4096.5, Source: "peak_usage",
			StartTime: now.Add(-1 * time.Hour), EndTime: now,
			ImageID: "docker.io/team/model@sha256:abc",
		},
		{
			ID: 2, PodName: "train-2", PodNamespace: "default",
			PeakValueMiB: 3800.0, Source: "peak_usage",
			StartTime: now.Add(-2 * time.Hour), EndTime: now.Add(-1 * time.Hour),
			ImageID: "docker.io/team/model@sha256:abc",
		},
	}

	h := NewRecommendHandler(
		&mockStore{records: records},
		&mockResolver{digest: "sha256:abc"},
		10.0,
	)

	body := `{"image_url": "registry.example.com/team/model:v3"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp recommendResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.Status != "ok" {
		t.Errorf("status = %q, want 'ok'", resp.Status)
	}
	if resp.Recommendation == nil {
		t.Fatal("expected non-nil recommendation")
	}

	rec := resp.Recommendation

	if rec.PeakVRAMMiB != 4096.5 {
		t.Errorf("PeakVRAMMiB = %f, want 4096.5", rec.PeakVRAMMiB)
	}
	if rec.SafetyBufferPct != 10.0 {
		t.Errorf("SafetyBufferPct = %f, want 10.0", rec.SafetyBufferPct)
	}
	// 4096.5 * 1.10 = 4506.15 → ceil = 4507
	expectedRecommended := math.Ceil(4096.5 * 1.10)
	if rec.RecommendedVRAMMiB != expectedRecommended {
		t.Errorf("RecommendedVRAMMiB = %f, want %f", rec.RecommendedVRAMMiB, expectedRecommended)
	}
	if rec.RecordCount != 2 {
		t.Errorf("RecordCount = %d, want 2", rec.RecordCount)
	}
	if rec.Source != "peak_usage" {
		t.Errorf("Source = %q, want 'peak_usage'", rec.Source)
	}
}

func TestRecommendHandler_MixedSources(t *testing.T) {
	records := []db.VRAMRecord{
		{PeakValueMiB: 2000.0, Source: "peak_usage"},
		{PeakValueMiB: 2500.0, Source: "prerun_profile"},
	}

	h := NewRecommendHandler(
		&mockStore{records: records},
		&mockResolver{digest: "sha256:mixed"},
		15.0,
	)

	body := `{"image_url": "ghcr.io/owner/repo:main"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp recommendResponse
	json.NewDecoder(w.Body).Decode(&resp)

	rec := resp.Recommendation
	if rec == nil {
		t.Fatal("expected recommendation")
	}
	if rec.PeakVRAMMiB != 2500.0 {
		t.Errorf("PeakVRAMMiB = %f, want 2500.0", rec.PeakVRAMMiB)
	}
	if rec.Source != "mixed" {
		t.Errorf("Source = %q, want 'mixed'", rec.Source)
	}
	// 2500 * 1.15 = 2875.0
	if rec.RecommendedVRAMMiB != math.Ceil(2500.0*1.15) {
		t.Errorf("RecommendedVRAMMiB = %f, want %f", rec.RecommendedVRAMMiB, math.Ceil(2500.0*1.15))
	}
}

func TestRecommendHandler_ContentTypeJSON(t *testing.T) {
	h := NewRecommendHandler(
		&mockStore{records: nil},
		&mockResolver{digest: "sha256:abc"},
		10.0,
	)

	body := `{"image_url": "nginx:latest"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want 'application/json'", ct)
	}
}

func TestRecommendHandler_DigestPassedToStore(t *testing.T) {
	store := &mockStore{records: nil}
	h := NewRecommendHandler(
		store,
		&mockResolver{digest: "sha256:specific_digest_123"},
		10.0,
	)

	body := `{"image_url": "quay.io/foo/bar:v2"}`
	req := httptest.NewRequest(http.MethodPost, "/recommend", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if store.lastDigest != "sha256:specific_digest_123" {
		t.Errorf("store queried with digest %q, want 'sha256:specific_digest_123'", store.lastDigest)
	}
}

func TestComputeRecommendation_ZeroBuffer(t *testing.T) {
	h := &RecommendHandler{safetyBufferPct: 0.0}
	records := []db.VRAMRecord{
		{PeakValueMiB: 1024.0, Source: "prerun_profile"},
	}

	rec := h.computeRecommendation(records)
	if rec.PeakVRAMMiB != 1024.0 {
		t.Errorf("PeakVRAMMiB = %f, want 1024.0", rec.PeakVRAMMiB)
	}
	if rec.RecommendedVRAMMiB != 1024.0 {
		t.Errorf("RecommendedVRAMMiB = %f, want 1024.0 (0%% buffer)", rec.RecommendedVRAMMiB)
	}
}

func TestComputeRecommendation_CeilRounding(t *testing.T) {
	h := &RecommendHandler{safetyBufferPct: 10.0}
	records := []db.VRAMRecord{
		{PeakValueMiB: 1000.1, Source: "peak_usage"},
	}

	rec := h.computeRecommendation(records)
	// 1000.1 * 1.10 = 1100.11 → ceil = 1101
	if rec.RecommendedVRAMMiB != 1101.0 {
		t.Errorf("RecommendedVRAMMiB = %f, want 1101.0", rec.RecommendedVRAMMiB)
	}
}

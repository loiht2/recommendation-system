package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	t.Run("valid address", func(t *testing.T) {
		c, err := NewClient("http://localhost:9090")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c == nil {
			t.Fatal("expected non-nil client")
		}
	})
}

func TestQueryPeakVRAM_ParsesAllLabels(t *testing.T) {
	// Fake Prometheus server that returns a vector with all labels.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [{
					"metric": {
						"__name__": "vGPU_device_memory_usage_real_in_MiB",
						"ctrname": "trainer",
						"deviceuuid": "GPU-abc-123",
						"device_type": "Tesla V100-PCIE-32GB",
						"vdeviceid": "0",
						"image": "docker.io/deepspeed/deepspeed:latest",
						"image_id": "docker.io/deepspeed/deepspeed@sha256:deadbeef",
						"pod_uid": "uid-1234",
						"podname": "my-pod",
						"podnamespace": "test-ns"
					},
					"value": [1700000000, "4096"]
				}]
			}
		}`)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	results, err := c.QueryPeakVRAM(context.Background(), "vGPU_device_memory_usage_real_in_MiB", "my-pod", "test-ns", 60*time.Second)
	if err != nil {
		t.Fatalf("QueryPeakVRAM: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	checks := []struct {
		field string
		got   string
		want  string
	}{
		{"PodName", r.PodName, "my-pod"},
		{"PodNamespace", r.PodNamespace, "test-ns"},
		{"PodUID", r.PodUID, "uid-1234"},
		{"ContainerName", r.ContainerName, "trainer"},
		{"DeviceUUID", r.DeviceUUID, "GPU-abc-123"},
		{"DeviceType", r.DeviceType, "Tesla V100-PCIE-32GB"},
		{"VDeviceID", r.VDeviceID, "0"},
		{"Image", r.Image, "docker.io/deepspeed/deepspeed:latest"},
		{"ImageID", r.ImageID, "docker.io/deepspeed/deepspeed@sha256:deadbeef"},
		{"MetricName", r.MetricName, "vGPU_device_memory_usage_real_in_MiB"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if r.PeakValueMiB != 4096 {
		t.Errorf("PeakValueMiB = %f, want 4096", r.PeakValueMiB)
	}
}

func TestQueryPeakVRAM_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": []
			}
		}`)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.QueryPeakVRAM(context.Background(), "metric", "pod", "ns", 60*time.Second)
	if err == nil {
		t.Fatal("expected error for empty result, got nil")
	}
}

func TestQueryPeakVRAM_MinDuration(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prometheus client may use GET or POST; capture query from either.
		gotQuery = r.URL.Query().Get("query")
		if gotQuery == "" {
			r.ParseForm()
			gotQuery = r.FormValue("query")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [{"metric": {}, "value": [1700000000, "100"]}]
			}
		}`)
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL)

	// Duration less than 1 second should clamp to 1s
	_, err := c.QueryPeakVRAM(context.Background(), "m", "p", "n", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery == "" {
		t.Fatal("no query received")
	}
	if !contains(gotQuery, "[1s]") {
		t.Errorf("expected [1s] in query, got %q", gotQuery)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstr(s, substr)
}

func searchSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might interfere
	for _, key := range []string{"PROMETHEUS_URL", "VRAM_METRIC", "WATCH_LABEL",
		"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE",
		"PROFILING_LABEL", "PROFILING_DURATION", "HEALTH_PORT"} {
		os.Unsetenv(key)
	}

	cfg := Load()

	if cfg.PrometheusURL != "http://kube-prometheus-stack-prometheus.prometheus.svc.cluster.local:9090" {
		t.Errorf("PrometheusURL = %q", cfg.PrometheusURL)
	}
	if cfg.VRAMMetric != "vGPU_device_memory_usage_real_in_MiB" {
		t.Errorf("VRAMMetric = %q", cfg.VRAMMetric)
	}
	if cfg.WatchLabel != "job-type=training" {
		t.Errorf("WatchLabel = %q", cfg.WatchLabel)
	}
	if cfg.DBPort != 5432 {
		t.Errorf("DBPort = %d, want 5432", cfg.DBPort)
	}
	if cfg.ProfilingDuration != 45*time.Second {
		t.Errorf("ProfilingDuration = %s, want 45s", cfg.ProfilingDuration)
	}
	if cfg.HealthPort != ":8081" {
		t.Errorf("HealthPort = %q, want ':8081'", cfg.HealthPort)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	os.Setenv("PROMETHEUS_URL", "http://custom:9090")
	os.Setenv("DB_PORT", "5433")
	os.Setenv("PROFILING_DURATION", "2m")
	os.Setenv("HEALTH_PORT", ":9090")
	defer func() {
		os.Unsetenv("PROMETHEUS_URL")
		os.Unsetenv("DB_PORT")
		os.Unsetenv("PROFILING_DURATION")
		os.Unsetenv("HEALTH_PORT")
	}()

	cfg := Load()

	if cfg.PrometheusURL != "http://custom:9090" {
		t.Errorf("PrometheusURL = %q, want http://custom:9090", cfg.PrometheusURL)
	}
	if cfg.DBPort != 5433 {
		t.Errorf("DBPort = %d, want 5433", cfg.DBPort)
	}
	if cfg.ProfilingDuration != 2*time.Minute {
		t.Errorf("ProfilingDuration = %s, want 2m0s", cfg.ProfilingDuration)
	}
	if cfg.HealthPort != ":9090" {
		t.Errorf("HealthPort = %q, want ':9090'", cfg.HealthPort)
	}
}

func TestWatchLabelParts(t *testing.T) {
	cfg := &Config{WatchLabel: "job-type=training"}
	k, v, err := cfg.WatchLabelParts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k != "job-type" || v != "training" {
		t.Errorf("got (%q, %q), want (job-type, training)", k, v)
	}
}

func TestWatchLabelParts_Invalid(t *testing.T) {
	cfg := &Config{WatchLabel: "invalid"}
	_, _, err := cfg.WatchLabelParts()
	if err == nil {
		t.Fatal("expected error for invalid label")
	}
}

func TestProfilingLabelParts(t *testing.T) {
	cfg := &Config{ProfilingLabel: "vram-profiling=true"}
	k, v, err := cfg.ProfilingLabelParts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k != "vram-profiling" || v != "true" {
		t.Errorf("got (%q, %q), want (vram-profiling, true)", k, v)
	}
}

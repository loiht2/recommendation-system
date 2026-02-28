package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration for the profiler.
type Config struct {
	// Prometheus
	PrometheusURL string

	// The vGPU metric name to query for peak VRAM (used with max_over_time)
	VRAMMetric string

	// Pod label selector to watch (e.g. "job-type=training")
	WatchLabel string

	// PostgreSQL
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	// Pre-run profiling
	ProfilingLabel    string        // label selector for pre-run pods (e.g. "vram-profiling=true")
	ProfilingDuration time.Duration // how long to profile after pod reaches Running

	// Health endpoint
	HealthPort string // e.g. ":8081"
}

// WatchLabelParts splits WatchLabel "key=value" into (key, value).
func (c *Config) WatchLabelParts() (string, string, error) {
	return splitLabel(c.WatchLabel, "WATCH_LABEL")
}

// ProfilingLabelParts splits ProfilingLabel "key=value" into (key, value).
func (c *Config) ProfilingLabelParts() (string, string, error) {
	return splitLabel(c.ProfilingLabel, "PROFILING_LABEL")
}

func splitLabel(label, field string) (string, string, error) {
	parts := strings.SplitN(label, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid %s %q, expected key=value", field, label)
	}
	return parts[0], parts[1], nil
}

// Load reads configuration from environment variables.
//
// From ConfigMap (non-sensitive):
//
//	PROMETHEUS_URL  – Prometheus address
//	VRAM_METRIC     – metric name for peak VRAM query
//	WATCH_LABEL     – pod label selector (key=value)
//	DB_HOST         – PostgreSQL host
//	DB_PORT         – PostgreSQL port
//	DB_NAME         – database name
//	DB_SSLMODE      – SSL mode
//
// From Secret (sensitive):
//
//	DB_USER         – database user
//	DB_PASSWORD     – database password
func Load() *Config {
	return &Config{
		PrometheusURL:     envOrDefault("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.prometheus.svc.cluster.local:9090"),
		VRAMMetric:        envOrDefault("VRAM_METRIC", "vGPU_device_memory_usage_real_in_MiB"),
		WatchLabel:        envOrDefault("WATCH_LABEL", "job-type=training"),
		DBHost:            envOrDefault("DB_HOST", "profiler-postgresql.profiler.svc.cluster.local"),
		DBPort:            envIntOrDefault("DB_PORT", 5432),
		DBUser:            envOrDefault("DB_USER", "profiler"),
		DBPassword:        envOrDefault("DB_PASSWORD", "profiler"),
		DBName:            envOrDefault("DB_NAME", "profiler"),
		DBSSLMode:         envOrDefault("DB_SSLMODE", "disable"),
		ProfilingLabel:    envOrDefault("PROFILING_LABEL", "vram-profiling=true"),
		ProfilingDuration: envDurationOrDefault("PROFILING_DURATION", 45*time.Second),
		HealthPort:        envOrDefault("HEALTH_PORT", ":8081"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

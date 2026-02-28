package config

import (
	"os"
	"strconv"
)

// Config holds all configuration for the recommender API.
type Config struct {
	// HTTP server
	ListenAddr string

	// PostgreSQL (same database as the profiler)
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	// VRAM recommendation
	SafetyBufferPercent float64 // e.g. 10.0 means add 10% on top of peak

	// Docker registry credentials (optional, for private registries)
	RegistryUser     string
	RegistryPassword string
}

// Load reads configuration from environment variables.
func Load() *Config {
	return &Config{
		ListenAddr:          envOrDefault("LISTEN_ADDR", ":8080"),
		DBHost:              envOrDefault("DB_HOST", "profiler-postgresql.profiler.svc.cluster.local"),
		DBPort:              envIntOrDefault("DB_PORT", 5432),
		DBUser:              envOrDefault("DB_USER", "profiler"),
		DBPassword:          envOrDefault("DB_PASSWORD", "profiler"),
		DBName:              envOrDefault("DB_NAME", "profiler"),
		DBSSLMode:           envOrDefault("DB_SSLMODE", "disable"),
		SafetyBufferPercent: envFloatOrDefault("SAFETY_BUFFER_PERCENT", 10.0),
		RegistryUser:        os.Getenv("REGISTRY_USER"),
		RegistryPassword:    os.Getenv("REGISTRY_PASSWORD"),
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

func envFloatOrDefault(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

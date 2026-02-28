package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loihoangthanh1411/recommender/internal/config"
	"github.com/loihoangthanh1411/recommender/internal/db"
	"github.com/loihoangthanh1411/recommender/internal/handler"
	"github.com/loihoangthanh1411/recommender/internal/registry"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting recommender API...")

	cfg := config.Load()

	log.Printf("Listen address      : %s", cfg.ListenAddr)
	log.Printf("DB host             : %s:%d/%s", cfg.DBHost, cfg.DBPort, cfg.DBName)
	log.Printf("Safety buffer       : %.1f%%", cfg.SafetyBufferPercent)

	// ---- PostgreSQL store (read-only, shared DB with profiler) ----
	store, err := db.NewStore(cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode)
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	defer store.Close()
	log.Println("Connected to PostgreSQL")

	// ---- Registry resolver ----
	resolver := registry.NewResolver(cfg.RegistryUser, cfg.RegistryPassword)

	// ---- HTTP server ----
	recommendHandler := handler.NewRecommendHandler(store, resolver, cfg.SafetyBufferPercent)

	mux := http.NewServeMux()
	mux.Handle("/recommend", recommendHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// ---- Graceful shutdown ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %s, shutting down...", sig)
		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}()

	log.Printf("Listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}

	<-ctx.Done()
	log.Println("Recommender stopped.")
}

package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
	prom "github.com/loihoangthanh1411/profiler/internal/prometheus"
)

// Store wraps the PostgreSQL connection.
type Store struct {
	db *sql.DB
}

// NewStore opens a connection to PostgreSQL and ensures the schema exists.
func NewStore(host string, port int, user, password, dbname, sslmode string) (*Store, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		host, port, user, password, dbname, sslmode,
	)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return s, nil
}

// Ping checks whether the database connection is alive.
func (s *Store) Ping() error {
	return s.db.Ping()
}

// migrate creates the tables if they don't already exist.
// All label columns mirror the Prometheus metric labels for
// vGPU_device_memory_usage_real_in_MiB.
func (s *Store) migrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS vgpu_peak_usage (
		id                BIGSERIAL        PRIMARY KEY,
		pod_name          TEXT             NOT NULL,
		pod_namespace     TEXT             NOT NULL,
		pod_uid           TEXT             NOT NULL DEFAULT '',
		container_name    TEXT             NOT NULL DEFAULT '',
		device_uuid       TEXT             NOT NULL DEFAULT '',
		device_type       TEXT             NOT NULL DEFAULT '',
		vdevice_id        TEXT             NOT NULL DEFAULT '',
		image             TEXT             NOT NULL DEFAULT '',
		image_id          TEXT             NOT NULL DEFAULT '',
		metric_name       TEXT             NOT NULL,
		peak_value_mib    DOUBLE PRECISION NOT NULL,
		pod_start_time    TIMESTAMPTZ      NOT NULL,
		pod_end_time      TIMESTAMPTZ      NOT NULL,
		duration_seconds  DOUBLE PRECISION NOT NULL,
		pod_phase         TEXT             NOT NULL,
		created_at        TIMESTAMPTZ      NOT NULL DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_vgpu_peak_pod
		ON vgpu_peak_usage (pod_name, pod_namespace);
	CREATE INDEX IF NOT EXISTS idx_vgpu_peak_created
		ON vgpu_peak_usage (created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_vgpu_peak_image_id
		ON vgpu_peak_usage (image_id) WHERE image_id != '';

	CREATE TABLE IF NOT EXISTS vgpu_prerun_profile (
		id                BIGSERIAL        PRIMARY KEY,
		pod_name          TEXT             NOT NULL,
		pod_namespace     TEXT             NOT NULL,
		pod_uid           TEXT             NOT NULL DEFAULT '',
		container_name    TEXT             NOT NULL DEFAULT '',
		device_uuid       TEXT             NOT NULL DEFAULT '',
		device_type       TEXT             NOT NULL DEFAULT '',
		vdevice_id        TEXT             NOT NULL DEFAULT '',
		image             TEXT             NOT NULL DEFAULT '',
		image_id          TEXT             NOT NULL DEFAULT '',
		metric_name       TEXT             NOT NULL,
		peak_value_mib    DOUBLE PRECISION NOT NULL,
		profile_start     TIMESTAMPTZ      NOT NULL,
		profile_end       TIMESTAMPTZ      NOT NULL,
		duration_seconds  DOUBLE PRECISION NOT NULL,
		created_at        TIMESTAMPTZ      NOT NULL DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_vgpu_prerun_pod
		ON vgpu_prerun_profile (pod_name, pod_namespace);
	CREATE INDEX IF NOT EXISTS idx_vgpu_prerun_created
		ON vgpu_prerun_profile (created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_vgpu_prerun_image_id
		ON vgpu_prerun_profile (image_id) WHERE image_id != '';
	`
	_, err := s.db.Exec(query)
	return err
}

// InsertPeakUsage inserts peak VRAM results for a completed training pod.
func (s *Store) InsertPeakUsage(results []prom.PeakVRAMResult, podStart, podEnd time.Time, durationSec float64, podPhase string) error {
	if len(results) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("ERROR: rollback failed: %v", rbErr)
			}
		}
	}()

	stmt, err := tx.Prepare(`
		INSERT INTO vgpu_peak_usage
			(pod_name, pod_namespace, pod_uid, container_name, device_uuid,
			 device_type, vdevice_id, image, image_id,
			 metric_name, peak_value_mib,
			 pod_start_time, pod_end_time, duration_seconds, pod_phase)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`)
	if err != nil {
		return fmt.Errorf("preparing statement: %w", err)
	}
	defer stmt.Close()

	for _, r := range results {
		_, err = stmt.Exec(
			r.PodName, r.PodNamespace, r.PodUID,
			r.ContainerName, r.DeviceUUID, r.DeviceType, r.VDeviceID,
			r.Image, r.ImageID,
			r.MetricName, r.PeakValueMiB,
			podStart, podEnd, durationSec, podPhase,
		)
		if err != nil {
			return fmt.Errorf("inserting peak usage for %s/%s: %w", r.PodNamespace, r.PodName, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// InsertPreRunProfile inserts peak VRAM results from a pre-run profiling session.
func (s *Store) InsertPreRunProfile(results []prom.PeakVRAMResult, profileStart, profileEnd time.Time, durationSec float64) error {
	if len(results) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("ERROR: rollback failed: %v", rbErr)
			}
		}
	}()

	stmt, err := tx.Prepare(`
		INSERT INTO vgpu_prerun_profile
			(pod_name, pod_namespace, pod_uid, container_name, device_uuid,
			 device_type, vdevice_id, image, image_id,
			 metric_name, peak_value_mib,
			 profile_start, profile_end, duration_seconds)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`)
	if err != nil {
		return fmt.Errorf("preparing statement: %w", err)
	}
	defer stmt.Close()

	for _, r := range results {
		_, err = stmt.Exec(
			r.PodName, r.PodNamespace, r.PodUID,
			r.ContainerName, r.DeviceUUID, r.DeviceType, r.VDeviceID,
			r.Image, r.ImageID,
			r.MetricName, r.PeakValueMiB,
			profileStart, profileEnd, durationSec,
		)
		if err != nil {
			return fmt.Errorf("inserting pre-run profile for %s/%s: %w", r.PodNamespace, r.PodName, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

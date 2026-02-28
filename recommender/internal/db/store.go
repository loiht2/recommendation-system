package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// Store wraps the PostgreSQL connection for read-only queries.
type Store struct {
	db *sql.DB
}

// NewStore opens a connection to the profiler's PostgreSQL database.
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

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// VRAMRecord holds one row of VRAM peak data.
type VRAMRecord struct {
	ID           int64
	PodName      string
	PodNamespace string
	PodUID       string
	Container    string
	DeviceUUID   string
	DeviceType   string
	VDeviceID    string
	Image        string
	ImageID      string
	MetricName   string
	PeakValueMiB float64
	StartTime    time.Time
	EndTime      time.Time
	DurationSec  float64
	CreatedAt    time.Time
	Source       string // "peak_usage" or "prerun_profile"
}

// QueryByImageID searches both vgpu_peak_usage and vgpu_prerun_profile
// tables for records whose image_id ends with the given digest (sha256:...).
// Results are ordered by created_at DESC (most recent first).
func (s *Store) QueryByImageID(digest string) ([]VRAMRecord, error) {
	query := `
		(
			SELECT id, pod_name, pod_namespace, pod_uid, container_name,
				   device_uuid, device_type, vdevice_id, image, image_id,
				   metric_name, peak_value_mib,
				   pod_start_time AS start_time, pod_end_time AS end_time,
				   duration_seconds, created_at,
				   'peak_usage' AS source
			FROM vgpu_peak_usage
			WHERE image_id LIKE '%' || $1
		)
		UNION ALL
		(
			SELECT id, pod_name, pod_namespace, pod_uid, container_name,
				   device_uuid, device_type, vdevice_id, image, image_id,
				   metric_name, peak_value_mib,
				   profile_start AS start_time, profile_end AS end_time,
				   duration_seconds, created_at,
				   'prerun_profile' AS source
			FROM vgpu_prerun_profile
			WHERE image_id LIKE '%' || $1
		)
		ORDER BY created_at DESC
	`

	rows, err := s.db.Query(query, digest)
	if err != nil {
		return nil, fmt.Errorf("querying by image id: %w", err)
	}
	defer rows.Close()

	var records []VRAMRecord
	for rows.Next() {
		var r VRAMRecord
		if err := rows.Scan(
			&r.ID, &r.PodName, &r.PodNamespace, &r.PodUID, &r.Container,
			&r.DeviceUUID, &r.DeviceType, &r.VDeviceID, &r.Image, &r.ImageID,
			&r.MetricName, &r.PeakValueMiB,
			&r.StartTime, &r.EndTime, &r.DurationSec,
			&r.CreatedAt, &r.Source,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		records = append(records, r)
	}

	return records, rows.Err()
}

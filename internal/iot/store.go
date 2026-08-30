package iot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Store persists the telemetry pipeline's output.
type Store struct {
	db *sql.DB
}

func NewStore(database *sql.DB) *Store {
	return &Store{db: database}
}

// SaveTelemetry records one event, registering the device on first sight.
func (s *Store) SaveTelemetry(ctx context.Context, event TelemetryEvent) error {
	if event.DeviceID == "" {
		return fmt.Errorf("telemetry is missing device_id")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.Type == "" {
		event.Type = "reading"
	}

	data, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("telemetry data is not serialisable: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices (device_id) VALUES ($1)
		 ON CONFLICT (device_id) DO UPDATE SET last_seen = NOW()`,
		event.DeviceID,
	); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO telemetry (device_id, type, data, recorded_at) VALUES ($1, $2, $3, $4)`,
		event.DeviceID, event.Type, data, event.Timestamp,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// TouchDevice records that a device was seen, without storing a reading.
func (s *Store) TouchDevice(ctx context.Context, deviceID string) error {
	if deviceID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (device_id) VALUES ($1)
		 ON CONFLICT (device_id) DO UPDATE SET last_seen = NOW()`,
		deviceID,
	)
	return err
}

// RecentTelemetry returns a device's latest readings, newest first.
func (s *Store) RecentTelemetry(ctx context.Context, deviceID string, limit int) ([]TelemetryEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT device_id, type, data, recorded_at FROM telemetry
		 WHERE device_id = $1 ORDER BY recorded_at DESC LIMIT $2`,
		deviceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []TelemetryEvent{}
	for rows.Next() {
		var e TelemetryEvent
		var raw []byte
		if err := rows.Scan(&e.DeviceID, &e.Type, &raw, &e.Timestamp); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &e.Data); err != nil {
			return nil, err
		}
		events = append(events, e)
	}

	return events, rows.Err()
}

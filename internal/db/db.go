package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

// connectTimeout bounds how long startup waits for the database. On a platform
// deploy the app frequently boots before Postgres is accepting connections.
const connectTimeout = 60 * time.Second

func Connect(url string) (*sql.DB, error) {
	database, err := sql.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	deadline := time.Now().Add(connectTimeout)
	delay := 500 * time.Millisecond
	var lastErr error

	for attempt := 1; ; attempt++ {
		if err := database.Ping(); err == nil {
			break
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			database.Close()
			return nil, fmt.Errorf("database unreachable after %s: %w", connectTimeout, lastErr)
		}

		log.Printf("database not ready (attempt %d): %v; retrying in %s", attempt, lastErr, delay)
		time.Sleep(delay)
		if delay < 5*time.Second {
			delay *= 2
		}
	}

	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)

	return database, nil
}

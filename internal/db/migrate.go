package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"sort"
)

// Migrate applies every embedded .sql file that has not been applied yet, in
// filename order, recording each in schema_migrations. Files are expected to be
// idempotent so a database created by an earlier initdb-based deploy can be
// adopted without a manual baseline.
func Migrate(database *sql.DB, files fs.FS) error {
	if _, err := database.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(255) PRIMARY KEY,
			applied_at TIMESTAMPTZ DEFAULT NOW()
		)`,
	); err != nil {
		return fmt.Errorf("failed to create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(database)
	if err != nil {
		return err
	}

	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}

		body, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}

		if err := applyOne(database, name, string(body)); err != nil {
			return fmt.Errorf("migration %s failed: %w", name, err)
		}
		log.Printf("applied migration %s", name)
	}

	return nil
}

func appliedVersions(database *sql.DB) (map[string]bool, error) {
	rows, err := database.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// applyOne runs a migration and records it in the same transaction, so a
// failure part-way leaves neither the change nor the bookkeeping behind.
func applyOne(database *sql.DB, name, body string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(body); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
		return err
	}

	return tx.Commit()
}

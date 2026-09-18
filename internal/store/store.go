// Package store handles SQLite access and schema migrations.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (or creates) the SQLite database at dbPath and applies any
// pending migrations.
func Open(dbPath string) (*sql.DB, error) {
	// busy_timeout: without it, two SQLite connections writing around the
	// same time fail with SQLITE_BUSY immediately instead of one waiting for
	// the other.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}

	// SQLite only ever allows one writer at a time; letting database/sql's
	// pool open more than one connection just means concurrent writers
	// (MQTT collector, Beszel/HA pollers, rollup) contend for that single
	// writer slot across separate OS-level file locks instead of queuing
	// in-process. busy_timeout(5000) alone didn't fully eliminate the
	// resulting SQLITE_BUSY errors in production (a lock held a little
	// too long — e.g. during a WAL checkpoint — still lost the race).
	// Capping the pool at one connection removes the multi-connection
	// contention entirely: every access serializes through Go's own
	// (fast, in-process) connection checkout instead of SQLite's
	// cross-connection busy-retry loop.
	//
	// This makes a correctness assumption the rest of the codebase must
	// hold: no query may keep a *sql.Rows open while issuing another
	// query/exec on the same *sql.DB, or that second call blocks forever
	// waiting for the only connection, which the still-open Rows is
	// pinning (see rollup.rollupResources and api.summaryFor, both fixed
	// to fully buffer a SELECT's results before writing/querying again).
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

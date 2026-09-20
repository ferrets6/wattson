package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	tables := []string{
		"power_samples", "power_hourly", "power_minutely",
		"resource_samples", "resource_hourly", "resource_minutely",
		"attribution_buckets", "pricing_periods", "pun_prices",
	}
	for _, tbl := range tables {
		if _, err := db.Exec("SELECT * FROM " + tbl + " LIMIT 0"); err != nil {
			t.Errorf("table %s not created: %v", tbl, err)
		}
	}
	db.Close()

	// reopening must not fail (already-applied migrations are skipped)
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
}

// TestConcurrentWritesDoNotFailWithSQLiteBusy reproduces the shape of the
// production bug: several goroutines (standing in for the MQTT collector,
// the Beszel/HA pollers, and the rollup job) writing to the same DB at once.
// Before busy_timeout + a capped connection pool, this intermittently failed
// with "database is locked (5) (SQLITE_BUSY)".
func TestConcurrentWritesDoNotFailWithSQLiteBusy(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	const writers = 8
	const writesEach = 100
	errCh := make(chan error, writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			for i := 0; i < writesEach; i++ {
				ts := int64(w*writesEach + i)
				_, err := db.Exec(
					`INSERT OR REPLACE INTO power_samples (ts, watts, cumulative_kwh, voltage, current) VALUES (?, ?, ?, ?, ?)`,
					ts, 1.0, 1.0, 230.0, 1.0,
				)
				if err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(w)
	}

	// A hung run here means a deadlock (a connection permanently pinned by
	// an unclosed Rows elsewhere), not just a slow one — fail fast instead
	// of waiting out the whole test binary's default 10-minute timeout.
	deadline := time.After(15 * time.Second)
	for i := 0; i < writers; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("concurrent write failed: %v", err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for concurrent writers: possible deadlock or busy_timeout starvation")
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM power_samples`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != writers*writesEach {
		t.Errorf("power_samples count = %d, want %d", count, writers*writesEach)
	}
}

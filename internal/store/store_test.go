package store

import (
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	tables := []string{
		"power_samples", "power_hourly",
		"resource_samples", "resource_hourly",
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

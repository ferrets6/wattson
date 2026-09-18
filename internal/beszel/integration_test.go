package beszel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/ferrets6/wattson/internal/store"
)

// TestLiveConnection never runs automatically (not in CI, not in a local
// `go test ./...`, since .env holds real credentials): it requires the
// explicit opt-in RUN_BESZEL_LIVE_TEST=1 and a reachable real Beszel (e.g.
// via an SSH tunnel). Usage:
//
//	RUN_BESZEL_LIVE_TEST=1 BESZEL_URL=http://localhost:18090 go test ./internal/beszel/ -run TestLiveConnection -v
func TestLiveConnection(t *testing.T) {
	if os.Getenv("RUN_BESZEL_LIVE_TEST") == "" {
		t.Skip("RUN_BESZEL_LIVE_TEST not set, skipping the live test (see comment above)")
	}
	_ = godotenv.Load("../../.env")

	email := os.Getenv("BESZEL_ADMIN_EMAIL")
	if email == "" {
		t.Skip("BESZEL_ADMIN_EMAIL not set, skipping the live test")
	}

	c := New(nil, Config{
		URL:           os.Getenv("BESZEL_URL"),
		AdminEmail:    email,
		AdminPassword: os.Getenv("BESZEL_ADMIN_PASSWORD"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	names, err := c.CheckConnection(ctx)
	if err != nil {
		t.Fatalf("connecting to Beszel failed: %v", err)
	}
	t.Logf("systems found: %v", names)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	c.db = db

	c.pollOnce(ctx)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM resource_samples`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("rows written to resource_samples: %d", n)
	if n == 0 {
		t.Error("no row written by a real poll")
	}

	rows, _ := db.Query(`SELECT container, cpu_pct, mem_used FROM resource_samples`)
	defer rows.Close()
	for rows.Next() {
		var container string
		var cpu, mem float64
		rows.Scan(&container, &cpu, &mem)
		t.Logf("  %-20s cpu=%.2f mem=%.2f", container, cpu, mem)
	}
}

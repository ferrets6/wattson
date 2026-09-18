package homeassistant

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"

	"github.com/ferrets6/wattson/internal/store"
)

// TestLiveConnection never runs automatically (not in CI, not in a local
// `go test ./...`, since .env holds a real token): it requires the explicit
// opt-in RUN_HA_LIVE_TEST=1. Usage:
//
//	RUN_HA_LIVE_TEST=1 go test ./internal/homeassistant/ -run TestLiveConnection -v
func TestLiveConnection(t *testing.T) {
	if os.Getenv("RUN_HA_LIVE_TEST") == "" {
		t.Skip("RUN_HA_LIVE_TEST not set, skipping the live test")
	}
	_ = godotenv.Load("../../.env")

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	c := New(db, Config{
		URL:         os.Getenv("HA_URL"),
		Token:       os.Getenv("HA_TOKEN"),
		PunEntityID: os.Getenv("HA_PUN_ENTITY_ID"),
	})
	c.pollOnce(context.Background())

	var pun float64
	if err := db.QueryRow(`SELECT pun_eur_kwh FROM pun_prices`).Scan(&pun); err != nil {
		t.Fatalf("no row written: %v", err)
	}
	t.Logf("PUN read: %v EUR/kWh", pun)
}

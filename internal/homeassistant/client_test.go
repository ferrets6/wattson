package homeassistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ferrets6/wattson/internal/store"
)

func TestPollOnceWritesPunPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states/sensor.pun_orario" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"state":"0.1234"}`))
	}))
	defer srv.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	c := New(db, Config{URL: srv.URL, Token: "test-token", PunEntityID: "sensor.pun_orario"})
	c.pollOnce(context.Background())

	var pun float64
	if err := db.QueryRow(`SELECT pun_eur_kwh FROM pun_prices`).Scan(&pun); err != nil {
		t.Fatalf("query: %v", err)
	}
	if pun != 0.1234 {
		t.Errorf("pun_eur_kwh = %v, want 0.1234", pun)
	}
}

func TestPollOnceSkipsUnavailableEntity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"unavailable"}`))
	}))
	defer srv.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	c := New(db, Config{URL: srv.URL, Token: "t", PunEntityID: "sensor.pun_orario"})
	c.pollOnce(context.Background())

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pun_prices`).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 rows for an unavailable entity, got %d", n)
	}
}

func TestPollOnceSkipsWhenEntityMissing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	c := New(db, Config{URL: "http://unused", Token: "t", PunEntityID: ""})
	c.pollOnce(context.Background())

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pun_prices`).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 rows without an entity id, got %d", n)
	}
}

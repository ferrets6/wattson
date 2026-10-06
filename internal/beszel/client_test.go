package beszel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ferrets6/wattson/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/collections/_superusers/auth-with-password", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	})
	mux.HandleFunc("/api/collections/systems/records", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]string{{"id": "sys1", "name": "nas"}},
		})
	})
	mux.HandleFunc("/api/collections/system_stats/records", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"stats": map[string]any{"cpu": 12.5, "mu": 3.2, "b": []uint64{100, 200}, "dio": []uint64{10, 20}},
			}},
		})
	})
	mux.HandleFunc("/api/collections/container_stats/records", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"stats": []map[string]any{
					{"n": "beszel", "c": 1.5, "m": 40.0, "b": []uint64{5, 6}},
					{"n": "vaultwarden", "c": 0.2, "m": 20.0, "b": []uint64{1, 2}},
				},
			}},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPollOnceWritesHostAndContainerSamples(t *testing.T) {
	srv := newTestServer(t)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	c := New(db, Config{URL: srv.URL, AdminEmail: "a@a.it", AdminPassword: "pw"})
	c.pollOnce(context.Background())

	rows, err := db.Query(`SELECT container, cpu_pct, mem_used, net_sent_bytes, net_recv_bytes, disk_io_bytes FROM resource_samples ORDER BY container`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		container            string
		cpu, mem, sent, recv float64
		disk                 *float64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.container, &r.cpu, &r.mem, &r.sent, &r.recv, &r.disk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 rows (host + 2 containers), got %d: %+v", len(got), got)
	}

	byName := map[string]row{}
	for _, r := range got {
		byName[r.container] = r
	}

	host, ok := byName["__host__"]
	if !ok {
		t.Fatal("missing __host__ row")
	}
	if host.cpu != 12.5 || host.mem != 3.2 || host.disk == nil || *host.disk != 30 {
		t.Errorf("wrong host sample: %+v", host)
	}

	beszel, ok := byName["beszel"]
	if !ok {
		t.Fatal("missing 'beszel' container row")
	}
	if beszel.cpu != 1.5 || beszel.mem != 40.0 || beszel.disk != nil {
		t.Errorf("wrong container sample: %+v", beszel)
	}
}

func TestPollOnceRelogsInWhenSystemsListComesBackEmpty(t *testing.T) {
	logins := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collections/_superusers/auth-with-password", func(w http.ResponseWriter, r *http.Request) {
		logins++
		json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
	})
	mux.HandleFunc("/api/collections/systems/records", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}}) // expired token: 200 + empty
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	c := New(db, Config{URL: srv.URL, AdminEmail: "a@a.it", AdminPassword: "pw"})
	c.pollOnce(context.Background())
	c.pollOnce(context.Background())
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (empty list must force a fresh login)", logins)
	}
}

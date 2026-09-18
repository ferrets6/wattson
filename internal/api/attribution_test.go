package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAttributionHandlerAggregatesAndExcludesBaseline(t *testing.T) {
	db, h := newTestServer(t)
	bucketStart := time.Now().Add(-time.Hour).Unix()

	db.Exec(`INSERT INTO attribution_buckets (bucket_start, bucket_end, container, category, watts_allocated) VALUES (?, ?, '__baseline__', 'system', 30)`, bucketStart, bucketStart+3600)
	db.Exec(`INSERT INTO attribution_buckets (bucket_start, bucket_end, container, category, watts_allocated) VALUES (?, ?, 'immich_server', 'user', 20)`, bucketStart, bucketStart+3600)
	db.Exec(`INSERT INTO attribution_buckets (bucket_start, bucket_end, container, category, watts_allocated) VALUES (?, ?, 'beszel', 'system', 5)`, bucketStart, bucketStart+3600)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/power/attribution?from=%d&to=%d", bucketStart-10, bucketStart+3700), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp attributionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.ByCategory) != 2 {
		t.Fatalf("expected 2 categories (system, user), got %d: %+v", len(resp.ByCategory), resp.ByCategory)
	}
	var systemTotal float64
	for _, c := range resp.ByCategory {
		if c.Category == "system" {
			systemTotal = c.KwhAllocated
		}
	}
	if systemTotal != 0.035 {
		t.Errorf("system total = %v, want 0.035 kWh (30+5 Wh = 0.035 kWh)", systemTotal)
	}

	if len(resp.ByContainer) != 2 {
		t.Fatalf("expected 2 containers (baseline excluded), got %d: %+v", len(resp.ByContainer), resp.ByContainer)
	}
	for _, c := range resp.ByContainer {
		if c.Container == "__baseline__" {
			t.Error("__baseline__ must not appear in by_container")
		}
	}
}

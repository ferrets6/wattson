package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ferrets6/wattson/internal/pricing"
	"github.com/ferrets6/wattson/internal/store"
)

func newTestServer(t *testing.T) (*sql.DB, http.Handler) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mux := http.NewServeMux()
	RegisterRoutes(mux, db, 0.10)
	return db, mux
}

func TestPowerCurrentNotFoundWithoutData(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/power/current", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPowerCurrentReturnsLatestAndStaleFlag(t *testing.T) {
	db, h := newTestServer(t)
	db.Exec(`INSERT INTO power_samples (ts, watts, cumulative_kwh, voltage, current) VALUES (?, ?, ?, ?, ?)`,
		time.Now().Add(-time.Hour).Unix(), 42.0, 1.0, 230.0, 0.18)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/power/current", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp powerCurrentResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Watts != 42.0 || !resp.Stale {
		t.Errorf("unexpected response: %+v (expected stale=true, 1h-old sample)", resp)
	}
}

func TestPowerHistoryRequiresFromTo(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/power/history", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without from/to", rec.Code)
	}
}

func TestPowerHistoryReturnsPointsInRange(t *testing.T) {
	db, h := newTestServer(t)
	now := time.Now().Unix()
	db.Exec(`INSERT INTO power_hourly (bucket_start, watts_avg, watts_min, watts_max, kwh, voltage_avg, current_avg, sample_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		now-3600, 50.0, 40.0, 60.0, 0.05, 230, 1, 100)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/power/history?from=%d&to=%d", now-7200, now), nil)
	h.ServeHTTP(rec, req)

	var points []powerHourlyPoint
	json.NewDecoder(rec.Body).Decode(&points)
	if len(points) != 1 || points[0].WattsAvg != 50.0 {
		t.Errorf("unexpected points: %+v", points)
	}
}

func TestPricingPeriodsCrudAndOverlapRejection(t *testing.T) {
	_, h := newTestServer(t)

	body, _ := json.Marshal(periodInput{ValidFrom: "2026-01-01", ValidUntil: "2026-01-31", Mode: pricing.ModeFixedOverride, FixedPrice: 0.25})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/pricing/periods", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created map[string]int64
	json.NewDecoder(rec.Body).Decode(&created)
	id := created["id"]

	// overlapping: must be rejected
	overlap, _ := json.Marshal(periodInput{ValidFrom: "2026-01-15", ValidUntil: "2026-02-15", Mode: pricing.ModeSpreadOverride, SpreadValue: 0.05})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/pricing/periods", bytes.NewReader(overlap)))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("overlapping create status = %d, want 400", rec2.Code)
	}

	// list
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/v1/pricing/periods", nil))
	var periods []pricing.Period
	json.NewDecoder(rec3.Body).Decode(&periods)
	if len(periods) != 1 {
		t.Fatalf("expected 1 period after the overlap rejection, got %d", len(periods))
	}

	// update
	upd, _ := json.Marshal(periodInput{ValidFrom: "2026-01-01", ValidUntil: "2026-01-31", Mode: pricing.ModeFixedOverride, FixedPrice: 0.30})
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/pricing/periods/%d", id), bytes.NewReader(upd)))
	if rec4.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, want 204: %s", rec4.Code, rec4.Body.String())
	}

	// delete
	rec5 := httptest.NewRecorder()
	h.ServeHTTP(rec5, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/pricing/periods/%d", id), nil))
	if rec5.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec5.Code)
	}
}

func TestPricingPreviewComputesHypotheticalCost(t *testing.T) {
	db, h := newTestServer(t)
	loc := pricing.RomeLocation()
	day := time.Date(2026, 3, 10, 12, 0, 0, 0, loc)
	db.Exec(`INSERT INTO power_hourly (bucket_start, watts_avg, watts_min, watts_max, kwh, voltage_avg, current_avg, sample_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		day.Unix(), 1000.0, 1000.0, 1000.0, 2.0, 230, 4, 3600)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pricing/preview?from=2026-03-10&to=2026-03-10&fixed_price=0.20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var result summaryPeriod
	json.NewDecoder(rec.Body).Decode(&result)
	if result.KWh != 2.0 || result.CostEur != 0.40 || !result.Complete {
		t.Errorf("unexpected preview: %+v (expected kwh=2.0 cost=0.40)", result)
	}
}

func TestPricingPreviewRequiresModeParam(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pricing/preview?from=2026-03-10&to=2026-03-10", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without spread/fixed_price", rec.Code)
	}
}

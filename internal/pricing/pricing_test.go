package pricing

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ferrets6/wattson/internal/store"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func romeAt(dateTime string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", dateTime, romeLocation)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCreatePeriodRejectsOverlap(t *testing.T) {
	db := newTestDB(t)

	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-01-01", ValidUntil: "2026-01-31", Mode: ModeFixedOverride, FixedPrice: 0.25}); err != nil {
		t.Fatalf("first period: %v", err)
	}

	_, err := CreatePeriod(db, Period{ValidFrom: "2026-01-15", ValidUntil: "2026-02-15", Mode: ModeSpreadOverride, SpreadValue: 0.08})
	if err == nil {
		t.Fatal("expected an overlap error, got none")
	}

	// not overlapping: right after the first one ends (Feb 1st)
	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-02-01", Mode: ModeSpreadOverride, SpreadValue: 0.08}); err != nil {
		t.Fatalf("non-overlapping period rejected: %v", err)
	}
}

func TestCreatePeriodRejectsOverlapWithOpenEnded(t *testing.T) {
	db := newTestDB(t)

	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-01-01", Mode: ModeSpreadOverride, SpreadValue: 0.05}); err != nil {
		t.Fatalf("first period (no expiry): %v", err)
	}
	if _, err := CreatePeriod(db, Period{ValidFrom: "2027-01-01", Mode: ModeFixedOverride, FixedPrice: 0.3}); err == nil {
		t.Fatal("a future period must overlap an earlier open-ended one")
	}
}

func TestUpdatePeriodIgnoresSelfOverlap(t *testing.T) {
	db := newTestDB(t)

	id, err := CreatePeriod(db, Period{ValidFrom: "2026-01-01", ValidUntil: "2026-01-31", Mode: ModeFixedOverride, FixedPrice: 0.25})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	err = UpdatePeriod(db, Period{ID: id, ValidFrom: "2026-01-01", ValidUntil: "2026-01-31", Mode: ModeFixedOverride, FixedPrice: 0.30, Note: "updated"})
	if err != nil {
		t.Fatalf("updating over the same range must not fail on self-overlap: %v", err)
	}

	periods, err := ListPeriods(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(periods) != 1 || periods[0].FixedPrice != 0.30 || periods[0].Note != "updated" {
		t.Errorf("period not updated correctly: %+v", periods)
	}
}

func TestRejectsInvalidMode(t *testing.T) {
	db := newTestDB(t)
	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-01-01", Mode: "boh"}); err == nil {
		t.Fatal("expected an error for an invalid mode")
	}
}

// March 2026 is a concluded month relative to "now" (this session runs in
// September 2026): Resolve must use that month's *real* average, not the
// previous month's provisional estimate.

func TestResolveDefaultWhenNoPeriod(t *testing.T) {
	db := newTestDB(t)
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, romeAt("2026-03-05 12:00").Unix(), 0.10)
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, romeAt("2026-03-20 12:00").Unix(), 0.20)

	res, err := Resolve(db, romeAt("2026-03-10 12:30"), 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Complete || res.Provisional || res.Source != "default" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// March average = (0.10+0.20)/2 = 0.15, + default spread 0.10 = 0.25
	if diff := res.EurPerKwh - 0.25; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("price = %v, want 0.25 (monthly average 0.15 + 0.10 default)", res.EurPerKwh)
	}
}

func TestResolveSpreadOverride(t *testing.T) {
	db := newTestDB(t)
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, romeAt("2026-03-10 12:00").Unix(), 0.20)
	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-03-01", ValidUntil: "2026-03-31", Mode: ModeSpreadOverride, SpreadValue: 0.05}); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := Resolve(db, romeAt("2026-03-10 12:30"), 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Complete || res.Provisional || res.Source != ModeSpreadOverride {
		t.Fatalf("unexpected result: %+v", res)
	}
	if diff := res.EurPerKwh - 0.25; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("price = %v, want 0.25 (monthly average 0.20 + period spread 0.05)", res.EurPerKwh)
	}
}

// Real user scenario: fresh install, no historical PUN data, but a manual
// period covering through the end of August with the last known bill's
// price. September (current month) should use that price as a provisional
// estimate, with no other logic needed.
func TestResolveCurrentMonthBootstrapsFromManualPeriod(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	endOfAugust := startOfMonth(now).AddDate(0, -1, 0).AddDate(0, 1, 0).AddDate(0, 0, -1) // last day of the previous month

	if _, err := CreatePeriod(db, Period{
		ValidFrom: "2020-01-01", ValidUntil: endOfAugust.Format("2006-01-02"),
		Mode: ModeFixedOverride, FixedPrice: 0.27, Note: "last known bill",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := Resolve(db, now, 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Complete || !res.Provisional {
		t.Fatalf("expected Complete=true Provisional=true from the manual period, got: %+v", res)
	}
	if res.EurPerKwh != 0.27 {
		t.Errorf("price = %v, want 0.27 (the manual period's price, no spread added on top)", res.EurPerKwh)
	}
}

func TestResolveCurrentMonthIsProvisionalFromPreviousMonth(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	prevMonth := startOfMonth(now).AddDate(0, -1, 0).AddDate(0, 0, 4) // any day in the previous month
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, prevMonth.Unix(), 0.20)

	res, err := Resolve(db, now, 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Complete || !res.Provisional {
		t.Fatalf("expected Complete=true Provisional=true for the current month, got: %+v", res)
	}
	if diff := res.EurPerKwh - 0.30; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("price = %v, want 0.30 (previous month average 0.20 + 0.10 default)", res.EurPerKwh)
	}
}

func TestResolveCurrentMonthFallsBackToOwnPartialAverage(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	// no previous month, but a couple of samples already collected in the current month itself
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, startOfMonth(now).Add(time.Hour).Unix(), 0.10)
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, startOfMonth(now).Add(2*time.Hour).Unix(), 0.20)

	res, err := Resolve(db, now, 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Complete || !res.Provisional {
		t.Fatalf("expected Complete=true Provisional=true from the current month's partial average, got: %+v", res)
	}
	if diff := res.EurPerKwh - 0.25; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("price = %v, want 0.25 (partial average 0.15 + 0.10 default)", res.EurPerKwh)
	}
}

func TestResolveCurrentMonthIncompleteWithoutPreviousMonth(t *testing.T) {
	db := newTestDB(t)
	res, err := Resolve(db, time.Now(), 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Complete {
		t.Fatalf("expected Complete=false for the current month with no previous-month history, got: %+v", res)
	}
}

func TestResolveFixedOverrideIgnoresPun(t *testing.T) {
	db := newTestDB(t)
	// no pun_prices inserted: a fixed_override shouldn't need any
	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-03-01", ValidUntil: "2026-03-31", Mode: ModeFixedOverride, FixedPrice: 0.28}); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := Resolve(db, romeAt("2026-03-10 12:30"), 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Complete || res.Source != ModeFixedOverride || res.EurPerKwh != 0.28 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestResolveIncompleteWhenPunMissing(t *testing.T) {
	db := newTestDB(t)
	// no pun_prices, no period -> must report incomplete, never estimate
	res, err := Resolve(db, romeAt("2026-03-10 12:30"), 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Complete {
		t.Fatalf("expected Complete=false with no PUN data, got: %+v", res)
	}
}

func TestResolveOutsidePeriodUsesDefault(t *testing.T) {
	db := newTestDB(t)
	db.Exec(`INSERT INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`, romeAt("2026-04-10 12:00").Unix(), 0.18)
	if _, err := CreatePeriod(db, Period{ValidFrom: "2026-03-01", ValidUntil: "2026-03-31", Mode: ModeFixedOverride, FixedPrice: 0.28}); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := Resolve(db, romeAt("2026-04-10 12:30"), 0.10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// 0.18 pun + 0.10 default = 0.28, a numeric coincidence with the fixed
	// price above: check via Source, not EurPerKwh, to tell them apart.
	if res.Source != "default" {
		t.Errorf("expected source=default outside March's period, got %+v", res)
	}
}

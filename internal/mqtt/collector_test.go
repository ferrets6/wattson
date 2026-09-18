package mqtt

import (
	"path/filepath"
	"testing"

	"github.com/ferrets6/wattson/internal/store"
)

// realPayload was captured from the "Sonoff Dual Meter" Tasmota device via
// tele/tasmota/SENSOR.
const realPayload = `{"Time":"2026-09-18T11:19:21","ENERGY":{"TotalStartTime":"2026-09-16T00:00:00","Total":[2.813,0.254],"Yesterday":[1.530,0.125],"Today":[0.772,0.059],"Power":[52,5],"ApparentPower":[66,10],"ReactivePower":[40,9],"Factor":[0.79,0.50],"Voltage":235,"Current":[0.278,0.044],"BL09XX":{"Temperature":46.5}}}`

func TestParseLine1UsesOnlyFirstLine(t *testing.T) {
	watts, kwh, voltage, current, err := parseLine1([]byte(realPayload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if watts != 52 {
		t.Errorf("watts = %v, want 52 (not line 2's 5)", watts)
	}
	if kwh != 2.813 {
		t.Errorf("cumulativeKwh = %v, want 2.813", kwh)
	}
	if voltage != 235 {
		t.Errorf("voltage = %v, want 235", voltage)
	}
	if current != 0.278 {
		t.Errorf("current = %v, want 0.278 (not line 2's 0.044)", current)
	}
}

func TestParseLine1RejectsGarbage(t *testing.T) {
	if _, _, _, _, err := parseLine1([]byte(`{"foo":"bar"}`)); err == nil {
		t.Error("expected an error for a payload without ENERGY, got nil")
	}
	if _, _, _, _, err := parseLine1([]byte(`not json`)); err == nil {
		t.Error("expected an error for a non-JSON payload, got nil")
	}
}

func TestInsertSampleWritesRow(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := insertSample(db, 1700000000, 52, 2.813, 235, 0.278); err != nil {
		t.Fatalf("insertSample: %v", err)
	}

	var watts float64
	if err := db.QueryRow(`SELECT watts FROM power_samples WHERE ts = ?`, 1700000000).Scan(&watts); err != nil {
		t.Fatalf("query: %v", err)
	}
	if watts != 52 {
		t.Errorf("watts read back = %v, want 52", watts)
	}
}

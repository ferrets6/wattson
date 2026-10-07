package main

import (
	"testing"
	"time"
)

func TestSampleTimesSpreadsCoarseRecordsOverTheirHours(t *testing.T) {
	created := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC) // 120m record: covers 06:00–08:00
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	got := sampleTimes(created, "120m", since, until)
	want := []int64{
		time.Date(2026, 9, 25, 6, 30, 0, 0, time.UTC).Unix(),
		time.Date(2026, 9, 25, 7, 30, 0, 0, time.UTC).Unix(),
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("120m: got %v, want %v", got, want)
	}

	// until cuts the hours that already have live data.
	if got := sampleTimes(created, "120m", since, time.Date(2026, 9, 25, 7, 0, 0, 0, time.UTC)); len(got) != 1 {
		t.Errorf("until: got %v, want only the 06:30 hour", got)
	}
	// Sub-hour records keep their own timestamp.
	if got := sampleTimes(created, "20m", since, until); len(got) != 1 || got[0] != created.Unix() {
		t.Errorf("20m: got %v", got)
	}
}

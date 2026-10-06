package rollup

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ferrets6/wattson/internal/attribution"
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

func TestRollupBucketAggregatesPowerAndResources(t *testing.T) {
	db := newTestDB(t)

	// a full hour that started 2 hours ago, to be safely "past" and not the current partial hour
	bucketStart := floorToHour(time.Now().Add(-2 * time.Hour).Unix())

	insertPowerSample(t, db, bucketStart+60, 100, 1.0)
	insertPowerSample(t, db, bucketStart+600, 200, 1.05)
	insertPowerSample(t, db, bucketStart+1200, 150, 1.10)

	insertResourceSample(t, db, bucketStart+60, "immich_server", 40.0)
	insertResourceSample(t, db, bucketStart+60, "beszel", 1.0)
	insertResourceSample(t, db, bucketStart+60, "__host__", 3.0)
	insertResourceSample(t, db, bucketStart+600, "__host__", 7.0)

	// Ten earlier 50 W hours make the dynamic baseline (10th percentile) 50 W.
	for k := int64(1); k <= 10; k++ {
		db.Exec(`INSERT INTO power_hourly (bucket_start, watts_avg, watts_min, watts_max, kwh, voltage_avg, current_avg, sample_count) VALUES (?, 50, 50, 50, 0.05, 230, 1, 1)`, bucketStart-k*3600)
	}

	cfg := Config{BaselineWindow: 24 * time.Hour, BaselinePercentile: 0.1, Attribution: attribution.Config{
		Containers:      map[string]attribution.ContainerEntry{"immich_server": {Category: attribution.CategoryUser}},
		DefaultCategory: attribution.CategoryUnknown,
	}}

	if err := rollupBucket(db, cfg, bucketStart); err != nil {
		t.Fatalf("rollupBucket: %v", err)
	}

	var wattsAvg, kwh float64
	var count int
	if err := db.QueryRow(`SELECT watts_avg, kwh, sample_count FROM power_hourly WHERE bucket_start = ?`, bucketStart).Scan(&wattsAvg, &kwh, &count); err != nil {
		t.Fatalf("power_hourly: %v", err)
	}
	if count != 3 || wattsAvg != 150 {
		t.Errorf("wrong power_hourly: avg=%v count=%v (want 150, 3)", wattsAvg, count)
	}
	if diff := kwh - 0.10; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("kwh = %v, want 0.10", kwh)
	}

	var hostCpuAvg, hostCpuMin, hostCpuMax float64
	if err := db.QueryRow(`SELECT cpu_avg, cpu_min, cpu_max FROM resource_hourly WHERE bucket_start = ? AND container = '__host__'`, bucketStart).Scan(&hostCpuAvg, &hostCpuMin, &hostCpuMax); err != nil {
		t.Fatalf("resource_hourly host: %v", err)
	}
	if hostCpuAvg != 5.0 || hostCpuMin != 3.0 || hostCpuMax != 7.0 {
		t.Errorf("host cpu avg/min/max = %v/%v/%v, want 5.0/3.0/7.0", hostCpuAvg, hostCpuMin, hostCpuMax)
	}

	// Minute-level rollup rides along with the hourly one: the sample at
	// bucketStart+60 (watts=100) and the one at bucketStart+600 (watts=200)
	// land in different one-minute buckets and must not be averaged together.
	var minuteWatts float64
	if err := db.QueryRow(`SELECT watts_avg FROM power_minutely WHERE bucket_start = ?`, bucketStart+60).Scan(&minuteWatts); err != nil {
		t.Fatalf("power_minutely: %v", err)
	}
	if minuteWatts != 100 {
		t.Errorf("power_minutely[%d].watts_avg = %v, want 100", bucketStart+60, minuteWatts)
	}
	var minuteWatts2 float64
	if err := db.QueryRow(`SELECT watts_avg FROM power_minutely WHERE bucket_start = ?`, bucketStart+600).Scan(&minuteWatts2); err != nil {
		t.Fatalf("power_minutely: %v", err)
	}
	if minuteWatts2 != 200 {
		t.Errorf("power_minutely[%d].watts_avg = %v, want 200", bucketStart+600, minuteWatts2)
	}

	var minuteCpu float64
	if err := db.QueryRow(`SELECT cpu_avg FROM resource_minutely WHERE bucket_start = ? AND container = '__host__'`, bucketStart+600).Scan(&minuteCpu); err != nil {
		t.Fatalf("resource_minutely: %v", err)
	}
	if minuteCpu != 7.0 {
		t.Errorf("resource_minutely[%d].cpu_avg = %v, want 7.0", bucketStart+600, minuteCpu)
	}

	rows, err := db.Query(`SELECT container, category, watts_allocated FROM attribution_buckets WHERE bucket_start = ?`, bucketStart)
	if err != nil {
		t.Fatalf("attribution_buckets query: %v", err)
	}
	defer rows.Close()
	found := map[string][2]any{}
	for rows.Next() {
		var container, category string
		var watts float64
		rows.Scan(&container, &category, &watts)
		found[container] = [2]any{category, watts}
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 attribution rows, got %d: %+v", len(found), found)
	}
	if found["__baseline__"][0] != "system" || found["__baseline__"][1] != 50.0 {
		t.Errorf("wrong baseline row: %+v", found["__baseline__"])
	}
	if found["immich_server"][0] != "user" || found["immich_server"][1] != 100.0 {
		t.Errorf("wrong peak row (should be immich_server, not beszel/__host__): %+v", found["immich_server"])
	}
}

func TestRollupBucketNoDataIsANoop(t *testing.T) {
	db := newTestDB(t)
	bucketStart := floorToHour(time.Now().Add(-2 * time.Hour).Unix())

	if err := rollupBucket(db, Config{}, bucketStart); err != nil {
		t.Fatalf("rollupBucket on an empty bucket should not fail: %v", err)
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM power_hourly`).Scan(&n)
	if n != 0 {
		t.Errorf("expected no power_hourly row for a bucket with no samples, got %d", n)
	}
}

func TestPendingBucketsAdvancesEvenThroughGaps(t *testing.T) {
	db := newTestDB(t)

	base := floorToHour(time.Now().Add(-5 * time.Hour).Unix())
	insertPowerSample(t, db, base+60, 100, 1.0) // only the first hour has data, the rest is a "gap"

	cfg := Config{}
	cfg.applyDefaults()

	RunOnce(db, cfg)

	var cursor int64
	if err := db.QueryRow(`SELECT value FROM rollup_state WHERE key = 'last_bucket'`).Scan(&cursor); err != nil {
		t.Fatalf("cursor did not advance: %v", err)
	}
	expectedMin := floorToHour(time.Now().Unix()) // the cursor should have reached the current hour (excluded)
	if cursor != expectedMin {
		t.Errorf("cursor = %v, want %v (should advance through data-less hours too)", cursor, expectedMin)
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM power_hourly`).Scan(&n)
	if n != 1 {
		t.Errorf("expected exactly 1 power_hourly row (the other hours were empty), got %d", n)
	}
}

func TestBaselineWattsDynamicPercentile(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	values := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	for i, v := range values {
		db.Exec(`INSERT INTO power_hourly (bucket_start, watts_avg, watts_min, watts_max, kwh, voltage_avg, current_avg, sample_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			now-int64(i)*3600, v, v, v, 0, 230, 1, 1)
	}

	cfg := Config{BaselineWindow: 24 * time.Hour, BaselinePercentile: 0.1}
	b, err := baselineWatts(db, cfg)
	if err != nil {
		t.Fatalf("baselineWatts: %v", err)
	}
	if b != 20 {
		t.Errorf("baseline (10th percentile) = %v, want 20 (2nd lowest of 10 values)", b)
	}
}

func TestPruneMinutelyDeletesOnlyPastRetention(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	old := now - int64((9 * 24 * time.Hour).Seconds()) // past an 8-day retention
	recent := now - int64((1 * time.Hour).Seconds())   // well within it

	db.Exec(`INSERT INTO power_minutely (bucket_start, watts_avg, watts_min, watts_max, sample_count) VALUES (?, 1, 1, 1, 1)`, old)
	db.Exec(`INSERT INTO power_minutely (bucket_start, watts_avg, watts_min, watts_max, sample_count) VALUES (?, 1, 1, 1, 1)`, recent)
	db.Exec(`INSERT INTO resource_minutely (bucket_start, container, cpu_avg, cpu_min, cpu_max) VALUES (?, '__host__', 1, 1, 1)`, old)
	db.Exec(`INSERT INTO resource_minutely (bucket_start, container, cpu_avg, cpu_min, cpu_max) VALUES (?, '__host__', 1, 1, 1)`, recent)

	if err := pruneMinutely(db, 8*24*time.Hour); err != nil {
		t.Fatalf("pruneMinutely: %v", err)
	}

	var powerCount, resourceCount int
	db.QueryRow(`SELECT COUNT(*) FROM power_minutely`).Scan(&powerCount)
	db.QueryRow(`SELECT COUNT(*) FROM resource_minutely`).Scan(&resourceCount)
	if powerCount != 1 || resourceCount != 1 {
		t.Errorf("power_minutely/resource_minutely rows after prune = %d/%d, want 1/1 (only the recent row)", powerCount, resourceCount)
	}
}


func insertPowerSample(t *testing.T, db *sql.DB, ts int64, watts, cumulativeKwh float64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO power_samples (ts, watts, cumulative_kwh, voltage, current) VALUES (?, ?, ?, ?, ?)`,
		ts, watts, cumulativeKwh, 230.0, watts/230.0); err != nil {
		t.Fatalf("insert power_samples: %v", err)
	}
}

func insertResourceSample(t *testing.T, db *sql.DB, ts int64, container string, cpuPct float64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO resource_samples (ts, container, cpu_pct, mem_used, net_sent_bytes, net_recv_bytes, disk_io_bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ts, container, cpuPct, 100.0, 0.0, 0.0, nil); err != nil {
		t.Fatalf("insert resource_samples: %v", err)
	}
}

func TestRollupBucketReusesHourlyPowerWhenRawIsPruned(t *testing.T) {
	db := newTestDB(t)
	bucketStart := floorToHour(time.Now().Add(-10 * 24 * time.Hour).Unix())

	// Raw power long gone, hourly row kept; resource history just backfilled.
	db.Exec(`INSERT INTO power_hourly (bucket_start, watts_avg, watts_min, watts_max, kwh, voltage_avg, current_avg, sample_count) VALUES (?, 80, 70, 90, 0.08, 230, 1, 360)`, bucketStart)
	insertResourceSample(t, db, bucketStart+600, "__host__", 12.0)
	insertResourceSample(t, db, bucketStart+600, "immich_server", 30.0)

	if err := rollupBucket(db, Config{}, bucketStart); err != nil {
		t.Fatalf("rollupBucket: %v", err)
	}

	var cpu float64
	if err := db.QueryRow(`SELECT cpu_avg FROM resource_hourly WHERE bucket_start = ? AND container = '__host__'`, bucketStart).Scan(&cpu); err != nil || cpu != 12.0 {
		t.Errorf("host cpu = %v (err %v), want 12.0", cpu, err)
	}
	var watts float64
	db.QueryRow(`SELECT watts_avg FROM power_hourly WHERE bucket_start = ?`, bucketStart).Scan(&watts)
	if watts != 80 {
		t.Errorf("power_hourly overwritten: watts_avg = %v, want 80", watts)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM attribution_buckets WHERE bucket_start = ?`, bucketStart).Scan(&n)
	if n == 0 {
		t.Error("expected attribution rows recomputed from the hourly power")
	}
}

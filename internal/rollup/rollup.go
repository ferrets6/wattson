// Package rollup aggregates raw samples (power_samples, resource_samples)
// into long-term hourly rollups, runs the heuristic attribution for each
// hour, and prunes raw data past the short retention window.
package rollup

import (
	"context"
	"database/sql"
	"log"
	"sort"
	"time"

	"github.com/ferrets6/wattson/internal/attribution"
)

const bucketSeconds = 3600

// maxCatchUpBuckets caps how many hours get processed in one run (a safety
// bound against a pathological catch-up after a long outage); the next run
// picks up where this one left off.
const maxCatchUpBuckets = 24 * 30

// Config are the job's parameters, read from the environment/config by the caller.
type Config struct {
	RawRetention       time.Duration // default 7*24h
	BaselineWindow     time.Duration // default 7*24h
	BaselinePercentile float64       // default 0.1 (10th percentile = "low load")
	FixedBaselineWatts *float64      // if set, skips the dynamic computation
	Attribution        attribution.Config
}

func (c *Config) applyDefaults() {
	if c.RawRetention == 0 {
		c.RawRetention = 7 * 24 * time.Hour
	}
	if c.BaselineWindow == 0 {
		c.BaselineWindow = 7 * 24 * time.Hour
	}
	if c.BaselinePercentile == 0 {
		c.BaselinePercentile = 0.1
	}
}

// Start runs the rollup every hour until ctx is canceled. Never fails
// silently: every error is logged and the next run retries (the cursor in
// rollup_state guarantees it never gets stuck on a data-less hour).
func Start(ctx context.Context, db *sql.DB, cfg Config) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	RunOnce(db, cfg)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			RunOnce(db, cfg)
		}
	}
}

// RunOnce runs one rollup pass immediately (exported for the backfill tool:
// after inserting raw history there's no reason to wait up to an hour for
// the rollup to pick it up).
func RunOnce(db *sql.DB, cfg Config) {
	cfg.applyDefaults()
	buckets, err := pendingBuckets(db)
	if err != nil {
		log.Println("rollup: computing pending buckets failed:", err)
		return
	}
	for _, b := range buckets {
		if err := rollupBucket(db, cfg, b); err != nil {
			log.Println("rollup: bucket", time.Unix(b, 0).UTC(), "failed:", err)
			continue // cursor doesn't advance: retry next run
		}
		if err := advanceCursor(db, b+bucketSeconds); err != nil {
			log.Println("rollup: advancing cursor failed:", err)
			return
		}
	}
	if err := pruneRaw(db, cfg.RawRetention); err != nil {
		log.Println("rollup: pruning raw data failed:", err)
	}
}

func floorToHour(ts int64) int64 { return ts - ts%bucketSeconds }

func pendingBuckets(db *sql.DB) ([]int64, error) {
	var cursor sql.NullInt64
	if err := db.QueryRow(`SELECT value FROM rollup_state WHERE key = 'last_bucket'`).Scan(&cursor); err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	var start int64
	if cursor.Valid {
		start = cursor.Int64
	} else {
		var firstRaw sql.NullInt64
		if err := db.QueryRow(`SELECT MIN(ts) FROM power_samples`).Scan(&firstRaw); err != nil {
			return nil, err
		}
		if !firstRaw.Valid {
			return nil, nil // no raw data yet, nothing to do
		}
		start = floorToHour(firstRaw.Int64)
	}

	end := floorToHour(time.Now().Unix()) // the current hour is still partial, excluded
	var buckets []int64
	for b := start; b < end && len(buckets) < maxCatchUpBuckets; b += bucketSeconds {
		buckets = append(buckets, b)
	}
	return buckets, nil
}

func advanceCursor(db *sql.DB, next int64) error {
	_, err := db.Exec(`INSERT INTO rollup_state (key, value) VALUES ('last_bucket', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, next)
	return err
}

func rollupBucket(db *sql.DB, cfg Config, bucketStart int64) error {
	bucketEnd := bucketStart + bucketSeconds

	hasPower, wattsAvg, err := rollupPower(db, bucketStart, bucketEnd)
	if err != nil {
		return err
	}
	if !hasPower {
		return nil // no samples in this window (e.g. broker down): no row, a gap visible through the API
	}

	containers, err := rollupResources(db, bucketStart, bucketEnd)
	if err != nil {
		return err
	}

	baseline, err := baselineWatts(db, cfg)
	if err != nil {
		return err
	}

	var loads []attribution.ContainerLoad
	for _, c := range containers {
		if c.name == "__host__" {
			continue
		}
		loads = append(loads, attribution.ContainerLoad{Name: c.name, CpuAvg: c.cpuAvg})
	}

	for _, b := range attribution.Allocate(wattsAvg, loads, baseline, cfg.Attribution) {
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO attribution_buckets (bucket_start, bucket_end, container, category, watts_allocated) VALUES (?, ?, ?, ?, ?)`,
			bucketStart, bucketEnd, b.Container, b.Category, b.WattsAllocated,
		); err != nil {
			return err
		}
	}
	return nil
}

// rollupPower aggregates power_samples into one power_hourly row. hasPower
// is false if there were no samples in the window (no row written).
func rollupPower(db *sql.DB, bucketStart, bucketEnd int64) (hasPower bool, wattsAvg float64, err error) {
	var (
		count                            int
		wMin, wMax, vAvg, cAvg, kwhDelta sql.NullFloat64
	)
	row := db.QueryRow(
		`SELECT COUNT(*), AVG(watts), MIN(watts), MAX(watts), AVG(voltage), AVG(current), MAX(cumulative_kwh) - MIN(cumulative_kwh)
		 FROM power_samples WHERE ts >= ? AND ts < ?`,
		bucketStart, bucketEnd,
	)
	var avg sql.NullFloat64
	if err := row.Scan(&count, &avg, &wMin, &wMax, &vAvg, &cAvg, &kwhDelta); err != nil {
		return false, 0, err
	}
	if count == 0 {
		return false, 0, nil
	}

	_, err = db.Exec(
		`INSERT OR REPLACE INTO power_hourly (bucket_start, watts_avg, watts_min, watts_max, kwh, voltage_avg, current_avg, sample_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		bucketStart, avg.Float64, wMin.Float64, wMax.Float64, kwhDelta.Float64, vAvg.Float64, cAvg.Float64, count,
	)
	if err != nil {
		return false, 0, err
	}
	return true, avg.Float64, nil
}

type containerRollup struct {
	name   string
	cpuAvg float64
}

// rollupResources aggregates resource_samples (every container, including
// __host__) into resource_hourly, and returns the values just written.
//
// The SELECT results are fully buffered before any INSERT runs: with the DB
// pool capped at one connection (see store.Open), writing while the SELECT's
// *sql.Rows is still open would deadlock — that Rows pins the only
// connection, so the nested db.Exec would block forever waiting for a
// connection nothing will ever release.
func rollupResources(db *sql.DB, bucketStart, bucketEnd int64) ([]containerRollup, error) {
	rows, err := db.Query(
		`SELECT container, AVG(cpu_pct), AVG(mem_used), AVG(net_sent_bytes), AVG(net_recv_bytes), AVG(disk_io_bytes)
		 FROM resource_samples WHERE ts >= ? AND ts < ? GROUP BY container`,
		bucketStart, bucketEnd,
	)
	if err != nil {
		return nil, err
	}

	type aggregate struct {
		container                              string
		cpuAvg, memAvg, netSentAvg, netRecvAvg sql.NullFloat64
		diskAvg                                sql.NullFloat64
	}
	var aggregates []aggregate
	for rows.Next() {
		var a aggregate
		if err := rows.Scan(&a.container, &a.cpuAvg, &a.memAvg, &a.netSentAvg, &a.netRecvAvg, &a.diskAvg); err != nil {
			rows.Close()
			return nil, err
		}
		aggregates = append(aggregates, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	result := make([]containerRollup, 0, len(aggregates))
	for _, a := range aggregates {
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO resource_hourly (bucket_start, container, cpu_avg, mem_used_avg, net_sent_bytes_avg, net_recv_bytes_avg, disk_io_bytes_avg)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			bucketStart, a.container, a.cpuAvg.Float64, a.memAvg.Float64, a.netSentAvg.Float64, a.netRecvAvg.Float64, nullableFloat(a.diskAvg),
		); err != nil {
			return nil, err
		}
		result = append(result, containerRollup{name: a.container, cpuAvg: a.cpuAvg.Float64})
	}
	return result, nil
}

func nullableFloat(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}

// baselineWatts returns the idle baseline power: a fixed value if
// configured, otherwise the low percentile of watts_avg in the configured
// trailing window (default: 10th percentile, last 7 days).
func baselineWatts(db *sql.DB, cfg Config) (float64, error) {
	if cfg.FixedBaselineWatts != nil {
		return *cfg.FixedBaselineWatts, nil
	}

	windowStart := time.Now().Add(-cfg.BaselineWindow).Unix()
	rows, err := db.Query(`SELECT watts_avg FROM power_hourly WHERE bucket_start >= ?`, windowStart)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var values []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, nil // no history yet: everything counts as delta, degrades correctly
	}

	sort.Float64s(values)
	idx := int(float64(len(values)) * cfg.BaselinePercentile)
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx], nil
}

func pruneRaw(db *sql.DB, retention time.Duration) error {
	cutoff := time.Now().Add(-retention).Unix()
	if _, err := db.Exec(`DELETE FROM power_samples WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM resource_samples WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	return nil
}

// One-shot command to recover the history already sitting in Home Assistant
// (Tasmota power) and Beszel (host/container metrics) from before Wattson
// started running, instead of waiting for it to accumulate from scratch.
// Not part of the service: run by hand when needed.
//
// Usage:
//
//	go run ./cmd/backfill [--since 2026-09-16T00:00:00Z] [--until 2026-09-20T00:00:00Z] [--beszel-resolution 480m,120m]
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/joho/godotenv"

	"github.com/ferrets6/wattson/internal/attribution"
	"github.com/ferrets6/wattson/internal/beszel"
	"github.com/ferrets6/wattson/internal/homeassistant"
	"github.com/ferrets6/wattson/internal/rollup"
	"github.com/ferrets6/wattson/internal/store"
)

func main() {
	sinceFlag := flag.String("since", "2026-09-16T00:00:00Z", "backfill from this instant (RFC3339). Default: when the Tasmota was last restarted, the earliest useful data point in HA")
	untilFlag := flag.String("until", "", "Beszel history only: import nothing at or after this instant (RFC3339), to fill a gap without touching hours that already have live data. Default: now")
	beszelResolution := flag.String("beszel-resolution", "10m", "comma-separated Beszel resolutions to read, coarsest first (1m/10m/20m/120m/480m): finer ones override coarser ones hour by hour. Beszel keeps finer data only briefly (e.g. 120m ~7 days, 480m ~30 days)")
	flag.Parse()

	since, err := time.Parse(time.RFC3339, *sinceFlag)
	if err != nil {
		log.Fatalf("invalid --since: %v", err)
	}
	until := time.Now()
	if *untilFlag != "" {
		if until, err = time.Parse(time.RFC3339, *untilFlag); err != nil {
			log.Fatalf("invalid --until: %v", err)
		}
	}

	_ = godotenv.Load()
	db, err := store.Open(getenv("DB_PATH", "./wattson.db"))
	if err != nil {
		log.Fatalf("opening storage: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	earliest, err := backfillPower(ctx, db, since)
	if err != nil {
		log.Println("power backfill failed:", err)
	}

	earliestResource, err := backfillResources(ctx, db, since, until, strings.Split(*beszelResolution, ","))
	if err != nil {
		log.Println("resource backfill failed:", err)
	}
	if earliestResource != 0 && (earliest == 0 || earliestResource < earliest) {
		earliest = earliestResource
	}

	if earliest == 0 {
		log.Println("nothing imported, no rollup work to do")
		return
	}

	if err := rewindRollupCursor(db, earliest); err != nil {
		log.Fatalf("rewinding the rollup cursor failed: %v", err)
	}

	attrCfg, err := attribution.LoadConfig(getenv("ATTRIBUTION_CONFIG_PATH", ""))
	if err != nil {
		log.Fatalf("attribution config: %v", err)
	}

	log.Println("rolling up the imported period...")
	rollup.RunOnce(db, rollup.Config{Attribution: attrCfg})
	log.Println("done")
}

// backfillPower reads the power/current/voltage/cumulative-energy history
// from HA and merges them into power_samples rows. Returns the earliest
// imported unix timestamp (0 if none).
func backfillPower(ctx context.Context, db *sql.DB, since time.Time) (int64, error) {
	cfg := homeassistant.Config{
		URL:         getenv("HA_URL", ""),
		Token:       getenv("HA_TOKEN", ""),
		PunEntityID: getenv("HA_PUN_ENTITY_ID", ""),
		HTTPTimeout: 2 * time.Minute, // multi-day history across 4 entities: slower than live polling
	}
	prefix := getenv("HA_TASMOTA_ENTITY_PREFIX", "")
	if cfg.URL == "" || cfg.Token == "" || prefix == "" {
		return 0, fmt.Errorf("HA_URL/HA_TOKEN/HA_TASMOTA_ENTITY_PREFIX not configured")
	}
	client := homeassistant.New(db, cfg)

	// Tasmota line-1 entities used to reconstruct power_samples.
	entityPower := prefix + "_power_0"
	entityCurrent := prefix + "_current_0"
	entityVoltage := prefix + "_voltage"
	entityTotal := prefix + "_total_0"

	history, err := client.FetchHistory(ctx, []string{entityPower, entityCurrent, entityVoltage, entityTotal}, since)
	if err != nil {
		return 0, fmt.Errorf("reading HA history: %w", err)
	}

	power := history[entityPower]
	current := history[entityCurrent]
	voltage := history[entityVoltage]
	total := history[entityTotal]
	if len(power) == 0 {
		return 0, fmt.Errorf("no history for %s in the requested period", entityPower)
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO power_samples (ts, watts, cumulative_kwh, voltage, current) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var idxCur, idxVolt, idxTotal int
	var lastCur, lastVolt, lastTotal float64
	var haveCur, haveVolt, haveTotal bool
	var earliest, inserted int64

	for _, p := range power {
		for idxCur < len(current) && !current[idxCur].Time.After(p.Time) {
			lastCur, haveCur = current[idxCur].Value, true
			idxCur++
		}
		for idxVolt < len(voltage) && !voltage[idxVolt].Time.After(p.Time) {
			lastVolt, haveVolt = voltage[idxVolt].Value, true
			idxVolt++
		}
		for idxTotal < len(total) && !total[idxTotal].Time.After(p.Time) {
			lastTotal, haveTotal = total[idxTotal].Value, true
			idxTotal++
		}
		if !haveCur || !haveVolt || !haveTotal {
			continue // before all 4 series have at least one known value: skip rather than invent a zero
		}

		ts := p.Time.Unix()
		if _, err := stmt.Exec(ts, p.Value, lastTotal, lastVolt, lastCur); err != nil {
			return 0, err
		}
		inserted++
		if earliest == 0 || ts < earliest {
			earliest = ts
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	log.Printf("power_samples: %d rows imported from %s\n", inserted, since.Format(time.RFC3339))
	return earliest, nil
}

// sampleTimes maps one Beszel record to the resource_samples timestamps it
// fills. A record coarser than an hour is the average over the period
// ending at created, so it's written once per hour it covers, at the
// hour's midpoint: every hour gets a value (not one hour in 8), and a
// finer resolution imported afterwards lands on the same timestamps and
// replaces it. Only timestamps in [since, until) are kept.
func sampleTimes(created time.Time, resolution string, since, until time.Time) []int64 {
	period, err := time.ParseDuration(resolution)
	if err != nil || period <= time.Hour {
		if created.Before(since) || !created.Before(until) {
			return nil
		}
		return []int64{created.Unix()}
	}
	var out []int64
	end := created.Unix()
	for h := end - int64(period.Seconds()); h < end; h += 3600 {
		ts := h - h%3600 + 1800
		if ts >= since.Unix() && ts < until.Unix() && (len(out) == 0 || out[len(out)-1] != ts) {
			out = append(out, ts)
		}
	}
	return out
}

// backfillResources reads host/container history from Beszel and writes it
// to resource_samples for every monitored system, one resolution after the
// other (coarsest first, see sampleTimes).
func backfillResources(ctx context.Context, db *sql.DB, since, until time.Time, resolutions []string) (int64, error) {
	cfg := beszel.Config{
		URL:           getenv("BESZEL_URL", ""),
		AdminEmail:    getenv("BESZEL_ADMIN_EMAIL", ""),
		AdminPassword: getenv("BESZEL_ADMIN_PASSWORD", ""),
	}
	if cfg.URL == "" || cfg.AdminEmail == "" {
		return 0, fmt.Errorf("BESZEL_URL/BESZEL_ADMIN_EMAIL not configured")
	}
	client := beszel.New(db, cfg)

	systems, err := client.ListSystems(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading systems: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO resource_samples (ts, container, cpu_pct, mem_used, net_sent_bytes, net_recv_bytes, disk_io_bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var earliest, insertedHost, insertedContainer int64
	track := func(ts int64) {
		if earliest == 0 || ts < earliest {
			earliest = ts
		}
	}
	for _, resolution := range resolutions {
		// Fetch from a bit before since: a coarse record created after
		// since can still cover hours before it.
		fetchSince := since.Add(-8 * time.Hour)
		for _, sys := range systems {
			hostPoints, err := client.FetchSystemStatsHistory(ctx, sys.ID, resolution, fetchSince)
			if err != nil {
				log.Println("backfill: system_stats failed for", sys.Name, ":", err)
			}
			for _, h := range hostPoints {
				for _, ts := range sampleTimes(h.Time, resolution, since, until) {
					if _, err := stmt.Exec(ts, "__host__", h.Cpu, h.MemUsed, h.NetSent, h.NetRecv, h.DiskIO); err != nil {
						return 0, err
					}
					insertedHost++
					track(ts)
				}
			}

			containerPoints, err := client.FetchContainerStatsHistory(ctx, sys.ID, resolution, fetchSince)
			if err != nil {
				log.Println("backfill: container_stats failed for", sys.Name, ":", err)
			}
			for _, c := range containerPoints {
				for _, ts := range sampleTimes(c.Time, resolution, since, until) {
					if _, err := stmt.Exec(ts, c.Name, c.Cpu, c.Mem, c.NetSent, c.NetRecv, nil); err != nil {
						return 0, err
					}
					insertedContainer++
					track(ts)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	log.Printf("resource_samples: %d host rows + %d container rows imported (resolutions %s)\n", insertedHost, insertedContainer, strings.Join(resolutions, ","))
	return earliest, nil
}

// rewindRollupCursor moves the rollup_state cursor back if the newly
// imported history starts before where the rollup had already reached,
// otherwise the next run would never reprocess the hours just imported (see
// the rollup_state comment in internal/rollup).
func rewindRollupCursor(db *sql.DB, earliestTS int64) error {
	flooredHour := earliestTS - earliestTS%3600
	_, err := db.Exec(
		`INSERT INTO rollup_state (key, value) VALUES ('last_bucket', ?)
		 ON CONFLICT(key) DO UPDATE SET value = MIN(value, excluded.value)`,
		flooredHour,
	)
	return err
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Package hostcpu samples instantaneous host-wide CPU utilization from
// /proc/stat, independent of Beszel's own ~1-minute polling. It exists
// only to feed the live (last 15 min) chart at the same ~2s cadence as the
// wattmeter, so the power and CPU lines actually move together there --
// the historical hourly/minutely CPU rollups still come exclusively from
// Beszel (needed for its per-container breakdown, which /proc can't give).
//
// Docker containers see the host's own /proc/stat by default (CPU counters
// aren't namespaced per-container the way cgroup limits are), unless a
// proc-virtualizing runtime like LXCFS is in play. If this ever reports
// the container's own view instead of the host's, that's the first thing
// to check -- HOSTCPU_PROC_STAT_PATH exists as an escape hatch to point at
// a bind-mounted /host/proc/stat instead, without a code change.
package hostcpu

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Reader turns successive /proc/stat reads into a CPU utilization
// percentage. Stateful: the first Read after construction (or after any
// read error) has nothing to diff against.
type Reader struct {
	path    string
	prev    *sample
	failing bool // true after the first logged failure, until a read succeeds again
}

type sample struct {
	idle, total uint64
}

func NewReader(path string) *Reader {
	if path == "" {
		path = "/proc/stat"
	}
	return &Reader{path: path}
}

// Read returns the CPU utilization percent (0-100) since the previous
// call. ok is false when there's no previous sample to diff against yet,
// or on a read/parse error (logged once per failure streak, not every
// call — a wedged /proc path shouldn't spam the log forever on a fast
// ticker).
func (r *Reader) Read() (pct float64, ok bool) {
	s, err := readStat(r.path)
	if err != nil {
		if !r.failing {
			log.Println("hostcpu: read failed, will keep retrying silently:", err)
			r.failing = true
		}
		r.prev = nil // force a fresh baseline once it starts working again
		return 0, false
	}
	r.failing = false
	prev := r.prev
	r.prev = s
	if prev == nil {
		return 0, false
	}

	deltaTotal := s.total - prev.total
	deltaIdle := s.idle - prev.idle
	if deltaTotal == 0 {
		return 0, false
	}
	return 100 * (1 - float64(deltaIdle)/float64(deltaTotal)), true
}

// readStat parses /proc/stat's first line: "cpu  user nice system idle
// iowait irq softirq steal guest guest_nice" (all in USER_HZ jiffies).
func readStat(path string) (*sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return nil, fmt.Errorf("hostcpu: empty %s", path)
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return nil, fmt.Errorf("hostcpu: unexpected first line of %s: %q", path, scanner.Text())
	}

	vals := make([]uint64, 0, len(fields)-1)
	var total uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("hostcpu: parsing %s: %w", path, err)
		}
		vals = append(vals, v)
		total += v
	}

	idle := vals[3] // idle
	if len(vals) > 4 {
		idle += vals[4] // + iowait: waiting on disk I/O isn't "busy" CPU either
	}
	return &sample{idle: idle, total: total}, nil
}

// Start samples every interval (default 2s) until ctx is canceled,
// inserting into host_cpu_samples. Errors are logged, not fatal: a missing
// or unreadable /proc/stat just means no live CPU line, not a crash.
func Start(ctx context.Context, db *sql.DB, interval time.Duration) {
	if interval == 0 {
		interval = 2 * time.Second
	}
	r := NewReader(os.Getenv("HOSTCPU_PROC_STAT_PATH"))
	r.Read() // primes the baseline; discarded, no delta yet

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pct, ok := r.Read()
			if !ok {
				continue
			}
			if _, err := db.Exec(`INSERT OR REPLACE INTO host_cpu_samples (ts, cpu_pct) VALUES (?, ?)`, time.Now().Unix(), pct); err != nil {
				log.Println("hostcpu: insert failed:", err)
			}
		}
	}
}

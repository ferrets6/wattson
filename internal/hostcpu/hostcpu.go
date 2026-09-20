// Package hostcpu samples host CPU utilization from /proc/stat every ~2s,
// feeding only the live chart (Beszel's ~1-minute poll is too coarse for
// it). Historical charts and attribution still use Beszel.
//
// Docker exposes the host's own /proc/stat by default; if that's ever not
// true here, HOSTCPU_PROC_STAT_PATH points at a bind-mounted alternative.
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

// Reader turns successive /proc/stat reads into a CPU utilization percent;
// the first Read (or one after an error) has nothing to diff against.
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

// Read returns CPU utilization percent (0-100) since the previous call, or
// ok=false with nothing to diff against yet or on a read/parse error
// (logged once per failure streak, not every call).
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

// Start samples every interval (default 2s) until ctx is canceled, writing
// to host_cpu_samples. Errors are logged, not fatal.
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

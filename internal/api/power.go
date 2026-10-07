package api

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/ferrets6/wattson/internal/pricing"
)

// staleAfter: past this, /power/current reports stale=true instead of
// silently going quiet.
const staleAfter = 2 * time.Minute

type powerCurrentResponse struct {
	Ts    int64   `json:"ts"`
	Watts float64 `json:"watts"`
	Stale bool    `json:"stale"`
}

func powerCurrentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ts int64
		var watts float64
		err := db.QueryRow(`SELECT ts, watts FROM power_samples ORDER BY ts DESC LIMIT 1`).Scan(&ts, &watts)
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "no power data available")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		stale := time.Since(time.Unix(ts, 0)) > staleAfter
		writeJSON(w, http.StatusOK, powerCurrentResponse{Ts: ts, Watts: watts, Stale: stale})
	}
}

type powerHourlyPoint struct {
	BucketStart int64   `json:"bucket_start"`
	WattsAvg    float64 `json:"watts_avg"`
	WattsMin    float64 `json:"watts_min"`
	WattsMax    float64 `json:"watts_max"`
	Kwh         float64 `json:"kwh"`
	// CPU is null for buckets with no host CPU data (e.g. a Beszel gap):
	// charted as a gap, never as a fake 0%.
	CpuAvgPct *float64 `json:"cpu_avg_pct"`
	CpuMinPct *float64 `json:"cpu_min_pct"`
	CpuMaxPct *float64 `json:"cpu_max_pct"`
}

// minutelyRangeThreshold: requests spanning at most this long use the
// minute-level rollup instead of the hourly one. Kept under
// rollup.Config's MinutelyRetention (8 days) so a request near the edge
// doesn't land on data about to be pruned.
const minutelyRangeThreshold = 7 * 24 * time.Hour

const historyHourlyQuery = `
	SELECT ph.bucket_start, ph.watts_avg, ph.watts_min, ph.watts_max, ph.kwh,
	       rh.cpu_avg, COALESCE(rh.cpu_min, rh.cpu_avg), COALESCE(rh.cpu_max, rh.cpu_avg)
	FROM power_hourly ph
	LEFT JOIN resource_hourly rh ON rh.bucket_start = ph.bucket_start AND rh.container = '__host__'
	WHERE ph.bucket_start >= ? AND ph.bucket_start < ?
	ORDER BY ph.bucket_start`

// power_minutely has no kwh column; the literal 0 keeps powerHourlyPoint's
// shape identical either way. Minutes with no minute-level CPU fall back to
// their hour's: hours rebuilt from Beszel's coarse history (cmd/backfill)
// only have hourly CPU, which would otherwise chart as all gaps.
const historyMinutelyQuery = `
	SELECT pm.bucket_start, pm.watts_avg, pm.watts_min, pm.watts_max, 0,
	       COALESCE(rm.cpu_avg, rh.cpu_avg), COALESCE(rm.cpu_min, rh.cpu_min, rh.cpu_avg), COALESCE(rm.cpu_max, rh.cpu_max, rh.cpu_avg)
	FROM power_minutely pm
	LEFT JOIN resource_minutely rm ON rm.bucket_start = pm.bucket_start AND rm.container = '__host__'
	LEFT JOIN resource_hourly rh ON rh.bucket_start = pm.bucket_start - pm.bucket_start % 3600 AND rh.container = '__host__'
	WHERE pm.bucket_start >= ? AND pm.bucket_start < ?
	ORDER BY pm.bucket_start`

// powerHistoryHandler returns the rollup in [from, to) -- minute-level for
// requests spanning up to a week, hourly beyond that -- plus host CPU for
// the same buckets.
func powerHistoryHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseUnixRange(w, r)
		if !ok {
			return
		}

		query := historyHourlyQuery
		if time.Duration(to-from)*time.Second <= minutelyRangeThreshold {
			query = historyMinutelyQuery
		}

		rows, err := db.Query(query, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer rows.Close()

		points := []powerHourlyPoint{}
		for rows.Next() {
			var p powerHourlyPoint
			if err := rows.Scan(&p.BucketStart, &p.WattsAvg, &p.WattsMin, &p.WattsMax, &p.Kwh, &p.CpuAvgPct, &p.CpuMinPct, &p.CpuMaxPct); err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			points = append(points, p)
		}
		writeJSON(w, http.StatusOK, points)
	}
}

type livePoint struct {
	Ts    int64   `json:"ts"`
	Value float64 `json:"value"`
}

type liveResponse struct {
	Power []livePoint `json:"power"`
	Cpu   []livePoint `json:"cpu"`
}

// liveWindow: how far back /power/live looks. A sliding window, not a
// paged range — the frontend just polls this on an interval for a "live"
// view, it doesn't need history depth here (that's /power/history).
const liveWindow = 15 * time.Minute

// powerLiveHandler returns raw power and host CPU samples from the last
// liveWindow, or only whatever's newer than power_since/cpu_since if given
// — the frontend polls this every 2s and, after its first full-window
// fetch, only needs the new points since its last poll, not the whole
// window resent each time. CPU comes from host_cpu_samples (internal/
// hostcpu, ~2s from /proc/stat) rather than Beszel, which only updates
// once a minute — too coarse for this view. The two series aren't
// timestamp-aligned, so the frontend charts them on a shared time axis
// rather than by index.
func powerLiveHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		windowStart := time.Now().Add(-liveWindow).Unix()
		powerSince := sinceParam(r, "power_since", windowStart)
		cpuSince := sinceParam(r, "cpu_since", windowStart)

		power, err := queryLiveSeries(db, `SELECT ts, watts FROM power_samples WHERE ts > ? ORDER BY ts`, powerSince)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		cpu, err := queryLiveSeries(db, `SELECT ts, cpu_pct FROM host_cpu_samples WHERE ts > ? ORDER BY ts`, cpuSince)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, liveResponse{Power: power, Cpu: cpu})
	}
}

// sinceParam reads an incremental-fetch cursor from the query string,
// falling back to (and never going earlier than) windowStart.
func sinceParam(r *http.Request, name string, windowStart int64) int64 {
	if s := r.URL.Query().Get(name); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > windowStart {
			return v
		}
	}
	return windowStart
}

func queryLiveSeries(db *sql.DB, query string, since int64) ([]livePoint, error) {
	rows, err := db.Query(query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := []livePoint{}
	for rows.Next() {
		var p livePoint
		if err := rows.Scan(&p.Ts, &p.Value); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

type summaryPeriod struct {
	KWh      float64 `json:"kwh"`
	CostEur  float64 `json:"cost_eur"`
	Complete bool    `json:"complete"` // false: no PUN reference for at least one hour, never estimated
	// Provisional: cost uses the previous month's price as an estimate
	// (current month isn't over yet) — a declared estimate, not missing data.
	Provisional bool `json:"provisional"`
}

type priceInfo struct {
	EurPerKwh   float64 `json:"eur_per_kwh"`
	Complete    bool    `json:"complete"`
	Provisional bool    `json:"provisional"`
	Source      string  `json:"source"` // "fixed_override" | "spread_override" | "default"
}

type summaryResponse struct {
	// Last24h is a rolling window, not "since local midnight": right after
	// midnight the latter is nearly empty and not a useful KPI.
	Last24h summaryPeriod `json:"last24h"`
	Month   summaryPeriod `json:"month"`
	// MonthStart is the current calendar month's start (unix seconds), so the
	// frontend can format "Cost for <month>" in the viewer's own locale
	// instead of the server guessing a language.
	MonthStart   int64     `json:"month_start"`
	CurrentPrice priceInfo `json:"current_price"`
}

func powerSummaryHandler(db *sql.DB, defaultSpread float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		loc := pricing.RomeLocation()
		now := time.Now().In(loc)
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)

		resolve := func(at time.Time) (pricing.Result, error) { return pricing.Resolve(db, at, defaultSpread) }

		last24h, err := summaryFor(db, now.Add(-24*time.Hour).Unix(), now.Unix(), resolve)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		month, err := summaryFor(db, monthStart.Unix(), now.Unix(), resolve)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		current, err := pricing.Resolve(db, time.Now(), defaultSpread)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, summaryResponse{
			Last24h:    last24h,
			Month:      month,
			MonthStart: monthStart.Unix(),
			CurrentPrice: priceInfo{
				EurPerKwh: current.EurPerKwh, Complete: current.Complete,
				Provisional: current.Provisional, Source: current.Source,
			},
		})
	}
}

// summaryFor sums kWh and cost for the hours in [fromTS, toTS) using resolve
// for each hour's price. Shared by /power/summary, /power/cost and
// /pricing/preview: only the price resolution differs (saved periods vs. a
// hypothetical override).
func summaryFor(db *sql.DB, fromTS, toTS int64, resolve func(time.Time) (pricing.Result, error)) (summaryPeriod, error) {
	hours, err := hourlyCosts(db, fromTS, toTS, resolve)
	if err != nil {
		return summaryPeriod{}, err
	}
	result := summaryPeriod{Complete: true}
	for _, h := range hours {
		result.add(h.summaryPeriod)
	}
	return result, nil
}

func (s *summaryPeriod) add(o summaryPeriod) {
	s.KWh += o.KWh
	s.CostEur += o.CostEur
	s.Complete = s.Complete && o.Complete
	s.Provisional = s.Provisional || o.Provisional
}

type hourCost struct {
	start int64
	summaryPeriod
}

// hourlyCosts prices each power_hourly bucket in [fromTS, toTS). An hour
// with no PUN reference has Complete=false and no cost, never estimated.
//
// The SELECT results are fully buffered before calling resolve: with the DB
// pool capped at one connection (see store.Open), resolve's own queries
// (pricing.Resolve reads pricing_periods/pun_prices) would deadlock while
// this function's own *sql.Rows is still open and pinning that connection.
func hourlyCosts(db *sql.DB, fromTS, toTS int64, resolve func(time.Time) (pricing.Result, error)) ([]hourCost, error) {
	rows, err := db.Query(`SELECT bucket_start, kwh FROM power_hourly WHERE bucket_start >= ? AND bucket_start < ? ORDER BY bucket_start`, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	var hours []hourCost
	for rows.Next() {
		var h hourCost
		if err := rows.Scan(&h.start, &h.KWh); err != nil {
			rows.Close()
			return nil, err
		}
		hours = append(hours, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range hours {
		price, err := resolve(time.Unix(hours[i].start, 0))
		if err != nil {
			return nil, err
		}
		hours[i].Complete = price.Complete
		if price.Complete {
			hours[i].Provisional = price.Provisional
			hours[i].CostEur = hours[i].KWh * price.EurPerKwh
		}
	}
	return hours, nil
}

type costBucket struct {
	BucketStart int64 `json:"bucket_start"`
	summaryPeriod
}

type costResponse struct {
	Bucket  string        `json:"bucket"` // "hour" | "day"
	Buckets []costBucket  `json:"buckets"`
	Total   summaryPeriod `json:"total"`
}

// powerCostHandler returns cost per hour (ranges up to a week) or per
// Europe/Rome calendar day (longer ranges) in [from, to), plus the total.
func powerCostHandler(db *sql.DB, defaultSpread float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseUnixRange(w, r)
		if !ok {
			return
		}
		resolve := func(at time.Time) (pricing.Result, error) { return pricing.Resolve(db, at, defaultSpread) }
		hours, err := hourlyCosts(db, from, to, resolve)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp := costResponse{Bucket: "hour", Buckets: []costBucket{}, Total: summaryPeriod{Complete: true}}
		daily := time.Duration(to-from)*time.Second > minutelyRangeThreshold
		if daily {
			resp.Bucket = "day"
		}
		loc := pricing.RomeLocation()
		for _, h := range hours {
			resp.Total.add(h.summaryPeriod)
			key := h.start
			if daily {
				t := time.Unix(h.start, 0).In(loc)
				key = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).Unix()
			}
			if n := len(resp.Buckets); n > 0 && resp.Buckets[n-1].BucketStart == key {
				resp.Buckets[n-1].add(h.summaryPeriod)
				continue
			}
			resp.Buckets = append(resp.Buckets, costBucket{BucketStart: key, summaryPeriod: h.summaryPeriod})
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// parseUnixRange reads from/to (unix seconds) from the query string, writing
// an error response and returning ok=false if missing or invalid.
func parseUnixRange(w http.ResponseWriter, r *http.Request) (from, to int64, ok bool) {
	fromStr, toStr := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if fromStr == "" || toStr == "" {
		writeError(w, http.StatusBadRequest, "from/to query params (unix seconds) are required")
		return 0, 0, false
	}
	from, err1 := strconv.ParseInt(fromStr, 10, 64)
	to, err2 := strconv.ParseInt(toStr, 10, 64)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "from/to must be integer unix timestamps")
		return 0, 0, false
	}
	return from, to, true
}

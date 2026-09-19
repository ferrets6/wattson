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
	CpuAvgPct   float64 `json:"cpu_avg_pct"`
}

// powerHistoryHandler returns the hourly rollup in [from, to), plus the
// host's CPU usage for the same buckets (0 if no resource data landed that
// hour) so the frontend can chart it alongside power. Hourly groupby only
// for now: day/category/service can be added when the frontend actually
// needs them.
func powerHistoryHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseUnixRange(w, r)
		if !ok {
			return
		}

		rows, err := db.Query(
			`SELECT ph.bucket_start, ph.watts_avg, ph.watts_min, ph.watts_max, ph.kwh, COALESCE(rh.cpu_avg, 0)
			 FROM power_hourly ph
			 LEFT JOIN resource_hourly rh ON rh.bucket_start = ph.bucket_start AND rh.container = '__host__'
			 WHERE ph.bucket_start >= ? AND ph.bucket_start < ?
			 ORDER BY ph.bucket_start`,
			from, to,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer rows.Close()

		points := []powerHourlyPoint{}
		for rows.Next() {
			var p powerHourlyPoint
			if err := rows.Scan(&p.BucketStart, &p.WattsAvg, &p.WattsMin, &p.WattsMax, &p.Kwh, &p.CpuAvgPct); err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			points = append(points, p)
		}
		writeJSON(w, http.StatusOK, points)
	}
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
// for each hour's price. Shared by /power/summary and /pricing/preview: only
// the price resolution differs (saved periods vs. a hypothetical override).
//
// The SELECT results are fully buffered before calling resolve: with the DB
// pool capped at one connection (see store.Open), resolve's own queries
// (pricing.Resolve reads pricing_periods/pun_prices) would deadlock while
// this function's own *sql.Rows is still open and pinning that connection.
func summaryFor(db *sql.DB, fromTS, toTS int64, resolve func(time.Time) (pricing.Result, error)) (summaryPeriod, error) {
	rows, err := db.Query(`SELECT bucket_start, kwh FROM power_hourly WHERE bucket_start >= ? AND bucket_start < ?`, fromTS, toTS)
	if err != nil {
		return summaryPeriod{}, err
	}

	type bucket struct {
		start int64
		kwh   float64
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.start, &b.kwh); err != nil {
			rows.Close()
			return summaryPeriod{}, err
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return summaryPeriod{}, err
	}
	rows.Close()

	result := summaryPeriod{Complete: true}
	for _, b := range buckets {
		result.KWh += b.kwh

		price, err := resolve(time.Unix(b.start, 0))
		if err != nil {
			return summaryPeriod{}, err
		}
		if !price.Complete {
			result.Complete = false
			continue
		}
		if price.Provisional {
			result.Provisional = true
		}
		result.CostEur += b.kwh * price.EurPerKwh
	}
	return result, nil
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

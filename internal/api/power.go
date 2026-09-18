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
}

// powerHistoryHandler returns the hourly rollup in [from, to). Hourly
// groupby only for now: day/category/service can be added when the
// frontend actually needs them.
func powerHistoryHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseUnixRange(w, r)
		if !ok {
			return
		}

		rows, err := db.Query(
			`SELECT bucket_start, watts_avg, watts_min, watts_max, kwh FROM power_hourly WHERE bucket_start >= ? AND bucket_start < ? ORDER BY bucket_start`,
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
			if err := rows.Scan(&p.BucketStart, &p.WattsAvg, &p.WattsMin, &p.WattsMax, &p.Kwh); err != nil {
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
	Today summaryPeriod `json:"today"`
	Month summaryPeriod `json:"month"`
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
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)

		resolve := func(at time.Time) (pricing.Result, error) { return pricing.Resolve(db, at, defaultSpread) }

		today, err := summaryFor(db, todayStart.Unix(), now.Unix(), resolve)
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
			Today:      today,
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
func summaryFor(db *sql.DB, fromTS, toTS int64, resolve func(time.Time) (pricing.Result, error)) (summaryPeriod, error) {
	rows, err := db.Query(`SELECT bucket_start, kwh FROM power_hourly WHERE bucket_start >= ? AND bucket_start < ?`, fromTS, toTS)
	if err != nil {
		return summaryPeriod{}, err
	}
	defer rows.Close()

	result := summaryPeriod{Complete: true}
	for rows.Next() {
		var bucketStart int64
		var kwh float64
		if err := rows.Scan(&bucketStart, &kwh); err != nil {
			return summaryPeriod{}, err
		}
		result.KWh += kwh

		price, err := resolve(time.Unix(bucketStart, 0))
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
		result.CostEur += kwh * price.EurPerKwh
	}
	return result, rows.Err()
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

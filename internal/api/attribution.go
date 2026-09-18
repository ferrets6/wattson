package api

import (
	"database/sql"
	"net/http"

	"github.com/ferrets6/wattson/internal/attribution"
)

// KwhAllocated, not Watts: attribution_buckets.watts_allocated is that
// hour's *average* power, so summing it across multiple hourly buckets
// yields energy (Wh, one bucket = 1h), not power. Converted to kWh here.
type categoryShare struct {
	Category     string  `json:"category"`
	KwhAllocated float64 `json:"kwh_allocated"`
}

type containerShare struct {
	Container    string  `json:"container"`
	Category     string  `json:"category"`
	KwhAllocated float64 `json:"kwh_allocated"`
}

type attributionResponse struct {
	ByCategory  []categoryShare  `json:"by_category"`
	ByContainer []containerShare `json:"by_container"`
}

// attributionHandler returns the category/container breakdown in [from,
// to): always a heuristic estimate (see CLAUDE.md), never presented as an
// exact measurement — the frontend must label it as such.
func attributionHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseUnixRange(w, r)
		if !ok {
			return
		}

		byCategory, err := queryCategoryShares(db, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		byContainer, err := queryContainerShares(db, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, attributionResponse{ByCategory: byCategory, ByContainer: byContainer})
	}
}

func queryCategoryShares(db *sql.DB, from, to int64) ([]categoryShare, error) {
	rows, err := db.Query(
		`SELECT category, SUM(watts_allocated) / 1000.0 FROM attribution_buckets WHERE bucket_start >= ? AND bucket_start < ? GROUP BY category ORDER BY SUM(watts_allocated) DESC`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	shares := []categoryShare{}
	for rows.Next() {
		var s categoryShare
		if err := rows.Scan(&s.Category, &s.KwhAllocated); err != nil {
			return nil, err
		}
		shares = append(shares, s)
	}
	return shares, rows.Err()
}

// queryContainerShares excludes the baseline row (not a container, it's the
// power share below the idle threshold) and caps at the top 15 to keep the
// UI uncluttered: past that the rest goes into an "Other" bucket.
func queryContainerShares(db *sql.DB, from, to int64) ([]containerShare, error) {
	rows, err := db.Query(
		`SELECT container, category, SUM(watts_allocated) / 1000.0 FROM attribution_buckets
		 WHERE bucket_start >= ? AND bucket_start < ? AND container != ?
		 GROUP BY container, category ORDER BY SUM(watts_allocated) DESC LIMIT 15`,
		from, to, attribution.BaselineContainer,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	shares := []containerShare{}
	for rows.Next() {
		var s containerShare
		if err := rows.Scan(&s.Container, &s.Category, &s.KwhAllocated); err != nil {
			return nil, err
		}
		shares = append(shares, s)
	}
	return shares, rows.Err()
}

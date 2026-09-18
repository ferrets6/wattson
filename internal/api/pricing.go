package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ferrets6/wattson/internal/pricing"
)

func pricingListHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		periods, err := pricing.ListPeriods(db)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, periods)
	}
}

type periodInput struct {
	ValidFrom   string  `json:"valid_from"`
	ValidUntil  string  `json:"valid_until"`
	Mode        string  `json:"mode"`
	SpreadValue float64 `json:"spread_value"`
	FixedPrice  float64 `json:"fixed_price"`
	Note        string  `json:"note"`
}

func (in periodInput) toPeriod(id int64) pricing.Period {
	return pricing.Period{
		ID: id, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil, Mode: in.Mode,
		SpreadValue: in.SpreadValue, FixedPrice: in.FixedPrice, Note: in.Note,
	}
}

func pricingCreateHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in periodInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		id, err := pricing.CreatePeriod(db, in.toPeriod(0))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
	}
}

func pricingUpdateHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		var in periodInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if err := pricing.UpdatePeriod(db, in.toPeriod(id)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func pricingDeleteHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		if err := pricing.DeletePeriod(db, id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// pricingPreviewHandler computes the cost [from, to] (calendar dates) would
// have had with a hypothetical override, without saving anything: for the
// "live" price editing in the UI (see PLAN.md).
func pricingPreviewHandler(db *sql.DB, defaultSpread float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fromDate, toDate := q.Get("from"), q.Get("to")
		if fromDate == "" || toDate == "" {
			writeError(w, http.StatusBadRequest, "from/to query params (YYYY-MM-DD) are required")
			return
		}

		override := pricing.Period{ValidFrom: fromDate, ValidUntil: toDate}
		switch {
		case q.Has("fixed_price"):
			v, err := strconv.ParseFloat(q.Get("fixed_price"), 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid fixed_price")
				return
			}
			override.Mode = pricing.ModeFixedOverride
			override.FixedPrice = v
		case q.Has("spread"):
			v, err := strconv.ParseFloat(q.Get("spread"), 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid spread")
				return
			}
			override.Mode = pricing.ModeSpreadOverride
			override.SpreadValue = v
		default:
			writeError(w, http.StatusBadRequest, "specify either spread or fixed_price")
			return
		}

		fromTS, toTS, err := pricing.DateRangeUnix(fromDate, toDate)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		resolve := func(at time.Time) (pricing.Result, error) {
			return pricing.ResolvePreview(db, at, override, defaultSpread)
		}
		result, err := summaryFor(db, fromTS, toTS, resolve)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

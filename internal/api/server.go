// Package api exposes Wattson's REST routes. Authentication (OIDC) is
// applied by the caller (cmd/wattson), not here: this package knows nothing
// about sessions or login.
package api

import (
	"database/sql"
	"net/http"
)

// RegisterRoutes registers the /api/v1/* routes on the caller's mux, so they
// can coexist with /login, /callback, /logout, and the static frontend
// under a single auth middleware.
func RegisterRoutes(mux *http.ServeMux, db *sql.DB, defaultSpread float64) {
	mux.HandleFunc("GET /api/v1/power/current", powerCurrentHandler(db))
	mux.HandleFunc("GET /api/v1/power/history", powerHistoryHandler(db))
	mux.HandleFunc("GET /api/v1/power/summary", powerSummaryHandler(db, defaultSpread))
	mux.HandleFunc("GET /api/v1/power/attribution", attributionHandler(db))

	mux.HandleFunc("GET /api/v1/pricing/periods", pricingListHandler(db))
	mux.HandleFunc("POST /api/v1/pricing/periods", pricingCreateHandler(db))
	mux.HandleFunc("PUT /api/v1/pricing/periods/{id}", pricingUpdateHandler(db))
	mux.HandleFunc("DELETE /api/v1/pricing/periods/{id}", pricingDeleteHandler(db))
	mux.HandleFunc("GET /api/v1/pricing/preview", pricingPreviewHandler(db, defaultSpread))
}

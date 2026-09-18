// Package pricing resolves the energy price for a given hour, applying
// override periods (fixed price or custom spread) on top of the monthly PUN
// average + default spread. See PLAN.md's "Pricing engine" section for the algorithm.
package pricing

import (
	"database/sql"
	"fmt"
	"time"
)

// romeLocation is where valid_from/valid_until are interpreted: Italian
// calendar day (local midnight to midnight), matching the fact that bills
// follow the Italian calendar. Loaded once.
var romeLocation = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		panic("pricing: failed to load Europe/Rome: " + err.Error())
	}
	return loc
}()

const (
	ModeSpreadOverride = "spread_override"
	ModeFixedOverride  = "fixed_override"
)

// RomeLocation is the timezone used to interpret period dates (Europe/Rome
// calendar day). Exported because the API (for power/summary's "today"/
// "this month") needs the same day boundaries.
func RomeLocation() *time.Location { return romeLocation }

// DateRangeUnix converts a calendar date range (like a Period's) to the
// corresponding unix timestamps [from, until). validUntil is required here
// (unlike Period.ValidUntil): a bounded range is needed to sum kWh over it.
func DateRangeUnix(validFrom, validUntil string) (from int64, until int64, err error) {
	r, err := rangeOf(Period{ValidFrom: validFrom, ValidUntil: validUntil})
	if err != nil {
		return 0, 0, err
	}
	if !r.hasUntil {
		return 0, 0, fmt.Errorf("valid_until is required to compute a range")
	}
	return r.from, r.until, nil
}

// Period is a price override period. ValidFrom/ValidUntil are ISO 8601 dates
// (YYYY-MM-DD); an empty ValidUntil means no expiry.
type Period struct {
	ID          int64
	ValidFrom   string
	ValidUntil  string // "" = no expiry
	Mode        string
	SpreadValue float64 // used only if Mode == ModeSpreadOverride
	FixedPrice  float64 // used only if Mode == ModeFixedOverride
	Note        string
}

// timeRange is [From, Until) in unix seconds; Until is a zero value (fine) for "no expiry".
type timeRange struct {
	from, until int64
	hasUntil    bool
}

func parseDateInRome(date string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", date, romeLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q (expected YYYY-MM-DD): %w", date, err)
	}
	return t, nil
}

// rangeOf converts a period's dates into the unix interval [from, until):
// valid_until is inclusive as a day, so the exclusive upper bound is
// midnight of the following day.
func rangeOf(p Period) (timeRange, error) {
	from, err := parseDateInRome(p.ValidFrom)
	if err != nil {
		return timeRange{}, err
	}
	r := timeRange{from: from.Unix()}
	if p.ValidUntil == "" {
		return r, nil
	}
	until, err := parseDateInRome(p.ValidUntil)
	if err != nil {
		return timeRange{}, err
	}
	r.until = until.AddDate(0, 0, 1).Unix()
	r.hasUntil = true
	return r, nil
}

func (r timeRange) contains(ts int64) bool {
	if ts < r.from {
		return false
	}
	return !r.hasUntil || ts < r.until
}

func (r timeRange) overlaps(o timeRange) bool {
	aEnd, bEnd := r.until, o.until
	if !r.hasUntil {
		aEnd = int64(1<<63 - 1)
	}
	if !o.hasUntil {
		bEnd = int64(1<<63 - 1)
	}
	return r.from < bEnd && o.from < aEnd
}

func validatePeriod(p Period) error {
	if p.ValidUntil != "" && p.ValidUntil < p.ValidFrom {
		return fmt.Errorf("valid_until (%s) is before valid_from (%s)", p.ValidUntil, p.ValidFrom)
	}
	switch p.Mode {
	case ModeFixedOverride:
		if p.FixedPrice <= 0 {
			return fmt.Errorf("fixed_override requires fixed_price > 0")
		}
	case ModeSpreadOverride:
		// spread_value may be zero or negative (e.g. a promotional rate below PUN), no sign constraint
	default:
		return fmt.Errorf("invalid mode %q (expected %q or %q)", p.Mode, ModeSpreadOverride, ModeFixedOverride)
	}
	return nil
}

// ListPeriods returns all periods ordered by valid_from.
func ListPeriods(db *sql.DB) ([]Period, error) {
	rows, err := db.Query(`SELECT id, valid_from, COALESCE(valid_until, ''), mode, spread_value, fixed_price, COALESCE(note, '') FROM pricing_periods ORDER BY valid_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var periods []Period
	for rows.Next() {
		var p Period
		if err := rows.Scan(&p.ID, &p.ValidFrom, &p.ValidUntil, &p.Mode, &p.SpreadValue, &p.FixedPrice, &p.Note); err != nil {
			return nil, err
		}
		periods = append(periods, p)
	}
	return periods, rows.Err()
}

// CreatePeriod validates and inserts a new period. Rejects overlaps with
// existing periods: at query time, at most one period covers any given hour.
func CreatePeriod(db *sql.DB, p Period) (int64, error) {
	if err := validatePeriod(p); err != nil {
		return 0, err
	}
	newRange, err := rangeOf(p)
	if err != nil {
		return 0, err
	}

	existing, err := ListPeriods(db)
	if err != nil {
		return 0, err
	}
	for _, e := range existing {
		if p.ID != 0 && e.ID == p.ID {
			continue // update: don't compare the period against itself
		}
		existingRange, err := rangeOf(e)
		if err != nil {
			return 0, err
		}
		if newRange.overlaps(existingRange) {
			return 0, fmt.Errorf("this period overlaps period #%d (%s .. %s)", e.ID, e.ValidFrom, e.ValidUntil)
		}
	}

	// spread_value and fixed_price are always written as given (even if
	// zero): only `mode` decides which one is meaningful on read, so there's
	// no need to null out the other field (a legitimate zero spread for a
	// promotional rate shouldn't get lost to a NULLIF).
	res, err := db.Exec(
		`INSERT INTO pricing_periods (valid_from, valid_until, mode, spread_value, fixed_price, note) VALUES (?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''))`,
		p.ValidFrom, p.ValidUntil, p.Mode, p.SpreadValue, p.FixedPrice, p.Note,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePeriod replaces period p.ID with the new values, with the same
// validation (including the overlap check, excluding itself).
func UpdatePeriod(db *sql.DB, p Period) error {
	if p.ID == 0 {
		return fmt.Errorf("update requires an ID")
	}
	if err := validatePeriod(p); err != nil {
		return err
	}
	newRange, err := rangeOf(p)
	if err != nil {
		return err
	}
	existing, err := ListPeriods(db)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.ID == p.ID {
			continue
		}
		existingRange, err := rangeOf(e)
		if err != nil {
			return err
		}
		if newRange.overlaps(existingRange) {
			return fmt.Errorf("this period overlaps period #%d (%s .. %s)", e.ID, e.ValidFrom, e.ValidUntil)
		}
	}

	_, err = db.Exec(
		`UPDATE pricing_periods SET valid_from = ?, valid_until = NULLIF(?, ''), mode = ?, spread_value = ?, fixed_price = ?, note = NULLIF(?, '') WHERE id = ?`,
		p.ValidFrom, p.ValidUntil, p.Mode, p.SpreadValue, p.FixedPrice, p.Note, p.ID,
	)
	return err
}

// DeletePeriod deletes a period. Past periods are as editable/deletable as
// future ones: this isn't a legal invoice, and fixing a past mistake should
// also affect the historical costs shown.
func DeletePeriod(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM pricing_periods WHERE id = ?`, id)
	return err
}

// Result is the price resolved for a given hour.
type Result struct {
	EurPerKwh float64
	// Complete is false when a PUN reference would be needed but none is
	// available (not even the previous month): the caller must report this
	// as "incomplete", never make up a number.
	Complete bool
	// Provisional is true when the PUN used is the *previous* month's
	// average, because the current month isn't over yet and its real
	// average isn't known: a declared estimate, not missing data. The
	// caller should label it "provisional, pending confirmation at month
	// end", not as an error nor as final data.
	Provisional bool
	Source      string // "fixed_override" | "spread_override" | "default"
}

// Resolve computes the price for instant at. The user's contract bills on
// the **monthly PUN average**, not the hourly market price (even though
// that's what Home Assistant exposes) — see the 2026-09-18 conversation.
// Algorithm: a fixed_override period -> fixed price (independent of PUN); a
// spread_override period -> monthly PUN average + the period's spread; no
// period -> monthly PUN average + defaultSpread. The "monthly PUN average"
// for an already-concluded month is the real average of the collected
// hourly samples; for the current month (not concluded yet, so no final
// real average) the previous month's average is used as a declared
// estimate (Result.Provisional = true), auto-corrected once the current
// month ends.
func Resolve(db *sql.DB, at time.Time, defaultSpread float64) (Result, error) {
	periods, err := ListPeriods(db)
	if err != nil {
		return Result{}, err
	}
	return resolveFromPeriods(db, periods, at, defaultSpread)
}

// ResolvePreview is like Resolve but ignores saved periods and uses override
// in their place, without touching the DB: for the "live" UI preview of a
// period not yet saved.
func ResolvePreview(db *sql.DB, at time.Time, override Period, defaultSpread float64) (Result, error) {
	return resolveFromPeriods(db, []Period{override}, at, defaultSpread)
}

func resolveFromPeriods(db *sql.DB, periods []Period, at time.Time, defaultSpread float64) (Result, error) {
	ts := at.Unix()
	var matched *Period
	for i := range periods {
		r, err := rangeOf(periods[i])
		if err != nil {
			return Result{}, err
		}
		if r.contains(ts) {
			matched = &periods[i]
			break
		}
	}

	if matched != nil && matched.Mode == ModeFixedOverride {
		return Result{EurPerKwh: matched.FixedPrice, Complete: true, Source: ModeFixedOverride}, nil
	}

	spread := defaultSpread
	source := "default"
	if matched != nil {
		spread = matched.SpreadValue
		source = ModeSpreadOverride
	}

	monthStart := startOfMonth(at)
	if monthStart.Before(startOfMonth(time.Now())) {
		avg, ok, err := monthAverage(db, monthStart)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{Complete: false, Source: source}, nil
		}
		return Result{EurPerKwh: avg + spread, Complete: true, Source: source}, nil
	}

	// Current month (or future, for a preview): its real average isn't known
	// yet. Provisional estimate = the effective price at the very last
	// instant of the previous month, whatever its source — a real data
	// average, or a period the user set manually (e.g. the last known bill's
	// price, covering through the end of last month: pricing_periods is
	// already the right tool for this, nothing else is needed). Only one
	// level of recursion: the month before "now" is always concluded.
	prev, err := resolveFromPeriods(db, periods, monthStart.Add(-time.Second), defaultSpread)
	if err != nil {
		return Result{}, err
	}
	if prev.Complete {
		return Result{EurPerKwh: prev.EurPerKwh, Complete: true, Provisional: true, Source: prev.Source}, nil
	}

	// No period and no data average for the previous month either (e.g. a
	// fresh install, the very first month): last resort, the partial average
	// of whatever has already been collected in the current month itself.
	avg, ok, err := monthAverage(db, monthStart)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Complete: false, Source: source}, nil
	}
	return Result{EurPerKwh: avg + spread, Complete: true, Provisional: true, Source: source}, nil
}

func startOfMonth(t time.Time) time.Time {
	lt := t.In(romeLocation)
	return time.Date(lt.Year(), lt.Month(), 1, 0, 0, 0, 0, romeLocation)
}

// monthAverage is the simple average of the pun_prices samples collected in
// the month starting at monthStart. No minimum coverage threshold: if the
// service was down for a long stretch of that month, the average only
// reflects the hours it did collect (ponytail: revisit if this proves
// unrepresentative in practice).
func monthAverage(db *sql.DB, monthStart time.Time) (float64, bool, error) {
	monthEnd := monthStart.AddDate(0, 1, 0)
	var avg sql.NullFloat64
	err := db.QueryRow(`SELECT AVG(pun_eur_kwh) FROM pun_prices WHERE ts >= ? AND ts < ?`, monthStart.Unix(), monthEnd.Unix()).Scan(&avg)
	if err != nil {
		return 0, false, err
	}
	return avg.Float64, avg.Valid, nil
}

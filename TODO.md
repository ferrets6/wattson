# TODO — Wattson

Status tracker. Completed sections are one-line summaries; open items have
enough context to act on. See `PLAN.md` for architecture, `CLAUDE.md` for
binding decisions.

## Done

- **Setup**: Go module, folder layout, `.gitignore`.
- **Storage** (`internal/store`): SQLite (`modernc.org/sqlite`, no CGO),
  embedded migrations, raw tables + hourly rollups from day one.
- **MQTT collector** (`internal/mqtt`): subscribes to `tele/tasmota/SENSOR`,
  auto-reconnect, non-blocking `Start()` (a blocking version once stalled the
  whole HTTP server when the broker was unreachable).
- **Beszel client** (`internal/beszel`): PocketBase login (with
  `_superusers`/`admins` fallback), polls host + per-container metrics.
  No per-container disk I/O (Beszel doesn't expose it); `mem_used` units
  differ between host (GB) and containers (MB) — see migration comments.
- **Home Assistant / PUN client** (`internal/homeassistant`): hourly poll of
  `sensor.pun_orario`, writes to `pun_prices`. Never fabricates a price if
  the entity is missing/unavailable.
- **Attribution engine** (`internal/attribution`, `internal/rollup`): static
  container→category JSON config, hourly rollup with a persisted cursor
  that advances through data gaps (broker downtime) instead of stalling.
  Idle baseline: 10th percentile of `watts_avg` over a trailing 7-day
  window, or a fixed override.
- **Pricing engine** (`internal/pricing`): bills on the **monthly PUN
  average**, not the hourly spot price (Home Assistant exposes hourly, but
  that's not what the user's contract bills on). A concluded month uses its
  real average; the current month uses whatever price was in effect at the
  end of the previous month (real average or a manual period) as a
  provisional estimate, falling back to its own partial average if neither
  is available. Overlapping periods are rejected on write. Past periods
  stay freely editable (not a legal invoice).
- **API + auth** (`internal/api`, `internal/auth`): OIDC login integrated
  into the app (same pattern as `hp-bios-webui`), off by default. Routes:
  `power/current`, `power/history`, `power/summary`, `power/attribution`,
  `pricing/periods` (CRUD), `pricing/preview`.
- **Frontend** (`cmd/wattson/web/`): vanilla JS/HTML/CSS, `embed.FS`, no
  build step. Chart.js via jsdelivr. i18n with a language dropdown
  (`locales/{en,it}.json`, defaults to the browser's language). Separate
  power/energy charts (no dual-axis — the dataviz skill forbids it).
- **Docker**: multi-stage build, static binary, `distroless/static-debian12`
  final image, `time/tzdata` embedded for `Europe/Rome` without system
  zoneinfo.
- **Backfill tool** (`cmd/backfill`): one-off import of pre-existing HA/Beszel
  history, not part of the running service.
- **Published**: `github.com/ferrets6/wattson` (private), deployed on the
  NAS via the `homelab` repo.
- **`SQLITE_BUSY` under concurrent writers** (2026-09-19): `busy_timeout(5000)`
  alone didn't fully eliminate `database is locked (5)` errors in
  production — a lock held a bit too long (e.g. during a WAL checkpoint)
  could still outlast it. Root cause: `database/sql`'s connection pool had
  no cap, so concurrent writers (MQTT collector, Beszel/HA pollers, rollup)
  could each get a *separate* SQLite connection and contend for the single
  writer slot across OS-level locks instead of queuing in-process. Fixed
  with `db.SetMaxOpenConns(1)` in `store.Open` — the standard fix for
  SQLite, which has no real concurrent-writer support regardless of
  application-level pooling.
  That cap exposed two **latent deadlocks** that would otherwise have gone
  unnoticed: `rollup.rollupResources` and `api.summaryFor` each issued a
  second query/exec while their own `*sql.Rows` was still open on the same
  `*sql.DB` — with only one connection available, the nested call blocked
  forever waiting for a connection the still-open `Rows` was pinning. Both
  now fully buffer their `SELECT` results before issuing any further
  query. Verified the failure mode was real (not theoretical) by
  temporarily reverting each fix with the pool cap in place and confirming
  both hang; a concurrency regression test
  (`store.TestConcurrentWritesDoNotFailWithSQLiteBusy`) now guards the
  original bug report.
- **Frontend UX pass** (2026-09-19, on `dev`, tested locally against a real
  read-only DB copy before merge): freshness pulse dot next to "Current
  power" (flashes only on an actual new `ts` from the poll, not on every
  30s tick that sees a stale sample again); "Cost today" replaced by a
  rolling 24h cost (`/power/summary`'s `today` field renamed to `last24h`);
  "cost this month"/"daily average" tile order swapped; mobile layout: first
  4 KPI tiles sit 2-by-2 under 600px, badges shrink to fit; "Hourly energy
  (kWh)" chart replaced with a host CPU usage (%) chart (`power/history` now
  also returns `cpu_avg_pct` per bucket, left-joined from `resource_hourly`);
  the category breakdown table now shows `__baseline__` as an explicit
  "Idle baseline" row instead of excluding it, so the category bar's
  "system" total matches the sum of the container rows; both the category
  and pricing-periods tables now scroll horizontally on narrow screens
  instead of overflowing the page.
- **Live raw chart** (2026-09-19): a new "Live (last 15 min)" section with
  two synced charts (power W, host CPU %) above the hourly ones. New
  `GET /api/v1/power/live` returns raw `power_samples`/`resource_samples`
  (the two series aren't timestamp-aligned — MQTT and Beszel poll
  independently — so each is charted on its own shared time axis rather
  than by matching index), polled every 10s to match the wattmeter's
  publish interval. **Design decision**: a single chart with power and CPU
  sharing one y-axis would either need a second axis — the dataviz skill
  rules out dual-axis charts, since two independently-scaled series on one
  plot invite false "look, they move together" readings — or normalizing
  both to a common index (% of max), which hides the real values behind a
  derived number nobody asked for. Went with two synced charts instead:
  `linkChartsHover()` in `app.js` attaches hover listeners once on the
  canvases (not on the Chart.js instances, which get destroyed/recreated
  on every range switch and live poll) and mirrors the active tooltip
  point onto the other chart by nearest timestamp. Applied to both the
  live pair and the existing hourly power/CPU pair. Verified locally: the
  power side gets real live data (MQTT reaches the broker directly from a
  dev machine); the CPU side's code path is the same one already proven by
  the hourly CPU chart, but couldn't be exercised with fresh raw samples
  in local dev specifically, because `BESZEL_URL=http://beszel:8090` is a
  Docker-internal hostname that only resolves inside the `homelab`
  network — not a bug, just untestable outside the container.
- **Mobile note**: with the live section added, a phone screen now shows 4
  time-series charts before the breakdown/pricing sections (2 live + 2
  hourly). Not addressed yet — revisit spacing/collapsing once it's been
  seen on a real phone (see the "Open" testing note below).

- **Custom date range + zoom on the hourly charts** (2026-09-19): two date
  inputs next to the preset buttons override the range entirely (an empty
  "until" defaults to now); picking a preset clears them back. Zoom/pan use
  `chartjs-plugin-zoom` (+ hammer.js for pinch) on the power/CPU chart pair
  only — the live charts redraw every 10s poll, so a zoom there would just
  get reset before anyone could use it. Zooming or panning either chart
  mirrors the x-axis range onto its pair (same `_pairChart` wiring as the
  hover-sync), plus a "Reset zoom" button. Verified in the browser: wheel
  zoom, cross-chart sync, reset, and preset/custom switching all work.

- **Beszel 1m retention checked (2026-09-19)**: confirmed via the Beszel UI
  at 1 hour. Wattson polls Beszel every 30s (`beszel.Config.PollInterval`
  default, `internal/beszel/client.go:38`), so as long as Wattson itself is
  down for less than an hour, no CPU/RAM data is lost at full granularity —
  the next poll always lands well inside Beszel's own 1m window. A Wattson
  outage longer than that would permanently lose 1-minute detail for the
  part beyond the hour (Beszel would have already rolled it up to 10m by
  the time Wattson polls again), though coarser data would still exist.
  No action needed at Wattson's current poll interval.

## Open
- **Idle baseline calibration (2026-09-19, calculated from real data)**:
  ran the same 10th-percentile calculation `rollup.baselineWatts` uses
  against the local dev DB's real backfilled `power_hourly` data. Only 57
  hours of history exist so far (backfill started 2026-09-16), so the
  trailing-7-day window isn't full yet: p10 = **37.8 W**, min ever observed
  = 36.6 W, median = 38.7 W. That's higher than the ~32 W guessed from
  memory — either the NAS hasn't had a fully idle hour yet in this short
  window (some load nudging every hourly average up), or the 32 W mental
  estimate was from different conditions. Left `FixedBaselineWatts` unset
  (no hardcoded override): baking in a number from 2.4 days of data would
  defeat the point of the dynamic calculation. Revisit once a full 7-day
  window has accumulated (around 2026-09-23) and recheck against a fixed
  32 W idea only if the dynamic value still looks off.

### Data granularity + chart rework (2026-09-20, requested by the user)

- **Increase data granularity** — two parts, not started:
  - **1.1 Live view at 2s resolution**: both power and CPU, for the last
    hour. Needs evaluation on both ends: the Tasmota "Sonoff Dual Meter"
    publishes `tele/tasmota/SENSOR` roughly every ~10s today (a
    `TelePeriod` change would be needed on the device itself, not in
    Wattson); Beszel's own finest resolution is 1m (see the retention note
    above) — its agent's own poll interval would need lowering, if that's
    even configurable, or Wattson would need to poll Beszel's *live* stats
    endpoint (if one exists) rather than its stored `1m` stats. Needs
    research on both the Tasmota and Beszel side before any Wattson code
    changes — don't assume either is a simple config flip. Once live data
    actually arrives at ~2s, the "Current power" freshness pulse dot
    should keep flashing correctly at that rate too (it already flashes on
    every genuinely new `ts`, not on a fixed timer — just needs to be
    re-verified once the faster feed is real, not re-implemented).
  - **1.2 A full week at 1-minute resolution**: today `power_hourly`/
    `resource_hourly` are the only long-term rollup (1 row/hour), which is
    too coarse for a "last 7 days" view. Would need a second rollup tier
    (e.g. `power_1m`/`resource_1m`, pruned after 7-8 days) sitting between
    the raw tables (short retention) and the existing hourly one (kept
    forever) — same idea as Beszel's own multi-tier rollup.
  - **Before implementing either**: estimate the storage impact of the new
    tables over the project's 10-year retention horizon. Not expected to
    be significant (SQLite, same order of magnitude as today), but size it
    with real row-size numbers before committing to the schema, per
    CLAUDE.md's own "SQLite, not Postgres" sizing assumption.
- **Charts should show min/max/avg for the period**, not just a single
  averaged line — reference: the "NAS" statistics-graph card on the user's
  Home Assistant dashboard (a private LAN URL, not reachable to check
  directly; the reference is Home Assistant's built-in "statistics graph"
  card, which bands min/mean/max per bucket). `power_hourly` already
  stores `watts_min`/`watts_max`/`watts_avg` per bucket — this is mostly a
  frontend rework (min/max as a shaded band or two thin lines around the
  average) rather than a backend change, except for the CPU chart, which
  would need `resource_hourly` to gain `cpu_min`/`cpu_max` columns (today
  it only has `cpu_avg`). Not started.
- **Live chart "blinks" on every refresh**: `loadLive()` destroys and
  recreates the chart from scratch every 10s poll, so it visibly empties
  and refills instead of scrolling smoothly like a normal live chart.
  Fix: keep the `Chart` instance alive across polls and update its
  dataset's data array + call `chart.update()` (or append only the new
  points and shift old ones out of the sliding window), instead of
  `destroy()`/`new Chart()` each time. Not started.

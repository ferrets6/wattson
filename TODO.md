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
- **Live chart "blinks" on every refresh (fixed 2026-09-20)**: `loadLive()`
  used to `destroy()`/`new Chart()` on every 10s poll, visibly emptying and
  refilling the canvas. `upsertLineChart()` now creates the chart once and
  updates its dataset's `data` + calls `chart.update('none')` on every
  later poll instead. Verified in the browser: the chart's own `id` stays
  the same across polls while the data keeps advancing — no
  destroy/recreate, no flicker.
- **12h vs 24h time format (fixed 2026-09-20)**: there's no browser API
  that exposes the OS's actual clock-format preference (deliberately
  withheld, a fingerprinting concern) — used the closest available signal,
  `Intl.DateTimeFormat(i18n.intlTag()).resolvedOptions().hourCycle`, via a
  new `use24Hour()` helper. `toLocaleString()`/`toLocaleDateString()` calls
  (tooltips, month label) already followed locale convention automatically
  and needed no change; the real bug was the chart axis *ticks*, which
  `chartjs-adapter-date-fns` formats with hardcoded 12h patterns
  regardless of locale — fixed by setting `time.displayFormats` explicitly
  per `use24Hour()` in `baseLineOptions()`. Verified in the browser: hourly
  and live chart ticks show `HH:mm` in Italian, `h a`/`h:mm a` in English,
  and switching the language dropdown updates both immediately (the live
  charts are now updated in place rather than recreated, so the locale
  switch handler explicitly tears them down once to pick up the new
  format).
- **Charts show min/max/avg for the period (done 2026-09-20)**: reference
  was the "NAS" statistics-graph card on the user's Home Assistant
  dashboard. `power_hourly` already had `watts_min`/`watts_max`; added the
  same to `resource_hourly` (`cpu_min`/`cpu_max`, migration
  `0002_resource_hourly_cpu_minmax.sql`, nullable — existing rows predate
  it) and `rollup.rollupResources` now computes `MIN`/`MAX(cpu_pct)`
  alongside the average. `GET /power/history` returns `cpu_min_pct`/
  `cpu_max_pct` (falling back to `cpu_avg_pct` via `COALESCE` when a bucket
  has no resource data at all, so an empty band doesn't disagree with a
  nonzero-looking average). Frontend: both hourly charts now render a
  three-dataset trio (min/max/avg) via `minMaxAvgDatasets()` — min is an
  invisible boundary, max fills back to it (`fill: '-1'`) for the shaded
  band, avg draws solid on top — with a caption explaining the band.
  `linkChartsHover`'s crosshair sync was generalized to track whichever
  dataset is *last* in the array (the average) instead of assuming index
  0, so it keeps working for both the 3-dataset historical charts and the
  single-dataset live ones. Verified in the browser: band + line render
  correctly, tooltip shows Min/Max/Media together, hover-sync and zoom-sync
  between the power/CPU pair still work with the new dataset count.

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

- **Increase data granularity** — two parts:
  - **1.1 Live view at 2s resolution**: both power and CPU, for the last
    hour. **Research done (2026-09-20), result: blocked on both sides.**
    - **Beszel side — hard no via the REST API.** Queried
      `system_stats` directly (`type='1m'` records): consecutive rows are
      exactly 60.00s apart (`09:18:47.033`, `09:17:47.005`,
      `09:16:47.012`, ...). That's not a display rollup, it's genuinely
      the finest data the hub ever persists — `10m`/`20m`/`120m`/`480m`
      are coarser aggregates of that same 1-per-minute series. Polling
      Beszel's REST API more often than every 60s would just re-read the
      same row; there is no 2s (or even sub-minute) data to fetch. The
      per-system page's live badge *does* visibly update faster than that
      in the UI, which means the agent pushes a faster real-time reading
      over its own websocket channel purely for that "right now" display
      — but that channel isn't a documented/stable data source, isn't
      persisted, and building a collector around it would be brittle. Not
      pursuing 2s CPU via Beszel; would need a fundamentally different
      source (e.g. Wattson reading `/proc` directly on the host, its own
      separate scope decision) if this is still wanted.
    - **Tasmota side — blocked on a stale address, not a research
      dead-end.** CLAUDE.md documents the wattmeter at
      `http://tasmota.example.lan`, but that doesn't respond from any
      vantage point tried (this dev machine, the NAS, the broker host):
      its web port is connection-refused everywhere, and port 80 answers
      with an unrelated nginx 404, so it's likely not even the same
      device. Checked ARP and dnsmasq/Pi-hole leases on the broker host for
      a `tasmota` hostname — nothing found (no mDNS/ARP-scan tooling
      installed there either). The device is still alive and publishing
      (Wattson's own MQTT collector keeps receiving live readings all
      session), so this is just a stale IP, not a dead device. Needs the
      current IP/port from whoever manages the router/DHCP — CLAUDE.md's
      URL should be corrected once known.
    - Once live data actually arrives at ~2s (power side, if unblocked),
      the "Current power" freshness pulse dot should keep flashing
      correctly at that rate too — it already flashes on every genuinely
      new `ts`, not on a fixed timer, so this just needs re-verifying,
      not re-implementing.
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

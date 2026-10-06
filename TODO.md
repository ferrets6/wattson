# TODO — Wattson

Status tracker. Done = one-liners. Open = enough context to act on. See
`PLAN.md` for architecture, `CLAUDE.md` for binding decisions.

## Done

- **Setup**: Go module, folder layout, `.gitignore`.
- **Storage** (`internal/store`): SQLite (`modernc.org/sqlite`, no CGO),
  embedded migrations, raw tables + hourly rollup from day one.
- **MQTT collector**: subscribes to the Tasmota telemetry topic,
  auto-reconnect, non-blocking `Start()`.
- **Beszel client**: PocketBase login, polls host + per-container metrics.
- **Home Assistant / PUN client**: hourly poll of `sensor.pun_orario` into
  `pun_prices`; never fabricates a price if the entity is unavailable.
- **Attribution engine**: container→category config, hourly rollup with a
  persisted cursor, idle baseline (10th percentile over a trailing window,
  or a fixed override).
- **Pricing engine**: bills on the monthly PUN average + spread, period
  overrides (fixed price or custom spread), overlapping periods rejected
  on write.
- **API + auth**: OIDC login (off by default), power/pricing REST routes.
- **Frontend**: vanilla JS/HTML/CSS, `embed.FS`, Chart.js, i18n (en/it,
  browser-language default + switcher).
- **Docker**: multi-stage build, distroless final image, embedded tzdata.
- **Backfill tool**: one-off import of pre-existing HA/Beszel history.
- **Published**: `github.com/ferrets6/wattson`.
- **`SQLITE_BUSY` fix**: capped the connection pool at 1 (SQLite's real
  limit anyway); fixed two deadlocks that cap exposed
  (`rollup.rollupResources`, `api.summaryFor`) by fully buffering a
  `SELECT` before issuing further queries. Regression test added.
- **Frontend UX pass**: freshness pulse on "Current power", rolling-24h
  cost KPI (was "since midnight"), tile reorder, CPU chart in place of the
  energy chart, idle baseline shown as an explicit breakdown row,
  responsive tables on mobile.
- **Live raw chart**: "Live (last 15 min)" section, power+CPU on synced
  charts (`linkChartsHover`, no dual-axis per the dataviz skill), new
  `GET /power/live`.
- **Custom date range + zoom**: date inputs override the presets; zoom/pan
  (`chartjs-plugin-zoom`) on the hourly power/CPU pair, synced between the
  two and resettable. Not on the live charts (redraw too often for zoom to
  help).
- **Live chart flicker**: fixed by updating the chart in place instead of
  destroying/recreating it every poll.
- **12h/24h time format**: chart axis ticks now follow the active locale
  (were hardcoded to English AM/PM).
- **Min/max/avg chart bands**: `power_hourly` already had min/max;
  added `cpu_min`/`cpu_max` to `resource_hourly`. Both hourly charts show
  a shaded min–max band with the average on top.
- **1-minute rollup tier**: `power_minutely`/`resource_minutely` (~8-day
  retention), used automatically by `/power/history` for ranges ≤7 days;
  hourly rollup unchanged beyond that.
- **Live CPU from `/proc`**: `internal/hostcpu` samples host CPU every 2s
  directly, independent of Beszel's ~1-minute poll — only the live chart
  uses it; historical charts and the attribution heuristic still use
  Beszel, which is what they actually need.
- **Beszel retention confirmed**: its finest resolution is 1 minute
  (checked via its own API); Wattson's 30s poll has ample margin.
- **Live-chart fixes (2026-09-20)**: pulse dot now flashes on the live
  feed's own cadence (was tied to a separate 30s poll, so it drifted);
  live charts show min/max/avg bands too (client-side bucketing of the raw
  points, 15s buckets); `/power/live` takes `power_since`/`cpu_since` so
  each 2s poll only fetches new points instead of the whole 15-minute
  window again; zoom on the hourly charts is capped at a 30-minute span
  and the tick scale now auto-adjusts to the zoomed range (was stuck on a
  fixed hour/minute unit); "Today" renamed to "Last 24h" (it was always a
  rolling 24h window, not since-midnight — that mislabeling read as data
  missing before noon); custom range inputs are now datetime-local
  (time-of-day selectable, not just a date), with "until" defaulting to
  now.

## Open

- **Idle baseline**: the dynamic 10th-percentile calc still has under a
  week of real data (currently reads 37.8 W vs. ~32 W expected). Revisit
  once a full week has accumulated, around 2026-09-23.
- **Tasmota `TelePeriod` to 2s**: user is locating the device's current
  address to lower it — no Wattson code changes needed for that part.
- **Host `/proc/stat` access unverified on the real NAS**: works locally
  against a simulated file; standard Docker behavior should expose the
  host's own `/proc/stat` without any mount, but hasn't been confirmed on
  that box yet. `HOSTCPU_PROC_STAT_PATH` + a `/proc:/host/proc:ro` mount
  in the deployment's `docker-compose.yml` is the fallback if it doesn't.
- **Mobile layout**: the live section added a 3rd/4th chart above the
  historical pair (4 time-series charts total on a phone). Not checked on
  a real device yet.

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

## Open

- 🔴 **Custom date range + zoom on charts**: not implemented. Flagged as the
  hardest remaining piece, to be tackled as separate follow-up work.
- Observe Beszel's real raw (`1m`) retention in practice (it already does
  its own internal rollup at 1m/10m/20m/120m/480m) — i.e. confirm how far
  back `resource_samples` at 1-minute granularity actually stays queryable
  on the Beszel side before Wattson's own poll would see gaps. Not started;
  low priority.
- Calibrate the idle baseline value once there's more real data to look at
  (explicitly deferred by the user — revisit only when asked).

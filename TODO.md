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

## Open

- 🔴 **Live chart**: currently only hourly rollups are charted, no raw
  (~10s) live view. Needs its own pass, together with the item below.
- 🔴 **Custom date range + zoom on charts**: not implemented. Flagged as the
  hardest remaining piece, to be tackled as separate follow-up work.
- Observe Beszel's real raw (`1m`) retention in practice (it already does
  its own internal rollup at 1m/10m/20m/120m/480m).
- Calibrate the idle baseline value once there's more real data to look at.

### Frontend UX feedback (2026-09-19, mobile screenshots)

- **"Current power" needs a freshness pulse**: a small dot next to the
  value that blinks/flashes every time a new reading arrives from the
  30s poll, even if the number itself is unchanged — so it's visible that
  it's actually updating, not stuck (distinct from the existing "stale"
  badge, which only fires after 2 minutes with no data at all).
- **"Cost today" is a low-value KPI** — a rolling 24h cost might be more
  useful than "since local midnight" (which is nearly empty right after
  midnight).
- Swap the order of the "cost this month" and "daily average" tiles.
- On mobile, the first 4 KPI tiles should sit two-by-two side by side
  instead of stacking full-width; shrink the rest of the layout and move
  the "estimate" badge if needed to fit.
- **The "Hourly energy (kWh)" chart isn't useful as-is** — consider CPU
  usage instead (part of the charts rework below).
- Since the X axis is always time, a smarter single chart combining power
  draw + CPU usage might read better than two separate ones (part of the
  charts rework below).
- **Breakdown by category/service: the bar's per-category totals don't
  match the sum of the container table rows.** Not a data bug — the
  container table excludes `__baseline__` (it's not a container), but the
  category bar's "system" total *includes* the baseline share, which is
  usually most of it. The UI never shows baseline as a line item, so the
  mismatch looks like an error. Fix: either show baseline as an explicit
  row/label in the table, or clarify in the note that the category totals
  include idle baseline while the container list doesn't.
- The category breakdown table overflows on the right (mobile).
- The pricing periods table isn't properly responsive (mobile).

## Homelab integration (separate session, done from the `homelab` repo)

- `services/wattson/docker-compose.yml` (hp-bios-webui pattern: build from a
  Git URL pinned to a commit SHA of this repo).
- Caddy block `wattson.example.lan` (`lan-only` + `sso`).
- Homepage `customapi` card.
- Uptime Kuma monitor.
- ZFS dataset + sanoid + backup; entries in `docs/services.md`,
  `docs/data-map.md`, `docs/decisions.md`.

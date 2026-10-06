# Architecture plan — Wattson

See `CLAUDE.md` for scope and binding decisions. This file covers technical detail.

## Backend components (Go)

### 1. MQTT collector — `internal/mqtt`

`eclipse/paho.mqtt.golang`, subscribed to the Tasmota's `tele/<topic>/SENSOR` (`MQTT_TOPIC`). Payload:

```json
{"Time":"...","ENERGY":{"Total":[2.694,0.240],"Power":[69,5],
  "Voltage":235,"Current":[0.340,0.047], ...}}
```

Only **index 0** (line 1) of `Power`/`Total`/`Current` is used; `Voltage` is
shared between both lines. Each sample is written to
`power_samples(ts, watts, cumulative_kwh, voltage, current)`.

Auto-reconnect with backoff, handled by the paho client. `Start()` doesn't
block waiting for the first connection attempt — an earlier blocking version
stalled the whole HTTP server's startup when the broker was unreachable.

### 2. Beszel client — `internal/beszel`

PocketBase login (`_superusers`/`admins` fallback for version compatibility)
with admin credentials from `.env`, then periodic polling (default 30s) of
host + per-container metrics into `resource_samples`. Beszel exposes no
per-container disk I/O; `mem_used` units differ between host (GB) and
containers (MB) — see the migration's comments.

### 3. Home Assistant client (PUN) — `internal/homeassistant`

REST API with a long-lived access token, hourly poll of the `pun_sensor`
entity (`sensor.pun_orario`) into `pun_prices(ts, pun_eur_kwh)`. If the
entity is missing/unavailable, the client logs it explicitly and never
writes a made-up price.

### 4. Attribution engine — `internal/attribution` + `internal/rollup`

Static JSON config (not YAML — stdlib `encoding/json` is enough for a map),
path from `ATTRIBUTION_CONFIG_PATH`, never hardcoded. Maps container name →
category (`system`/`user`) + readable label; unmapped containers fall back
to `default_category` (`unknown` if unset). See `attribution.example.json`.

Hourly rollup (not at query time): for each bucket, find the container with
the highest average CPU peak in the window (from `resource_hourly`) and
allocate the power delta above the idle baseline to that container/category;
the share below baseline stays a separate `__baseline__`/`system` bucket, so
the sum always equals that hour's average power. Written to
`attribution_buckets(bucket_start, bucket_end, container, category, watts_allocated)`.

The rollup uses a persisted cursor (`rollup_state`) that advances even
through hours with no raw samples (e.g. MQTT broker down): it never gets
stuck retrying the same hour forever, and the gap stays visible through the
API as a missing `power_hourly` row.

Idle baseline is configurable: default = 10th percentile of `watts_avg`
over a trailing 7-day window (computed in Go, no percentile arithmetic in
SQL), or a fixed override (`FixedBaselineWatts`) — the value itself is to
be calibrated once there's more real data to look at.

### 5. Pricing engine — `internal/pricing`

`pricing_periods` table:

| field | type | notes |
|---|---|---|
| id | int | |
| valid_from | date | inclusive |
| valid_until | date, nullable | nullable = no expiry |
| mode | enum | `spread_override` \| `fixed_override` |
| spread_value | decimal, nullable | used only if mode=spread_override |
| fixed_price | decimal, nullable | used only if mode=fixed_override |
| note | text, nullable | free-text reminder |

Global config: `default_spread_eur_kwh` (default €0.10).

**The PUN used is the monthly average, not the hourly price**: the user's
contract bills on the monthly average, even though Home Assistant exposes
the hourly spot price. "Monthly PUN average" for an hour `h`:

- If `h`'s month is already concluded: the real average of the `pun_prices`
  samples collected that month.
- If `h`'s month is the current one (or future, e.g. for a preview): the
  price in effect at the very last instant of the *previous* month — real
  data average, or a period the user set manually — as a declared estimate,
  auto-corrected once that month ends. If neither is available (e.g. a
  fresh install with no prior history at all), falls back to whatever's
  been collected in the current month itself. Never a made-up value.

Resolution for an hour `h`:

1. Look for a `pricing_period` covering `h`.
2. `mode=fixed_override` → price = `fixed_price`.
3. `mode=spread_override` → price = monthly PUN average for `h`'s month + `spread_value`.
4. No period → price = monthly PUN average + `default_spread_eur_kwh`.

If no PUN reference is available at all (not even a fallback), the price is
reported "incomplete", never estimated.

Overlapping periods are rejected on write (never resolved by runtime
priority); past periods stay freely editable — this isn't a legal invoice.

Cost for a range is always computed at query time from `power_hourly` × the
resolved price per hour, never materialized permanently: a new pricing
period is reflected immediately across all of history, no reprocessing job needed.

### 6. REST API + auth — `internal/api` + `internal/auth`

**Auth (SSO)**: same pattern as `hp-bios-webui` — OIDC against Authelia
integrated into the app itself (not Caddy forward-auth). Env:
`OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
`OIDC_REDIRECT_URL`, `SESSION_SECRET_KEY`. `/login` starts authorization-code
+ PKCE, `/callback` completes it and sets a signed HMAC/httponly/Secure
session cookie (stateless, no server-side store, sliding idle timeout),
`/logout` clears it. **Every** route (frontend + all of `/api/*`, including
read-only ones) requires a valid session, no exceptions. Off by default
(`OIDC_ISSUER_URL` empty) for anyone already using a forward-auth, and for
local development.

- `GET /api/v1/power/current` — latest sample, `stale: true` if older than 2 minutes.
- `GET /api/v1/power/history?from=&to=` — minute-level rollup for ranges up
  to 7 days, hourly beyond that (same response shape either way); includes
  host CPU avg/min/max alongside power.
- `GET /api/v1/power/live` — raw power + host CPU (from `/proc`, not
  Beszel) for the last 15 minutes, for the live dashboard chart.
- `GET /api/v1/power/summary` — kWh and cost for the last 24h/this month
  (Europe/Rome calendar boundaries), `complete: false` if the PUN is
  missing for any hour.
- `GET/POST/PUT/DELETE /api/v1/pricing/periods` — full CRUD, validation and
  overlap rejection live in `internal/pricing`.
- `GET /api/v1/pricing/preview?from=&to=&spread=|fixed_price=` — recomputed
  cost for a hypothetical price over a date range, without saving anything.
- `GET /api/v1/power/attribution?from=&to=` — category/container breakdown
  from `attribution_buckets` (`__baseline__` included as an explicit row,
  capped at 15 containers).

### 7. Storage — `internal/store`

SQLite via `modernc.org/sqlite` (pure Go, no CGO to compile in Docker).
Embedded SQL migrations, no heavyweight framework. Raw samples (~every
2-10s) kept for a short window (7 days) and feed the live view; rolled up
into a minute-level tier (~8 days, for "last week" charts) and an hourly
one (kept forever).

## Frontend (lightweight SPA) — `cmd/wattson/web/`

Served as static files from the same Go binary (`embed.FS`, see
`cmd/wattson/web.go`). Vanilla JS/HTML/CSS, no build step. Chart.js via
jsdelivr (cdnjs returned 503 during browser testing — worth remembering if
the CDN misbehaves in production).

Sections:
- Range picker (today/7 days/30 days — no custom range yet, YAGNI until
  actually needed).
- **Two separate charts**, power (W) and hourly energy (kWh), **not
  overlaid**: the `dataviz` skill forbids dual-axis charts, so the original
  idea of overlaying cost on the power chart became two side-by-side charts
  instead.
- Category breakdown (horizontal stacked bar, 3 fixed categories with fixed
  identity colors) + a container table (past ~7 entries a table is the
  right form, not a chart) — always labeled as an estimate.
- Pricing periods panel: list, create/edit/delete, with a debounced live
  preview against `/pricing/preview` as the user edits, before saving.
- KPIs: current power (stale flag), cost today/this month (incomplete
  flag), daily average, current effective price.
- i18n: a language dropdown (`locales/{en,it}.json`), defaults to the
  browser's language, overridable and persisted in localStorage.

Palette and mark spec from the `dataviz` skill's reference palette,
validated with `validate_palette.js` in both light and dark mode.

## Open items

- Beszel's real raw (`1m`) retention — it already rolls up internally at
  1m/10m/20m/120m/480m; observe how long it keeps the raw resolution once
  there's more history to look at.
- Idle baseline value — the mechanism is implemented, the number itself
  needs calibrating against real data.

# Wattson

Home NAS power consumption dashboard: a single Go binary that collects
power data from a Tasmota wattmeter over MQTT, correlates it with Beszel's
CPU/RAM/disk/network metrics, estimates a user/service attribution by
heuristic, computes the cost in euros with a configurable PUN+spread
pricing model with period overrides, and exposes all of it via a REST API +
an SPA frontend served by the same binary/container.

Deployed with the same pattern as `hp-bios-webui`: a Docker build from a
**Git context pinned to a commit SHA** (no ghcr publishing, no CI to
maintain).

See `PLAN.md` for the full architecture and `TODO.md` for implementation
order/status. This file gets updated when the scope changes durably (not
for progress status — that lives in `TODO.md`).

## Why it exists

The NAS has no real power consumption data otherwise: Beszel
covers CPU/RAM/disk/network/ZFS but not watts. The goal is to see real
consumption over time, correlated to what's causing it, with the cost in
euros computed from a price that can be automatic or manually overridden
for specific periods (including future ones, with an expiry).

## Binding decisions (don't relitigate without asking)

- Custom service, not Grafana+Prometheus (RAM, UX, live price editing).
- Power ingestion via **MQTT subscription**, not HTTP polling of the
  Tasmota. The wattmeter (a Tasmota "Sonoff Dual Meter") already publishes
  telemetry every ~10s to a Mosquitto broker on the LAN (managed outside
  this repo). Wattson uses a dedicated Mosquitto user with a read-only ACL
  on the single telemetry topic; broker address, user, and topic come from
  `.env`. **Note**: Mosquitto's ACL is global for the instance — any user
  not listed in the ACL file loses all access as soon as the ACL is active,
  so the users HA and the Tasmota connect with must stay listed with full
  access. Use only **line 1**
  (`ENERGY.Power[0]`, `Total[0]`, `Voltage`, `Current[0]`) — line 2 is a
  different load, ignore it.
- **PUN**: no GME scraping in Go. Read the value from a Home Assistant
  sensor created by the `virtualdj/pun_sensor` HACS integration (via HA's
  REST API with a long-lived token), which already fetches/parses GME data
  (quarter-hourly since 2025).
- **The user's contract bills on the monthly PUN average, not the hourly
  market price** (clarified 2026-09-18 — Home Assistant exposes the real
  hourly PUN, but that's not what ends up on the bill). Final price =
  monthly PUN average + default spread (€0.10, configurable via
  `DEFAULT_SPREAD_EUR_KWH`). For an already-concluded month the average is
  the real one from collected hourly samples; for the current month (not
  concluded yet, no known real average) the **effective price at the very
  last instant of the previous month** is used as a declared estimate —
  whatever its source, a real data average or a custom period the user set
  by hand (bootstrap for a fresh install with no history: a `fixed_override`
  period with the last known bill's price, through the end of last month,
  is enough — `pricing_periods` is already the right tool, nothing else is
  needed). Never a made-up value; auto-corrected once the month concludes.
  Overridable by **periods** (past or future, with an expiry) that can set
  either a custom spread or an entirely custom price (no PUN, no spread).
  If an hour isn't covered by any period, the default is used.
  **Overlapping periods are rejected on write** (never resolved by a
  runtime priority): at query time at most one period covers any given
  hour, no ambiguity. Past periods stay freely editable/deletable (this
  isn't a legal invoice). Period dates are interpreted as Europe/Rome
  calendar days. Implemented in `internal/pricing`.
- User/service attribution: **heuristic by time correlation** with
  Beszel's per-container metrics (an estimate, never presented as an exact
  measurement in the UI). Container→category mapping (`system`/`user`) is
  configurable, never hardcoded in Go.
- Stack: **Go** for the backend (including the MQTT collector and the Home
  Assistant client), **SQLite** for storage (same order of magnitude as
  Beszel/Nuno — no Postgres), lightweight SPA frontend served by the same
  binary (`embed.FS`, no separate Node server).
- Code, comments, and documentation are in **English**. The frontend UI has
  its own i18n (English/Italian for now, `cmd/wattson/web/locales/`),
  defaulting to the browser's language and overridable from a dropdown.
- Separate GitHub repo: `github.com/ferrets6/wattson` (same account as
  `hp-bios-webui`).

## Things that stay outside this repo (fetched separately, never committed here)

- Real credentials/host/topic for Home Assistant's MQTT broker.
- Home Assistant API long-lived token for reading `pun_sensor`.
- Beszel admin credentials (`BESZEL_ADMIN_EMAIL`/`PASSWORD`, the existing
  Beszel instance's ones — only copied into Wattson's `.env`, never
  regenerated).
- Network addresses, ports, MQTT user/topic, HA entity IDs of the real
  setup: `.env` only. Code comments and `.env.example` use placeholders.

All of these go in `.env` (gitignored), never in plain text in code or commits.

## Conventions

- No secrets in the repo: `.env` gitignored, `.env.example` documented.
- No `latest` tags for base Docker dependencies in the `Dockerfile`.
- The container publishes no host port: reachable only via the reverse
  proxy (LAN-only + SSO).
- If a design decision gets reversed, update `PLAN.md` and this file — don't
  let them describe an architecture that no longer exists.

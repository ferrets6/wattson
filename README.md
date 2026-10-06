# Wattson

Power consumption dashboard for a home NAS/homelab. A single Go binary
that:

- reads power telemetry from a Tasmota wattmeter over MQTT,
- correlates it with per-container CPU/RAM/network metrics from
  [Beszel](https://beszel.dev) to estimate which service is causing a given
  power draw (a heuristic, never an exact measurement),
- computes the cost in euros from the monthly PUN average (read from a
  Home Assistant `pun_sensor` entity) plus a spread, overridable for
  specific date ranges,
- serves a small SPA dashboard (charts, cost breakdown, price period
  editor) from the same binary.

Built for one specific home setup (see [`CLAUDE.md`](CLAUDE.md)); everything
setup-specific comes from `.env`. Published so the pieces are reusable, not
as a turnkey product.

## How it works

```
Tasmota wattmeter --MQTT--> power_samples ---\
Beszel (PocketBase API) --> resource_samples --+--> hourly rollup --> attribution + pricing --> REST API --> SPA
Home Assistant (pun_sensor) --> pun_prices ---/
```

Raw samples are kept for 7 days; hourly rollups are kept forever. See
[`PLAN.md`](PLAN.md) for the full architecture.

## Running it

Requires Go 1.27+.

```sh
cp .env.example .env   # fill in the values for your setup, see below
go run ./cmd/wattson
```

The dashboard is served at `http://localhost:8080/` (or `$LISTEN_ADDR`).

### Configuration

All configuration is environment variables — see
[`.env.example`](.env.example) for the full list with comments. The
required ones to get any data flowing:

| Variable | What it's for |
|---|---|
| `MQTT_BROKER_URL`, `MQTT_USERNAME`, `MQTT_PASSWORD`, `MQTT_TOPIC` | Power telemetry from the Tasmota wattmeter |
| `BESZEL_URL`, `BESZEL_ADMIN_EMAIL`, `BESZEL_ADMIN_PASSWORD` | Per-container metrics for the attribution heuristic |
| `HA_URL`, `HA_TOKEN`, `HA_PUN_ENTITY_ID` | Monthly PUN average for cost calculation |

Everything degrades gracefully and independently: if Beszel or Home
Assistant are unreachable, the server still starts and serves whatever data
it does have, logging the failure instead of crashing or inventing numbers.

Optional:

- `ATTRIBUTION_CONFIG_PATH` — path to a JSON file mapping container names to
  a category (`system`/`user`) and a display label, for the cost breakdown.
  Copy [`attribution.example.json`](attribution.example.json) as a
  starting point and adjust it to your own containers. Unmapped containers
  fall back to an "unknown" category.
- `OIDC_ISSUER_URL` (+ `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
  `OIDC_REDIRECT_URL`, `SESSION_SECRET_KEY`) — enables login against an
  OIDC provider (e.g. Authelia), integrated into the app itself. Leave
  unset if you're already running this behind a forward-auth proxy.
- `DEFAULT_SPREAD_EUR_KWH` — spread added to the PUN average when no
  pricing period overrides it (default `0.10`).
- `HOSTCPU_PROC_STAT_PATH` — path to read for the live CPU chart (default
  `/proc/stat`, which is the host's own view in a normal Docker setup).

### One-off history import

If your wattmeter/Beszel already have history from before Wattson started
running, `cmd/backfill` imports it once:

```sh
go run ./cmd/backfill --since 2026-01-01T00:00:00Z
```

To fill a gap in the CPU history only, bound it with `--until` and read
Beszel's coarser resolutions (it keeps fine ones only briefly), coarsest
first: `--since … --until … --beszel-resolution 480m,120m`. Each coarse
record is spread over the hours it covers. Stop the running service first:
the rollup it triggers holds the database long enough to make live writes
fail.

It reads the Tasmota's power history from Home Assistant, so it also needs
`HA_TASMOTA_ENTITY_PREFIX` (see `.env.example`). Not part of the running
service — run it by hand when needed. See its
`--help` for options (Beszel resolution, etc).

## Docker

```sh
docker build -t wattson .
docker run --env-file .env -p 8080:8080 -v wattson-data:/home/nonroot wattson
```

The image is a minimal `distroless` static binary with the frontend
embedded; no separate volume is needed beyond the SQLite database.

## Development

```sh
go test ./...
```

Tests that need a real Beszel/Home Assistant instance are gated behind
`RUN_BESZEL_LIVE_TEST=1` / `RUN_HA_LIVE_TEST=1` and skipped by default.

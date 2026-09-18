-- power_samples: raw, short retention (e.g. 7 days), pruned by a periodic job.
CREATE TABLE power_samples (
    ts             INTEGER PRIMARY KEY, -- unix seconds
    watts          REAL NOT NULL,
    cumulative_kwh REAL NOT NULL,
    voltage        REAL NOT NULL,
    current        REAL NOT NULL
);

-- power_hourly: long-term rollup, never pruned.
CREATE TABLE power_hourly (
    bucket_start INTEGER PRIMARY KEY, -- unix seconds, hour-aligned
    watts_avg    REAL NOT NULL,
    watts_min    REAL NOT NULL,
    watts_max    REAL NOT NULL,
    kwh          REAL NOT NULL,
    voltage_avg  REAL NOT NULL,
    current_avg  REAL NOT NULL,
    sample_count INTEGER NOT NULL
);

-- resource_samples: raw per-container/host from Beszel, same short retention
-- as power_samples. container = Docker container name, or '__host__' for
-- aggregate system metrics. mem_used is Beszel's native unit (GB for
-- '__host__', MB for containers — Beszel itself is inconsistent between the
-- two). disk_io_bytes is '__host__' only: Beszel doesn't expose per-container
-- disk I/O.
CREATE TABLE resource_samples (
    ts             INTEGER NOT NULL,
    container      TEXT NOT NULL,
    cpu_pct        REAL NOT NULL,
    mem_used       REAL NOT NULL,
    net_sent_bytes REAL NOT NULL,
    net_recv_bytes REAL NOT NULL,
    disk_io_bytes  REAL,
    PRIMARY KEY (ts, container)
);

-- resource_hourly: long-term rollup, same conventions as above.
CREATE TABLE resource_hourly (
    bucket_start       INTEGER NOT NULL,
    container          TEXT NOT NULL,
    cpu_avg            REAL NOT NULL,
    mem_used_avg       REAL NOT NULL,
    net_sent_bytes_avg REAL NOT NULL,
    net_recv_bytes_avg REAL NOT NULL,
    disk_io_bytes_avg  REAL,
    PRIMARY KEY (bucket_start, container)
);

-- attribution_buckets: attribution rollup result, hourly by design, never pruned.
CREATE TABLE attribution_buckets (
    bucket_start    INTEGER NOT NULL,
    bucket_end      INTEGER NOT NULL,
    container       TEXT NOT NULL,
    category        TEXT NOT NULL,
    watts_allocated REAL NOT NULL,
    PRIMARY KEY (bucket_start, container)
);

-- pricing_periods: few rows, hand-written. valid_from/valid_until are
-- calendar dates (whole day, Europe/Rome), resolved in Go, not SQL — no
-- date/timezone arithmetic here. Overlaps are rejected on write (see
-- internal/pricing), so at most one period covers any given hour.
CREATE TABLE pricing_periods (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    valid_from   TEXT NOT NULL, -- ISO 8601 date (YYYY-MM-DD), inclusive
    valid_until  TEXT,          -- ISO 8601 date, inclusive; NULL = no expiry
    mode         TEXT NOT NULL CHECK (mode IN ('spread_override', 'fixed_override')),
    spread_value REAL,          -- used only if mode = spread_override
    fixed_price  REAL,          -- used only if mode = fixed_override
    note         TEXT,          -- free-text user reminder, optional
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

-- pun_prices: PUN value read from Home Assistant, hourly/quarter-hourly granularity.
CREATE TABLE pun_prices (
    ts          INTEGER PRIMARY KEY, -- unix seconds, start of hour/quarter
    pun_eur_kwh REAL NOT NULL
);

-- rollup_state: cursor for the hourly rollup job. Always advances, even for
-- an hour with no raw samples (e.g. broker down): avoids getting stuck
-- retrying the same data gap forever.
CREATE TABLE rollup_state (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);

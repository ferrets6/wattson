-- Second rollup tier between the raw tables and the hourly one: the
-- hourly rollup is too coarse for "last week" chart requests. Pruned
-- after ~8 days (rollup.Config). Only avg/min/max, no voltage/mem/net/kwh
-- -- nothing queries those at minute resolution.
CREATE TABLE power_minutely (
    bucket_start INTEGER PRIMARY KEY, -- unix seconds, minute-aligned
    watts_avg    REAL NOT NULL,
    watts_min    REAL NOT NULL,
    watts_max    REAL NOT NULL,
    sample_count INTEGER NOT NULL
);

CREATE TABLE resource_minutely (
    bucket_start INTEGER NOT NULL,
    container    TEXT NOT NULL,
    cpu_avg      REAL NOT NULL,
    cpu_min      REAL NOT NULL,
    cpu_max      REAL NOT NULL,
    PRIMARY KEY (bucket_start, container)
);

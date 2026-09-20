-- Raw host CPU samples read directly from /proc/stat every ~2s (see
-- internal/hostcpu), independent of Beszel's own ~1-minute polling. Only
-- feeds the live chart, where a CPU line stepping once a minute next to a
-- power line jittering every 2s looked wrong side by side. The historical
-- hourly/minutely CPU rollups still come from Beszel (resource_samples),
-- unchanged -- this table only exists for the live (last 15 min) view.
CREATE TABLE host_cpu_samples (
    ts      INTEGER PRIMARY KEY, -- unix seconds
    cpu_pct REAL NOT NULL
);

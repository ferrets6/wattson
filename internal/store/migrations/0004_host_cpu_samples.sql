-- Raw host CPU from /proc/stat every ~2s (internal/hostcpu), independent
-- of Beszel's ~1-minute polling. Feeds only the live chart; historical
-- rollups still use Beszel's resource_samples.
CREATE TABLE host_cpu_samples (
    ts      INTEGER PRIMARY KEY, -- unix seconds
    cpu_pct REAL NOT NULL
);

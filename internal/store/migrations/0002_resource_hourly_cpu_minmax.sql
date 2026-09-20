-- power_hourly already has watts min/max for the chart's min-max band;
-- resource_hourly only had cpu_avg. Nullable: old rows predate this and
-- fall back to cpu_avg in the API.
ALTER TABLE resource_hourly ADD COLUMN cpu_min REAL;
ALTER TABLE resource_hourly ADD COLUMN cpu_max REAL;

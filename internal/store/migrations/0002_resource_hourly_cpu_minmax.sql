-- Charts show a min/max band around the hourly average, not just a single
-- line; power_hourly already has watts_min/watts_max, resource_hourly only
-- had cpu_avg. Nullable: existing rows predate this column and have no
-- min/max, the API falls back to cpu_avg for those (a collapsed band).
ALTER TABLE resource_hourly ADD COLUMN cpu_min REAL;
ALTER TABLE resource_hourly ADD COLUMN cpu_max REAL;

-- Read replica grants (scaling plan Phase 2 + part B). Run as superuser
-- on the cluster database. Idempotent; re-running is a no-op.
--
-- The allowlist rule lives in code (readDB comment in
-- internal/storage/postgres.go): dashboard/report reads AND the standing
-- parity check may use the replica. Enforcement, quota and the Recent
-- feed never do.
--
-- GOTCHA that shipped broken once: usage_hourly was added to this list
-- only in part B — ParityReport reads the rollup table THROUGH the
-- replica, and the first maintenance tick failed with
-- "permission denied for table usage_hourly". Any new table the
-- read path learns to read (see rollupXSQL constants) must be granted
-- here at the same time.

GRANT SELECT ON usage_events    TO metering_reader;
GRANT SELECT ON model_pricing   TO metering_reader;
GRANT SELECT ON user_profiles   TO metering_reader;
GRANT SELECT ON usage_hourly    TO metering_reader;

-- Self-validation: must return exactly these four tables and nothing
-- else (quota tables, rollup_meta and sequences stay primary-only).
--   SELECT table_name FROM information_schema.role_table_grants
--    WHERE grantee = 'metering_reader' GROUP BY 1 ORDER BY 1;

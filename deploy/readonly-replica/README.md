# Read-only replica access (scaling plan Phase 2, part B refresh)

Provisions the `metering_reader` role and its table grants so the
dashboard read pool (`READ_DATABASE_URL`) can point at the CNPG read
service without ever reaching a write path. This directory is the
source of truth — the cluster state must match it after every
provision, and grants for any NEW table the read path learns to read
belong here, not in an imperative `psql` line.

## Apply (idempotent)

1. `oc apply -n ai-gateway-dogfood -f 10-databaserole.yaml`
   (CNPG reconciles the login role; wait for `APPLIED=true`)
2. Get the generated password:
   `oc get secret metering-reader -n ai-gateway-dogfood -o jsonpath='{.data.password}'`
3. Table grants as superuser:
   `oc exec aigateway-pg-1 -n <ns> -c postgres -- psql -U postgres -d aigateway -f - < 20-grants.sql`
4. The app secret (`metering-readonly-db-url`) holds
   `READ_DATABASE_URL` = the METERING_DB_URL value with the host
   replaced by `aigateway-pg-r` (CNPG managed read service — falls back
   to the primary during failover). Never commit secret values.

## Verify

- Grants: run the self-validation query at the bottom of `20-grants.sql`
  — exactly `model_pricing, usage_events, usage_hourly, user_profiles`.
- Permission probes: as `metering_reader`, SELECT on each granted table
  must succeed; INSERT and access to quota tables / `rollup_meta` must
  be denied.
- App: boot log shows `read replica enabled`; `/api/v1/admin/rollups`
  shows `parity_healthy` going true within one tick — that check reads
  BOTH `usage_events` and `usage_hourly` through this role.

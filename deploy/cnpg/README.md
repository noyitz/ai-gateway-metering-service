# CloudNativePG on ai-gateway-dogfood

## Why the operator is a vendored manifest, not an OLM Subscription

The `cloud-native-postgresql` listing in this cluster's `certified-operators`
is EDB's commercial repackage: its CSV pulls
`docker.enterprisedb.com/k8s/edb-postgres-for-cloudnativepg` and expects a
`postgresql-operator-pull-secret` that requires EDB entitlements this
account doesn't have. Applying the Subscription installs fine and then sits
in `ImagePullBackOff`.

The community operator (Apache 2.0, same CRDs — `postgresql.cnpg.io/v1`)
ships as a plain manifest with public ghcr images, so that's what we run:
`cnpg-operator-1.30.0.yaml`, downloaded from the v1.30.0 GitHub release. It
installs cluster-scoped into `cnpg-system` and reconciles Clusters in every
namespace, so no OperatorGroup is involved.

Install:

```bash
oc apply -f deploy/cnpg/cnpg-operator-1.30.0.yaml
oc -n cnpg-system wait deploy/cnpg-controller-manager --for=condition=Available --timeout=5m
```

Upgrade = download the newer release's `cnpg-<version>.yaml` from
https://github.com/cloudnative-pg/cloudnative-pg/releases, apply, and only
then bump `spec.imageName` on the Cluster (operator before instance, always).

Note: Postgres images are pinned per-cluster via `spec.imageName`
(`ghcr.io/cloudnative-pg/postgresql:16.12`), independent of the operator
version — upgrades are explicit, nothing drifts.

## Apply order for the rest

```bash
oc apply -f deploy/cnpg/10-cluster.yaml            # needs aigateway-db-app first (docs/db-backup.md)
oc apply -f deploy/cnpg/20-scheduled-backup.yaml
# 30-restore-job.yaml is one-shot, cutover only — see docs/db-backup.md
```

## Manifest inventory

Steady state (keep applied):

| File | What |
|------|------|
| `cnpg-operator-1.30.0.yaml` | vendored community operator (see above) |
| `10-cluster.yaml` | the `aigateway-pg` Cluster (3 instances, WAL+backup config) |
| `20-scheduled-backup.yaml` | ScheduledBackup to COS; the RPO story |

Cutover / DR one-shot jobs (`oc create -f`, applied 2026-09-16 for the
postgresql-0 → CNPG migration, kept as operational history and recipes):

| File | Kind | Notes |
|------|------|------|
| `25-schema-wipe-job.yaml` | **DESTRUCTIVE** | empties `aigateway` on CNPG so `30-restore-job` can load a dump; never run against live traffic |
| `30-restore-job.yaml` | write | pg_restore of the cutover dump |
| `35-parity-check-job.yaml` | read-only | true COUNT(*) parity old-vs-new over every table — the number the cutover decision was made on (`n_live_tup` is an estimate and lies) |
| `40-backfill-usage-events.yaml` | **SUPERSEDED — DO NOT RUN** | awk multiset-diff was inverted and duplicated 114 rows on two runs; kept only as an incident record |
| `45-debug-rowdiff.yaml`, `51-debug-id-ranges.yaml`, `52-debug-idwindow.yaml` | read-only | debug jobs from that incident |
| `50-repair-usage-events.yaml` | write | the actual repair (deleted dup ids 32852..32965, backfilled the real 17 old-only rows) |
| `60-usage-heartbeat.yaml` | read-only | "is live traffic still landing?" probe |

Also part of the backup story, one level up: `../provision-cos-backup-target.sh`
(creates the COS service credential + bucket + `cnpg-backup-cos` secret —
idempotent, reads keys from the cluster, writes nothing to disk) and
`../postgres-backup-stopgap.yaml` (pre-CNPG `pg_dump` cronjob, retained
as a belt-and-braces recipe). No credentials live in any of these files —
everything flows through Secrets.

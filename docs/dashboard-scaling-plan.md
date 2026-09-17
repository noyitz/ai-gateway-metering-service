# Dashboard scaling plan — 30 → 100+ users, zero new hardware

Status: PROPOSAL (2026-09-17). Driven by: dashboard lagging at ~30 users;
pg-1 observed spiking to 758m of a 1-core CPU limit on dashboard reads
while pg-2/pg-3 replicas serve nothing (7m). Nodes at 6–16% CPU — the
cluster isn't the constraint; the read architecture is.

## Diagnosis (from the code, not vibes)

1. **Every refresh recomputes everything.** Each of the 6 dashboard
   endpoints (`cmd/main.go:190-195`) re-aggregates raw `usage_events` for
   the whole window per call. `GetDashboardUsers`
   (`internal/storage/postgres.go:376`) is a five-CTE query re-evaluating
   the full cost expression per (user, model) per request.
2. **Cost is computed at read time**, joining `model_pricing` every row
   every refresh (`costUSDExpr`, postgres.go) — never stored.
3. **No cache, no sharing.** N users × 4+ dashboards × 30s polling
   (`dashboard.html:912` + admin/user-dashboard timers) = dozens of
   identical heavy aggregates per cycle, all serialized on pg-1.
4. **Replicas idle.** All reads hit the CNPG primary.

## Phase 1 — response cache on the dashboard API (highest impact, lowest risk)

**Change:** in-memory TTL + single-flight cache wrapping the 6
`/api/v1/dashboard/*` handlers. Key = request path + query params +
auth scope marker; TTL ~20s (below the 30s poll, so steady-state = one
computation per window per cycle); single-flight collapses the
thundering herd when a key expires.

- Where: `internal/handler` — middleware added *outside* the existing
  auth wrappers, registered only on the 6 `/api/v1/dashboard/*` routes.
  Auth still runs per request (a cache hit must never skip authz).
- Key: method + path + query params + authz scope marker. Only
  responses byte-identical for all callers of the same scope are
  eligible; per-user scoped responses stay uncached this phase.
- What's cached: `200` responses only — marshalled JSON bytes +
  content-type. Errors and 3xx/4xx are never cached.
- Deps: `golang.org/x/sync/singleflight` (BSD-3) +
  `hashicorp/golang-lru/v2` (MPL-2.0) — no hand-rolled flight group or
  eviction list. Fixed-size LRU ≤512 entries (results are small JSON
  blobs; a few MB worst case against the 128Mi limit). Bounded buffers,
  no unbounded maps.
- Staleness ≤ TTL: invisible to users already polling every 30s.
- Expected effect (estimate): dashboard DB load becomes ~flat in
  viewer count — one aggregate per filter-combo per TTL regardless of
  whether 5 or 500 tabs are open.
- **Verify:** unit tests (hit / expiry / key isolation between filter
  combos); `oc adm top pods aigateway-pg-1` before/after during a busy
  hour — expect the 758m-class spikes to flatten substantially.
- **Rollback:** one config knob (cache enabled flag) → redeploy.

## Phase 2 — read-replica DSN

**Change:** optional `READ_DATABASE_URL`; when set, all read-path
`Get*` functions use a second pool; unset = today's behavior (zero-config
compat). Point it at the CNPG read service (`aigateway-pg-r`), which
routes to pg-2/pg-3. Writes (`InsertEvent`) never leave the primary.

- `Recent` panel: measure replication lag first; if >2s is visible, pin
  that one handler to the primary (it's a cheap indexed `ORDER BY
  timestamp DESC LIMIT n`).
- **Verify:** dashboard query load appears on pg-2/pg-3 in `oc adm top`;
  recent-activity rows land within lag tolerance; failover behavior
  documented (CNPG promotes a replica; the `-r` service follows).
- **Rollback:** unset the env var.

## Phase 3 — write-time cost + hourly rollups (the real 100+ fix)

Makes dashboard cost **flat in total event volume**.

1. **`cost_usd` column on `usage_events`**, computed at insert from the
   cached `internal/pricing` table (`InsertEvent`, postgres.go:80). One-time
   batched backfill of existing rows using the existing `costUSDExpr` —
   so new rows and historical rows agree by construction.
2. **`usage_hourly`** maintained in the same transaction as the insert:
   PK `(hour, username, group_name, model, provider)`; sums: requests,
   prompt/completion/total/cached/cache-creation tokens, `cost_usd`.
   Upsert via `ON CONFLICT DO UPDATE` — safe with metering-service and
   metering-service-shadow both writing the same DB.
3. **Switch overview / groups / users / models / timeline reads to
   `usage_hourly`** (sum of pre-aggregated rows; timeline's
   `date_trunc` becomes trivial). `recent` and drill-downs stay on raw —
   raw remains the source of truth.
4. **Pricing-change semantics decision (needs Yos's sign-off):** with
   write-time cost, editing `model_pricing` no longer rewrites history on
   the dashboard (today it does). Mitigation: a `rebuild-rollups` admin
   action that recomputes `cost_usd` + `usage_hourly` from raw using
   current prices. Raw + new prices can always regenerate everything.
5. Parity gate before switching reads: shadow-compute both paths against
   prod data and diff (same window, all endpoints) — numbers must match
   to the cent, then flip.
- **Verify:** parity diff green; dashboard query latency at the 30-day
  window; upsert correctness under concurrent writes (integration test
  with two writers).
- **Rollback:** reads flip back via config flag; the rollup table is
  additive, raw path untouched until parity passes.

## Deliberately NOT in this plan

- Raising CPU/memory limits or nodes — the constraint is query volume,
  not capacity; nodes have 6–16% used. Revisit only if phases 1–3 land
  and pg-1 still spikes.
- NATS/streaming dashboards — one system of record (Postgres), one write
  path, one read path.
- `pg_stat_statements` via CNPG config is worth enabling for measurement
  (config-only, no hardware) but not required to ship any phase.

## Sizing sanity (label: estimate)

Current ≈ 500 events/hr at 30 users ≈ 11M events/yr worst case.
`usage_hourly` at 100 users ≈ users×models×24 rows/day ≈ low tens of
thousands of rows/day — the dashboard sums thousands of rows instead of
millions. That's the whole trick.

## Sequencing

Phase 1 (≈ half day) → Phase 2 (≈ 2–3h + deploy) → Phase 3 (≈ 1–2 days
incl. backfill + parity). Each independently deployable and revertible;
1+2 alone should make 100 users feel acceptable, 3 makes it feel free.

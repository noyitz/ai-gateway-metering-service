# Dashboard scaling plan — 30 → 100+ users, zero new hardware

Status: PROPOSAL rev2 (2026-09-18) — revised per @noyitz's PR #18 review
(items 1–6 + minors folded in; sign-off pending on re-review). Original
2026-09-17. Driven by: dashboard lagging at ~30 users; pg-1 observed
spiking to 758m of a 1-core CPU limit on dashboard reads while pg-2/pg-3
replicas serve nothing (7m). Nodes at 6–16% CPU — the cluster isn't the
constraint; the read architecture is.

References below name symbols, not line numbers — they drift with every
quota commit.

## Diagnosis (from the code, not vibes)

1. **Every refresh recomputes everything.** Each of the 6 dashboard
   endpoints (the `/api/v1/dashboard/*` block in `cmd/main.go`)
   re-aggregates raw `usage_events` for the whole window per call.
   `GetDashboardUsers` (`internal/storage`) is a five-CTE query
   re-evaluating the full cost expression per (user, model) per request.
2. **Cost is computed at read time**, joining `model_pricing` every row
   every refresh (`costUSDExpr`, postgres.go) — never stored.
3. **No cache, no sharing.** N users × dashboard variants (user /
   admin / manager pages) × 30s polling (the refresh timers in
   `dashboard.html` / `admin.html` / `user-dashboard.html`) = dozens of
   identical heavy aggregates per cycle, all serialized on pg-1.
4. **Replicas idle.** All reads hit the CNPG primary.

## Phase 1 — response cache on the dashboard API (highest impact, lowest risk)

**Change:** in-memory TTL + single-flight cache wrapping the 6
`/api/v1/dashboard/*` handlers. Key = request path + query params +
resolved auth scope; TTL **45–60s — deliberately above the 30s poll**.

- **TTL must exceed the poll interval.** A TTL *below* the 30s poll
  (an earlier draft said 20s — wrong) means every client's next request
  lands after its key expired: the cache misses nearly all steady-state
  traffic, and single-flight can't rescue it because it only collapses
  *simultaneous* misses while drifted 30s polls don't overlap. With TTL
  above the poll, every client's poll finds a fresh entry and each key
  recomputes once per TTL. Alternative if 60s staleness ever feels too
  coarse: TTL 20s + serve-stale-while-revalidate (background refresh;
  never block a poll on the heavy query). Ship the simple TTL first;
  SWR is a drop-in later because the middleware boundary is the same.
- Where: `internal/handler` — middleware added *outside* the existing
  auth wrappers, registered only on the 6 `/api/v1/dashboard/*` routes.
  Auth still runs per request (a cache hit must never skip authz).
- Key: method + path + query params + the **resolved scope string** —
  i.e. what `ApplyScope` returns *before* the handler runs, which
  already absorbs impersonation and multi-login `selfScope` expansion.
  The scope marker is exactly what makes per-user responses cacheable:
  a self-scoped response is byte-identical across that user's own tabs,
  so **every** dashboard response is cache-eligible. (An earlier draft
  excluded per-user scopes — that guts the phase, since `ApplyScope`
  scopes every non-admin to self, making per-user responses the bulk
  of the motivating load.)
- What's cached: `200` responses only — marshalled JSON bytes +
  content-type. Errors and 3xx/4xx are never cached.
- Deps: `golang.org/x/sync/singleflight` (BSD-3) +
  `hashicorp/golang-lru/v2` (MPL-2.0) — no hand-rolled flight group or
  eviction list. Fixed-size LRU ≤512 entries (results are small JSON
  blobs; a few MB worst case against the 128Mi limit). Bounded buffers,
  no unbounded maps. LRU capacity must comfortably exceed the live
  working set (users × variants × filter combos) so hot keys don't
  evict each other.
- Staleness ≤ TTL (≤60s): new events appear on any open dashboard
  within one to two poll cycles — invisible to users already polling
  every 30s.
- Expected effect (estimate): dashboard DB load becomes ~flat in
  viewer count — one aggregate per filter-combo per TTL regardless of
  whether 5 or 500 tabs are open.
- **Verify:** unit tests (hit / expiry / key isolation between filter
  combos **and between scopes** — user A's self-scoped entry must never
  serve user B); `oc adm top pods aigateway-pg-1` before/after during a
  busy hour — expect the 758m-class spikes to flatten substantially.
- **Rollback:** one config knob (cache enabled flag) → redeploy.

## Phase 2 — read-replica DSN

**Change:** optional `READ_DATABASE_URL`; when set, **dashboard/report**
read functions use a second pool; unset = today's behavior (zero-config
compat). Point it at the CNPG read service (`aigateway-pg-r`), which
routes to pg-2/pg-3. Writes (`InsertEvent`) never leave the primary.

- **Money decisions stay on the primary.** A lagging replica serving an
  enforcement read means spend-lag → users under- or over-enforced for
  the lag window, and mid-failover a replica can serve a pre-promotion
  snapshot to the gate that decides 429s. The read switch is therefore
  by allowlist, not by `Get*` naming: replica pool only for the 6
  dashboard endpoints + report reads. Pinned to primary:
  `GetMonthlyUsage` (the gateway's synchronous entitlement path),
  `GetQuotaView` and every quota/approval read an admin UI acts on,
  `Recent` (freshness matters more than the negligible cost there).
  The existing 15s quota decision cache already keeps primary load flat,
  so offloading these buys nothing and risks the wrong 429.
- **Verify:** dashboard query load appears on pg-2/pg-3 in `oc adm top`;
  entitlement-path queries demonstrably still hit pg-1 (statement counts
  or `pg_stat_activity` sampling); failover behavior documented (CNPG
  promotes a replica; the `-r` service follows).
- **Rollback:** unset the env var.

## Phase 3 — write-time cost + hourly rollups (the real 100+ fix)

Makes dashboard cost **flat in total event volume**.

1. **`cost_usd` column on `usage_events`**, computed at insert from the
   cached `internal/pricing` table. `usage_events` has **two** insert
   sites — `InsertEvent` (gateway events) and `RecordQuotaDenial`
   (quota.go, 429 refusals: zero tokens, zero cost, `provider='gateway'`,
   `source='metering-quota'`) — and the latter bypasses `InsertEvent`, so
   the rollup hook must ride **both** transactions. One-time batched
   backfill of existing rows using the existing `costUSDExpr` — so new
   rows and historical rows agree by construction.
   - **Fallback-rate trap:** `costUSDExpr` bills unpriced models at
     hardcoded fallback rates, so "computed at insert agrees by
     construction" is only true if the write-time path reproduces the
     fallback branch, the price-visibility moment (a model gains a
     `model_pricing` row mid-stream), and the SQL expression's numeric
     rounding. Required test: rows written before and after a price
     insert, costed both raw-computed and column-read — same cents.
2. **`usage_hourly`** maintained in the same transaction as each insert
   (both insert sites upsert it): PK `(hour, username, group_name,
   model, provider)`; sums: requests,
   prompt/completion/total/cached/cache-creation tokens, `cost_usd`.
   Upsert via `ON CONFLICT DO UPDATE` — cheap insurance for
   concurrent/multiple writers even though today dogfood has a single
   writer (the shadow runs against its own DB). Denials count in
   `requests` with zero cost, identifiable via `source='metering-quota'`
   / `provider='gateway'`.
   - **Parity definition (explicit):** rollups count all rows including
     denials; the parity gate diffs requests **including** the 429 rows.
     Whether dashboards display requests with or without refusals is a
     display filter, not a rollup question — the rollup keeps both so
     the diff can't silently go red on the requests dimension.
3. **Switch reads to `usage_hourly`**: overview / groups / users /
   models / timeline (sum of pre-aggregated rows; timeline's
   `date_trunc` becomes trivial) **plus the quota engine** —
   `computeUsageStats`/`GetMonthlyUsage` currently run `costUSDExpr`
   over a full calendar month on the entitlement hot path; with
   `cost_usd` on the row that SUM collapses to an indexed range scan,
   and once `usage_hourly` exists the quota SUM reads rollups (fresh by
   construction — the upsert rides the insert transaction). Quota reads
   stay on the primary (Phase 2 rule). `recent` and drill-downs stay on
   raw — raw remains the source of truth.
4. **Pricing-change semantics decision (needs sign-off, and it now
   covers quotas):** with write-time cost, editing `model_pricing` no
   longer rewrites history on the dashboard (today it does) — and for
   enforcement that's arguably *preferable*: a later price correction can
   never retroactively push someone over budget. If we ever want
   repriced history anyway: a `rebuild-rollups` admin action recomputes
   `cost_usd` + `usage_hourly` from raw using current prices. Raw + new
   prices can always regenerate everything.
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

Current ≈ 500 events/hr at 30 users ≈ 4.4M events/yr (estimate; the
rollup conclusion comfortably survives the honest number).
`usage_hourly` at 100 users ≈ users×models×24 rows/day ≈ low tens of
thousands of rows/day — the dashboard sums thousands of rows instead of
millions. That's the whole trick.

## Sequencing

Phase 1 (≈ half day) → Phase 2 (≈ 2–3h + deploy) → Phase 3 (≈ 1–2 days
incl. backfill + parity). Each independently deployable and revertible;
1+2 alone should make 100 users feel acceptable, 3 makes it feel free.

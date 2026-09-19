# PriceTag on OpenShift — Fresh-Environment Deployment Guide

> **PriceTag** (formerly "the dogfood" environment) is a self-service AI-gateway stack:
> an API-key-authenticated LLM gateway (praxis) in front of Anthropic/OpenAI plus optional
> self-hosted models, with MaaS key management and a usage-billing dashboard.
>
> This guide recreates the full stack in a **single namespace on a vanilla OpenShift 4.x
> cluster** — no OpenShift AI, no Istio, no Kuadrant, no MaaS operator required.
>
> Every manifest, env var, and RBAC rule here was extracted from the live production
> environment (`ai-gateway-dogfood` namespace on IBM Cloud OpenShift, verified 2026-09-04).
> Where something is environment-specific (hostnames, IPs, groups), it is parameterized.
>
> This document is written to be executable by a human **or** another AI model: every step
> has exact commands and complete YAML. Do not improvise values — they're marked below.

---

## 1. What you're deploying

```
                        ┌────────────────────────── Routes (edge TLS) ──────────────────────────┐
 Claude Code ──────────►│ ai-gateway-unified-<ns>.<appsDomain>   (single URL, /model switching)  │
 Codex / SDKs ─────────►│ ai-gateway-anthropic-… / ai-gateway-openai-… / ai-gateway-benchmark-…  │
 Browser  ─────────────►│ dashboard-… (PriceTag)                 status-… (static status page)   │
                        └───────┬────────────────────────────────────────────┬───────────────────┘
                                ▼                                            ▼
                        ┌───────────────┐    validate key     ┌──────────────┐
                        │    praxis     │────────────────────►│   maas-api   │◄── MaaS CRDs
                        │  (Rust gateway)│   x-tenant-* ids   │  (Go, keys)  │    (subscriptions,
                        └───────┬───────┘◄────────────────────┴──────┬───────┘     model refs,
                                │  report usage (fail-open)          │             auth policies)
                                ▼                                    ▼
                        ┌───────────────────┐              ┌──────────────┐         ┌────────────┐
                        │ metering-service  │◄────────────►│  PostgreSQL  │◄────────┤ dashboard  │
                        │  (PriceTag, Go)   │  events+DB   │  (16-alpine) │  reads  │  login:    │
                        └───────────────────┘              └──────────────┘         │ key = login│
                                │                                                   └────────────┘
                                ├──► api.anthropic.com:443   (TLS SNI, pooled)
                                ├──► api.openai.com:443      (TLS SNI, pooled)
                                ├──► vLLM / self-hosted backends (optional)
                                └──► llm-katan:8000          (optional benchmark echo backend)
```

### Components

| Component | What it is | Source repo | Language |
|---|---|---|---|
| **praxis** | The gateway. Envoy-style listeners + filter chains: API-key auth, model allow/deny, routing, credential injection, metering, TLS pooling | [praxis-ai](https://github.com/yossiovadia/ai) | Rust |
| **maas-api** | MaaS key server — creates/validates `sk-…` API keys, backed by Postgres + MaaS CRDs | [models-as-a-service](https://github.com/opendatahub-io/models-as-a-service) (`maas-api/`) | Go |
| **metering-service** | **PriceTag** — token-usage billing + admin dashboard. Login = paste your MaaS API key | [ai-gateway-metering-service](https://github.com/noyitz/ai-gateway-metering-service) | Go |
| **postgresql** | Shared DB for maas-api (keys) and metering-service (usage events) | `postgres:16-alpine` | — |
| **llm-katan** | Optional: synthetic benchmark backend (echo LLM, openai+anthropic dialects) | llm-katan | Python |
| **status-page** | Optional: static status/info page | `nginxinc/nginx-unprivileged:alpine` | — |
| **qwen-flash-proxy** | Optional: nginx TLS-terminating hop to an external vLLM route | `nginx:1-alpine` | — |

### Request flow (the unified route, main user entrypoint)

1. Client POSTs Anthropic `/v1/messages` with `x-api-key: sk-…` to the `unified` route.
2. praxis `api_key_auth` filter validates the key against `maas-api` (`POST /internal/v1/api-keys/validate`, 300 s cache) → gets `username` + `groups` → sets `x-tenant-*` identity headers (`identity_header_guard` strips client-supplied ones first).
3. `model_catalog` answers `GET /v1/models` from static config (so `/model` pickers list Claude + self-hosted).
4. `model_access` enforces per-group allow/deny lists (groups come from the key's `X-MaaS-Group` at creation).
5. `model_to_header` promotes the body's `"model"` field to `X-Model`; `router` branches: self-hosted model names → vLLM clusters, everything else → Anthropic.
6. `external_metering` records the request + streamed response usage to metering-service (`fail_open: true` — metering never blocks traffic).
7. `credential_injection` swaps in the real provider key (client credential stripped); `load_balancer` sends it upstream with correct `Host`/SNI.

---

## 2. Prerequisites

| Requirement | Notes |
|---|---|
| OpenShift 4.12+ | Any provider (IBM Cloud, AWS, bare metal, ROSA). No operators beyond stock cluster. |
| `oc` CLI, cluster-admin | `oc login https://<api>:6443 -u <admin>` |
| Storage class with RWO support | For the Postgres PVC (live env: `ibmc-vpc-block-10iops-tier`). Any RWO class works. |
| Egress to `api.anthropic.com:443` / `api.openai.com:443` | praxis dials these directly by DNS name. Verify: `oc run curl --image=curlimages/curl --rm -it -- curl -sI https://api.anthropic.com` in the target namespace. |
| Provider API keys | A real Anthropic key and (optionally) OpenAI key. These go in one Secret; praxis injects them upstream. |
| Image build capability | In-cluster `ImageStream` builds (Docker strategy) are used. Or pre-build images elsewhere with Docker/Podman and change the Deployment image refs. |
| Source repos checked out | `ai-gateway-metering-service` (main), `models-as-a-service` (any recent main — only `maas-api/` subtree needed), `praxis-ai`. llm-katan optional. |
| *(optional)* Self-hosted model backends | Any OpenAI- or Anthropic-dialect vLLM endpoint. If none, drop the `vllm`/`qwen-flash` bits from praxis config and model catalog. |

**Not needed** (common false alarm): OpenShift AI / RHOAI, the MaaS controller/operator,
Istio/Service Mesh, Kuadrant, Gateway API CRDs, Red Hat OpenShift Serverless. The old
sandbox659 runbook (`docs/dogfood-runbook.md`) uses all of those — it is the **legacy
architecture**. This guide replaces it entirely; the only operator-free pieces that survive
are the 3 MaaS CRDs, applied manually in step 4.

---

## 3. Per-environment parameters

Set these once; every command below references them. Values marked **⟨pick⟩** have no
default — choose per environment.

```bash
NS=ai-gateway-dogfood                  # namespace ⟨pick⟩ — keep it short, it embeds in route hosts
APPS_DOMAIN=$(oc get dns cluster -o jsonpath='{.spec.baseDomain}')   # e.g. apps.ocp.example.com
ADMIN_USERS="alice@redhat.com,bob@redhat.com"                        # PriceTag dashboard admins ⟨pick⟩
STORAGE_CLASS=$(oc get sc -o jsonpath='{.items[0].metadata.name}')   # any RWO-capable class
ANTHROPIC_KEY=⟨real Anthropic API key⟩
OPENAI_KEY=⟨real OpenAI API key, or empty⟩

# Generated — never reuse a value from another environment or from git:
PG_PASSWORD=$(openssl rand -hex 16)
SESSION_SECRET=$(openssl rand -hex 32)     # PriceTag session-cookie signing key
```

What the dashboard does with `ADMIN_USERS`: those logins get the admin view (all users,
key management, impersonation). Everyone else sees only their own usage.

---

## 4. Install

Run steps in order. Each is idempotent (`apply`) unless noted.

### 4.1 Namespace + image streams

```bash
oc create namespace "$NS" --dry-run=client -o yaml | oc apply -f -
oc project "$NS"
```

### 4.2 Secrets

```bash
# Provider credentials (consumed by praxis, injected upstream)
oc create secret generic provider-credentials -n "$NS" \
  --from-literal=ANTHROPIC_API_KEY="$ANTHROPIC_KEY" \
  --from-literal=OPENAI_API_KEY="$OPENAI_KEY" \
  --dry-run=client -o yaml | oc apply -f -

# PostgreSQL admin credentials — the DB holds BOTH maas keys and metering events.
# Keys maas-db-config/metering need: POSTGRES_USER/POSTGRES_DB/POSTGRES_PASSWORD
# plus two per-app DSNs.
oc create secret generic postgresql-credentials -n "$NS" \
  --from-literal=POSTGRES_USER=postgres \
  --from-literal=POSTGRES_DB=postgres \
  --from-literal=POSTGRES_PASSWORD="$PG_PASSWORD" \
  --from-literal=MAAS_DB_URL="postgresql://postgres:${PG_PASSWORD}@postgresql:5432/postgres?sslmode=disable" \
  --from-literal=METERING_DB_URL="postgresql://postgres:${PG_PASSWORD}@postgresql:5432/postgres?sslmode=disable" \
  --dry-run=client -o yaml | oc apply -f -

# maas-api reads its DSN from THIS secret via the K8s API (not env vars) —
# exact name and key are hardcoded in its config loader:
oc create secret generic maas-db-config -n "$NS" \
  --from-literal=DB_CONNECTION_URL="postgresql://postgres:${PG_PASSWORD}@postgresql:5432/postgres?sslmode=disable" \
  --dry-run=client -o yaml | oc apply -f -
```

> Create two databases (e.g. `maas` and `metering`) if you want app-level separation —
> the live environment shares one. Update both DSNs accordingly.

### 4.3 PostgreSQL

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgresql
spec:
  serviceName: postgresql
  replicas: 1
  selector:
    matchLabels: { app: postgresql }
  template:
    metadata:
      labels: { app: postgresql }
    spec:
      containers:
      - name: postgresql
        image: postgres:16-alpine
        ports: [ { containerPort: 5432 } ]
        envFrom: [ { secretRef: { name: postgresql-credentials } } ]
        env:
        - name: PGDATA
          value: /var/lib/postgresql/data/pgdata
        volumeMounts:
        - { name: data, mountPath: /var/lib/postgresql/data }
        resources:
          requests: { cpu: 100m, memory: 256Mi }
          limits:   { memory: 1Gi }
  volumeClaimTemplates:
  - metadata: { name: data }
    spec:
      accessModes: [ ReadWriteOnce ]
      storageClassName: ibmc-vpc-block-10iops-tier   # ← $STORAGE_CLASS
      resources: { requests: { storage: 10Gi } }
---
apiVersion: v1
kind: Service
metadata:
  name: postgresql
spec:
  ports: [ { port: 5432 } ]
  selector: { app: postgresql }
```

`oc apply -f - <<EOF … EOF` the above (with your storage class substituted), then
`oc rollout status sts/postgresql -n "$NS"`.

### 4.4 MaaS CRDs (3 — installed manually, no operator)

```bash
git clone https://github.com/opendatahub-io/models-as-a-service /tmp/maas-src
oc apply -f /tmp/maas-src/deployment/base/maas-controller/crd/bases/maas.opendatahub.io_maasauthpolicies.yaml
oc apply -f /tmp/maas-src/deployment/base/maas-controller/crd/bases/maas.opendatahub.io_maasmodelrefs.yaml
oc apply -f /tmp/maas-src/deployment/base/maas-controller/crd/bases/maas.opendatahub.io_maassubscriptions.yaml
```

Only these three CRDs are needed. (`externalmodels.maas.opendatahub.io` is **not**
installed in the live env — `MaaSModelRef` records reference model names that maas-api
never resolves, so its absence is fine.)

### 4.5 maas-api (RBAC + SA + build + deploy)

maas-api reads the `maas-db-config` secret, namespaces, and the MaaS CRs through the
K8s API, so it needs a dedicated ServiceAccount with a **ClusterRole** (namespaces are
cluster-scoped reads):

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: { name: maas-api, namespace: ai-gateway-dogfood }   # ← $NS
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: maas-api }
rules:
- { apiGroups: [""], resources: [secrets], resourceNames: [maas-db-config], verbs: [get] }   # in practice: scope per-namespace via binding below
- { apiGroups: [""], resources: [namespaces], verbs: [get, list, watch, create] }
- { apiGroups: [""], resources: [serviceaccounts], verbs: [get, list, watch, create, delete] }
- { apiGroups: [""], resources: [serviceaccounts/token], verbs: [create] }
- { apiGroups: [authentication.k8s.io], resources: [tokenreviews], verbs: [create] }
- { apiGroups: [authorization.k8s.io], resources: [subjectaccessreviews], verbs: [create] }
- { apiGroups: [maas.opendatahub.io], resources: [maasauthpolicies, maasmodelrefs, maassubscriptions], verbs: [get, list, watch] }
- { apiGroups: [gateway.networking.k8s.io], resources: [httproutes], verbs: [get, list, watch] }   # harmless if Gateway API absent; drop otherwise
- { apiGroups: [""], resources: [pods, services, endpoints], verbs: [get, list, watch] }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: maas-api }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: maas-api }
subjects: [ { kind: ServiceAccount, name: maas-api, namespace: ai-gateway-dogfood } ]  # ← $NS
```

> The live env scopes `secrets` by resourceName only at cluster level. If your security
> posture forbids cluster-wide secret get even by name, note this as a known deviation —
> maas-api's config loader requires this exact read.

Build and deploy:

```bash
oc apply -n "$NS" -f - <<EOF
apiVersion: build.openshift.io/v1
kind: BuildConfig
metadata: { name: maas-api }
spec:
  runPolicy: Serial
  source: { type: Binary, binary: {} }
  strategy:
    dockerStrategy:
      dockerfilePath: Dockerfile
  output: { to: { kind: ImageStreamTag, name: 'maas-api:latest' } }
  sourceSecret: {}
EOF
# NOTE: build context = the models-as-a-service/maas-api directory (it is its own Go module)
oc start-build bc/maas-api -n "$NS" --from-dir=/path/to/models-as-a-service/maas-api --wait
```

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: maas-api }
spec:
  replicas: 1
  selector: { matchLabels: { app: maas-api } }
  template:
    metadata: { labels: { app: maas-api } }
    spec:
      serviceAccountName: maas-api
      containers:
      - name: maas-api
        image: image-registry.openshift-image-registry.svc:5000/ai-gateway-dogfood/maas-api:latest   # ← $NS
        ports:
        - { containerPort: 8080, name: http }
        - { containerPort: 9090, name: metrics }
        env:
        - name: NAMESPACE
          valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
        - { name: SECURE,                      value: "false" }   # ⚠ see Security notes
        - name: MAAS_SUBSCRIPTION_NAMESPACE
          valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
        - { name: METRICS_PORT,                value: "9090" }
        - { name: DEBUG_MODE,                  value: "true" }    # ⚠ see Security notes
        - { name: API_KEY_MAX_EXPIRATION_DAYS, value: "365" }
        readinessProbe: { httpGet: { path: /health, port: 8080 } }
        livenessProbe:  { httpGet: { path: /health, port: 8080 } }
---
apiVersion: v1
kind: Service
metadata: { name: maas-api }
spec:
  ports:
  - { name: http,    port: 8080 }
  - { name: metrics, port: 9090 }
  selector: { app: maas-api }
```

`SECURE=false` + `DEBUG_MODE=true` is what lets key creation accept trusted
`X-MaaS-Username`/`X-MaaS-Group` headers (see 4.9). This is the dogfood trade-off —
documented under Security.

### 4.6 MaaS model refs + subscription (CRs)

These grant key-holders access. Model names here are what maas-api meters/permits;
routing itself is praxis's job.

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata: { name: claude-sonnet, namespace: ai-gateway-dogfood }
spec:
  endpointOverride: https://api.anthropic.com
  modelRef: { kind: ExternalModel, name: claude-sonnet-4 }
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata: { name: gpt-4o, namespace: ai-gateway-dogfood }
spec:
  endpointOverride: https://api.openai.com
  modelRef: { kind: ExternalModel, name: gpt-4o }
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata: { name: dogfood-team, namespace: ai-gateway-dogfood }
spec:
  priority: 10
  owner:
    users: [admin@your-org.com]                  # ⟨pick⟩ at least yourself
    groups:                                       # ⟨pick⟩ IdM/LDAP groups, or invent names and
      - dogfood-testing                           #   pass them explicitly at key creation
  modelRefs:
  - name: claude-sonnet
    namespace: ai-gateway-dogfood
    tokenRateLimits: [ { limit: 1000000000, window: 24h } ]
  - name: gpt-4o
    namespace: ai-gateway-dogfood
    tokenRateLimits: [ { limit: 1000000000, window: 24h } ]
```

> ⚠ In the **current** wiring, praxis does not re-check subscriptions per-request —
> auth is key-validity + `model_access` group rules in praxis config. The CRs are the
> source of truth for key creation limits/tenant checks in maas-api and for the
> dashboard's model list. Keep both sides consistent when adding models.

### 4.7 praxis (gateway) — config, build, deploy, routes

**Config** — the full live `praxis.yaml` is long; this is the complete template with
all four chains. Replace `⟨…⟩` items. The `vllm` cluster endpoint `10.240.0.10:8000` in
the live env is a node-internal address — substitute your own self-hosted backend or
delete the `vllm`/`qwen-flash` routes+clusters and their catalog entries.

```yaml
apiVersion: v1
kind: ConfigMap
metadata: { name: praxis-config }
data:
  praxis.yaml: |
    admin:
      address: "0.0.0.0:9901"
    insecure_options:
      allow_public_admin: true        # ⚠ live env value — see Security notes
    listeners:
      - { name: anthropic, address: "0.0.0.0:8080", filter_chains: [anthropic] }
      - { name: openai,    address: "0.0.0.0:8081", filter_chains: [openai] }
      - { name: benchmark, address: "0.0.0.0:8082", filter_chains: [benchmark] }
      - { name: unified,   address: "0.0.0.0:8084", filter_chains: [unified] }
    filter_chains:
      # ---- anthropic-only chain ----
      - name: anthropic
        filters:
          - { filter: api_key_auth, validate_url: "http://maas-api:8080/internal/v1/api-keys/validate", token_header: "x-api-key", cache_ttl_seconds: 300, timeout_seconds: 5 }
          - filter: model_access
            mode: denylist
            models: ["claude-fable-*"]
            overrides:
              - { groups: ["⟨trusted-group⟩"], mode: allowlist, models: ["*"] }
            group_metadata_key: "x-tenant-group"
            max_body_bytes: 33554432
          - { filter: identity_header_guard, prefix: "x-tenant-" }
          - { filter: router, routes: [ { path_prefix: "/", cluster: anthropic } ] }
          - { filter: external_metering, metering_url: "http://metering-service:8080", timeout_seconds: 5, feature_key: "inference-tokens", source: "praxis-ai", fail_open: true, identity_header_prefix: "x-tenant-", default_model: "unknown" }
          - { filter: token_count, provider: anthropic }
          - { filter: token_usage_headers }
          - filter: credential_injection
            clusters:
              - { name: anthropic, header: x-api-key, env_var: ANTHROPIC_API_KEY, strip_client_credential: true }
          - filter: headers
            request_set:
              - { name: "Host", value: "api.anthropic.com" }
              - { name: "anthropic-version", value: "2023-06-01" }
          - filter: load_balancer
            clusters:
              - name: anthropic
                tls: { sni: "api.anthropic.com" }
                idle_timeout_ms: 45000     # MUST stay below provider keepalive kill (~60s) — see gotchas
                endpoints: ["api.anthropic.com:443"]
      # ---- openai-only chain: identical, with token_header "authorization",
      #      credential env OPENAI_API_KEY, header_prefix "Bearer ", Host api.openai.com,
      #      provider openai, cluster openai endpoints ["api.openai.com:443"] ----
      - name: openai
        filters:
          - { filter: api_key_auth, validate_url: "http://maas-api:8080/internal/v1/api-keys/validate", token_header: "authorization", cache_ttl_seconds: 300, timeout_seconds: 5 }
          - { filter: model_access, mode: denylist, models: ["claude-fable-*"], group_metadata_key: "x-tenant-group", max_body_bytes: 33554432 }
          - { filter: identity_header_guard, prefix: "x-tenant-" }
          - { filter: router, routes: [ { path_prefix: "/", cluster: openai } ] }
          - { filter: external_metering, metering_url: "http://metering-service:8080", timeout_seconds: 5, feature_key: "inference-tokens", source: "praxis-ai", fail_open: true, identity_header_prefix: "x-tenant-", default_model: "unknown" }
          - { filter: token_count, provider: openai }
          - { filter: token_usage_headers }
          - filter: credential_injection
            clusters:
              - { name: openai, header: Authorization, env_var: OPENAI_API_KEY, header_prefix: "Bearer ", strip_client_credential: true }
          - { filter: headers, request_set: [ { name: "Host", value: "api.openai.com" } ] }
          - filter: load_balancer
            clusters:
              - name: openai
                tls: { sni: "api.openai.com" }
                idle_timeout_ms: 45000
                endpoints: ["api.openai.com:443"]
      # ---- benchmark chain: same auth, routes to llm-katan, injects static key ----
      - name: benchmark
        filters:
          - { filter: api_key_auth, validate_url: "http://maas-api:8080/internal/v1/api-keys/validate", token_header: "x-api-key", cache_ttl_seconds: 300, timeout_seconds: 5 }
          - { filter: identity_header_guard, prefix: "x-tenant-" }
          - { filter: router, routes: [ { path_prefix: "/", cluster: llm-katan } ] }
          - { filter: external_metering, metering_url: "http://metering-service:8080", timeout_seconds: 5, feature_key: "inference-tokens", source: "praxis-ai-benchmark", fail_open: true, identity_header_prefix: "x-tenant-", default_model: "unknown" }
          - { filter: token_count, provider: anthropic }
          - { filter: token_usage_headers }
          - { filter: headers, request_set: [ { name: "x-api-key", value: "llm-katan-anthropic-key" }, { name: "anthropic-version", value: "2023-06-01" } ] }
          - { filter: load_balancer, clusters: [ { name: llm-katan, endpoints: ["llm-katan:8000"] } ] }
      # ---- unified: Anthropic-dialect, one URL, model-name routing ----
      - name: unified
        filters:
          - { filter: api_key_auth, validate_url: "http://maas-api:8080/internal/v1/api-keys/validate", token_header: "x-api-key", cache_ttl_seconds: 300, timeout_seconds: 5 }
          - filter: model_catalog               # answers GET /v1/models from config
            format: anthropic
            path: /v1/models
            models:                             # ⟨edit⟩ — what client /model pickers see
              - { id: Qwen3.8-27B-FP8,             display_name: "Qwen3.8-27B-FP8 (self-hosted, $0)", owned_by: vllm }
              - { id: claude-opus-4-8,             display_name: "Claude Opus 4.8" }
              - { id: claude-sonnet-5,             display_name: "Claude Sonnet 5" }
              - { id: claude-haiku-4-5-20251001,   display_name: "Claude Haiku 4.5" }
          - { filter: model_access, mode: denylist, models: ["claude-fable-*"], group_metadata_key: "x-tenant-group", max_body_bytes: 33554432 }
          - { filter: identity_header_guard, prefix: "x-tenant-" }
          - { filter: model_to_header, header: X-Model }      # promote body.model → header for routing
          - filter: router
            routes:                             # ⟨edit⟩ self-hosted names first, provider default last
              - { path_prefix: "/", headers: { x-model: "Qwen3.8-27B-FP8" }, cluster: vllm }
              - { path_prefix: "/", cluster: anthropic }
          - { filter: external_metering, metering_url: "http://metering-service:8080", timeout_seconds: 5, feature_key: "inference-tokens", source: "praxis-ai", fail_open: true, identity_header_prefix: "x-tenant-", default_model: "unknown" }
          - { filter: token_count, provider: anthropic }      # both backends speak Anthropic usage
          - { filter: token_usage_headers }
          - filter: credential_injection
            clusters:                           # only anthropic needs an upstream key; vllm matches nothing → no injection
              - { name: anthropic, header: x-api-key, env_var: ANTHROPIC_API_KEY, strip_client_credential: true }
          - { filter: headers, request_set: [ { name: "Host", value: "api.anthropic.com" }, { name: "anthropic-version", value: "2023-06-01" } ] }  # ⚠ see gotcha #4
          - filter: load_balancer
            clusters:
              - name: anthropic
                tls: { sni: "api.anthropic.com" }
                idle_timeout_ms: 45000
                endpoints: ["api.anthropic.com:443"]
              - { name: vllm, idle_timeout_ms: 45000, endpoints: ["⟨your-vllm-host:port⟩"] }   # or delete route+cluster
```

> Fetch the authoritative live version instead of retyping:
> `oc get cm praxis-config -n ai-gateway-dogfood -o jsonpath='{.data.praxis\.yaml}' > praxis.yaml`
> then edit environment-specific bits. The YAML above documents every filter; the live CM
> is the canonical instance.

**Build & deploy:**

```bash
# BuildConfig identical in shape to maas-api's, named praxis-ai.
# Build context = praxis-ai repo root (Rust workspace, Containerfile; ~10 min cold).
oc start-build bc/praxis-ai -n "$NS" --from-dir=/path/to/praxis-ai --wait
```

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: praxis }
spec:
  replicas: 1
  selector: { matchLabels: { app: praxis } }
  template:
    metadata: { labels: { app: praxis } }
    spec:
      containers:
      - name: praxis
        image: image-registry.openshift-image-registry.svc:5000/ai-gateway-dogfood/praxis-ai:latest   # ← $NS
        args: ["-c", "/etc/praxis/praxis.yaml"]
        ports:
        - { containerPort: 8080, name: anthropic }
        - { containerPort: 8081, name: openai }
        - { containerPort: 8082, name: benchmark }
        - { containerPort: 8084, name: unified }
        - { containerPort: 9901, name: admin }
        envFrom:
        - secretRef: { name: provider-credentials }
        volumeMounts:
        - { name: config, mountPath: /etc/praxis }
        resources:
          requests: { cpu: 100m, memory: 128Mi }
          limits:   { memory: 512Mi }
      volumes:
      - name: config
        configMap: { name: praxis-config }
---
apiVersion: v1
kind: Service
metadata: { name: praxis }
spec:
  ports:
  - { name: anthropic, port: 8080, targetPort: anthropic }
  - { name: openai,    port: 8081, targetPort: openai }
  - { name: benchmark, port: 8082, targetPort: benchmark }
  - { name: unified,   port: 8084, targetPort: unified }
  - { name: admin,     port: 9901, targetPort: admin }
  selector: { app: praxis }
```

> **Config reload**: the praxis pod reads the CM at start. After editing `praxis-config`,
> run `oc rollout restart deploy/praxis -n "$NS"`.

### 4.8 metering-service (PriceTag) + RBAC + route

The dashboard reads the `praxis-config` CM (for its routing view) through the K8s API —
hence the one-rule Role scoped to that single object:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: metering-service }
rules:
- { apiGroups: [""], resources: [configmaps], resourceNames: [praxis-config], verbs: [get] }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: metering-service }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: metering-service }
subjects: [ { kind: ServiceAccount, name: default } ]
```

```bash
# Build context = ai-gateway-metering-service repo root (Dockerfile at root).
oc start-build bc/metering-service -n "$NS" --from-dir=/path/to/ai-gateway-metering-service --wait
```

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: metering-service }
spec:
  replicas: 1
  selector: { matchLabels: { app: metering-service } }
  template:
    metadata: { labels: { app: metering-service } }
    spec:
      containers:
      - name: metering-service
        image: image-registry.openshift-image-registry.svc:5000/ai-gateway-dogfood/metering-service:latest   # ← $NS
        ports: [ { containerPort: 8080 } ]
        env:
        - name: DATABASE_URL
          valueFrom: { secretKeyRef: { name: postgresql-credentials, key: METERING_DB_URL } }
        - name: K8S_NAMESPACE
          valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
        - { name: ADMIN_USERS,                  value: "alice@redhat.com,bob@redhat.com" }  # ← $ADMIN_USERS
        - { name: SESSION_SECRET,               value: "⟨SESSION_SECRET⟩" }                 # ⚠ put in a Secret, not inline (see Security)
        - { name: ALLOW_UNAUTHENTICATED_ADMIN,  value: "false" }
        - { name: MONTHLY_TOKEN_QUOTA,          value: "10000000000" }
        - { name: PIPELINE_CONFIGMAP,           value: "praxis-config" }
        - { name: PIPELINE_CONFIGMAP_KEY,       value: "praxis.yaml" }
        # Defaults that work in-cluster as-is: MAAS_VALIDATE_URL=http://maas-api:8080/…,
        # MAAS_API_URL derived from it, PORT=8080.
        livenessProbe:  { httpGet: { path: /health, port: 8080 }, initialDelaySeconds: 10 }
        readinessProbe: { httpGet: { path: /ready,  port: 8080 } }
---
apiVersion: v1
kind: Service
metadata: { name: metering-service }
spec:
  ports: [ { port: 8080 } ]
  selector: { app: metering-service }
```

**Login model** (important for the peer): the PriceTag login page takes **the user's MaaS
API key**, POSTs it to `MAAS_VALIDATE_URL`, and on `valid:true` issues a signed session
cookie (`SESSION_SECRET`) carrying the key's username+groups. No OpenShift OAuth, no
oauth-proxy container. `ADMIN_USERS` members additionally get the admin view.

### 4.9 Routes

```yaml
# One route per listener + dashboard. host = <name>-<ns>.<APPS_DOMAIN> (default
# subdomain is fine — omit `host` entirely and let the router assign).
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: ai-gateway-unified }
spec:
  to: { kind: Service, name: praxis }
  port: { targetPort: unified }
  tls: { termination: edge }
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: ai-gateway-anthropic }
spec: { to: { kind: Service, name: praxis }, port: { targetPort: anthropic }, tls: { termination: edge } }
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: ai-gateway-openai }
spec: { to: { kind: Service, name: praxis }, port: { targetPort: openai }, tls: { termination: edge } }
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: ai-gateway-benchmark }
spec: { to: { kind: Service, name: praxis }, port: { targetPort: benchmark }, tls: { termination: edge } }
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: dashboard }
spec: { to: { kind: Service, name: metering-service }, port: { targetPort: 8080 }, tls: { termination: edge } }
```

### 4.10 Optional components

- **llm-katan** (benchmark echo backend): build from llm-katan repo root (`Containerfile`,
  Python). Deploy `llm-katan` Deployment+Service on port 8000 with args like
  `--model=benchmark-echo --backend=echo --providers=openai,anthropic --port=8000
  --ttft-ms=800 --itl-ms=15 --error-rate=0 --max-concurrent=100 --disable-dashboard`.
- **status-page**: nginx + the `status-page` ConfigMap (copy `index.html` etc. from the
  metering-service repo's `docs/`), mount at `/usr/share/nginx/html:ro`,
  `nginxinc/nginx-unprivileged:alpine`.
- **qwen-flash-proxy / vllm**: whatever backend you self-host. If it's reachable by
  hostname over the network, point the praxis `load_balancer` cluster at it directly —
  the nginx hop in the live env exists only because that vLLM is on a different cluster
  behind TLS re-termination.

### 4.11 Create API keys (per user)

With `DEBUG_MODE=true`/`SECURE=false`, maas-api trusts these headers — **cluster-internal
only**; the endpoint is not exposed by any Route:

```bash
oc port-forward svc/maas-api -n "$NS" 18080:8080 &

curl -s -X POST http://localhost:18080/v1/api-keys \
  -H "X-MaaS-Username: $(oc whoami)" \
  -H 'X-MaaS-Group: ["dogfood-testing"]' \
  -H 'Content-Type: application/json' \
  -d '{"name": "first-key", "description": "dogfood"}'
# → returns {"key": "sk-…"} — show to the user ONCE; store only the hash.
kill %1
```

The group(s) you pass become the key's `x-tenant-group` values, which drive
`model_access` overrides and the dashboard's per-user view. Admins can also create keys
from the PriceTag dashboard (it proxies to `MAAS_API_URL`).

---

## 5. Verification checklist

```bash
# 1. All pods up
oc get pods,deploy,sts,routes -n "$NS"

# 2. Key validation end-to-end (maas-api ↔ postgres ↔ CRDs)
oc exec -n "$NS" deploy/maas-api -- wget -qO- --header="Content-Type: application/json" \
  --post-data='{"key":"sk-<from-4.11>"}' http://localhost:8080/internal/v1/api-keys/validate
# expect: {"valid":true,"username":"…","groups":["dogfood-testing",…]}

# 3. Gateway auth rejects garbage key
KEY=sk-<from-4.11>
curl -sk -o /dev/null -w '%{http_code}\n' \
  https://ai-gateway-unified-${NS}.${APPS_DOMAIN}/v1/messages \
  -H "x-api-key: sk-bogus" -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
# expect: 401

# 4. Model catalog served (proves praxis config loaded)
curl -sk https://ai-gateway-unified-${NS}.${APPS_DOMAIN}/v1/models -H "x-api-key: $KEY" | head -c 300

# 5. Real completion through the gateway → Anthropic
curl -sk https://ai-gateway-unified-${NS}.${APPS_DOMAIN}/v1/messages \
  -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"say OK"}]}'

# 6. Usage landed in PriceTag DB
oc exec -n "$NS" deploy/metering-service -- wget -qO- http://localhost:8080/ready
oc exec -n "$NS" statefulset/postgresql -- psql -U postgres -c \
  'select model, count(*) from usage_events group by model;'   # table name per current schema

# 7. Dashboard login
open "https://dashboard-${NS}.${APPS_DOMAIN}/dashboard"   # paste the key from step 4.11
```

Point Claude Code at it:

```bash
export ANTHROPIC_BASE_URL="https://ai-gateway-unified-${NS}.${APPS_DOMAIN}"
export ANTHROPIC_API_KEY="sk-<key>"
claude
```

---

## 6. Day-2 operations

| Task | How |
|---|---|
| Add a model | praxis `model_catalog` + `router` (+ `load_balancer` cluster if new backend) → `oc rollout restart deploy/praxis`. Add a `MaaSModelRef` + subscription entry for key-side gating. |
| Add a user group | Key creation header `X-MaaS-Group`, plus `model_access.overrides` if it needs non-default access. |
| Edit routing/pricing live | The dashboard's routing tab reads `praxis-config` (read-only via the scoped Role). To write it, edit the CM + rollout restart. |
| Rotate provider key | Update `provider-credentials` → `oc rollout restart deploy/praxis`. |
| Rotate session keys | New `SESSION_SECRET` → restart metering-service (logs everyone out). |
| Upgrades per component | `oc start-build bc/<name> --from-dir <repo> --wait && oc rollout restart deploy/<name>` (for `:latest` tags; maas-api pins digests — update its image ref after build). |

## 7. Gotchas (all learned the hard way on the live env)

1. **`idle_timeout_ms: 45000` is load-bearing.** Provider keepalive kills pooled
   connections at ~60 s; evicting at 45 s prevents hung requests until route timeout.
   Do not raise it above the provider's keepalive window.
2. **`Host` header must match SNI for Anthropic** (`Host: api.anthropic.com` set
   explicitly). vLLM ignores Host, which is why the unified chain can set it
   unconditionally — but a future Host-validating backend on that listener breaks.
3. **praxis config is start-time only** — CM edits without rollout are silently inert.
4. **x-api-key vs Authorization**: Anthropic-dialect chains validate `x-api-key`, the
   OpenAI chain `authorization: Bearer`. Clients on the wrong header get 401 even with a
   valid key. The unified route is Anthropic-dialect (`x-api-key`).
5. **praxis strips inbound `x-tenant-*`** (`identity_header_guard`) — never rely on
   clients setting identity headers.
6. **Metering is fail-open** — dashboard gaps ≠ outage. Cross-check `external_metering`
   filter errors in praxis logs before trusting numbers.
7. **maas-api needs the `maas-db-config` secret before first start** — it fails fast at
   boot with a clear "ensure the secret exists" error if missing.
8. **praxis key-validation cache is 300 s** — a revoked key may still work up to 5 min.
9. **Docker builds**: praxis-ai is a Rust workspace — cold builds take ~10 min; give the
   builder pod room (default 1 CPU ok, just slow).
10. **Don't apply the sandbox659 runbook** (`docs/dogfood-runbook.md`) — its Istio
    EnvoyFilter/Kuadrant/controller steps belong to the retired architecture.

## 8. Security notes for a production-grade deployment

The live dogfood optimizes for access, not hardening. A new environment should start
better:

- **`SESSION_SECRET` is currently a plaintext env value in the Deployment** (in the live
  env this is a leaked credential — rotate it there). Put it in a Secret.
- **`SECURE=false` / `DEBUG_MODE=true` on maas-api** means `X-MaaS-Username`/`X-MaaS-Group`
  headers are trusted for key creation — anyone who can reach the service mints keys as
  anyone. Keep it cluster-internal (no Route — true today) and flip to `SECURE=true` when
  wiring a real identity provider.
- **`allow_public_admin: true`** exposes praxis's admin endpoint (:9901) on the pod. No
  Route points at it today; don't add one, or gate it.
- **`ALLOW_UNAUTHENTICATED_ADMIN=false`** (dashboard) must stay false outside local dev.
- The `maas-api` ClusterRole grants `get` on `secrets` by resourceName at cluster scope —
  acceptable-ish for dogfood, flag it if the cluster is shared.
- Postgres is single-DB, `sslmode=disable`, cluster-internal only. Fine inside a network
  policy'd namespace; add a `NetworkPolicy` so only praxis/maas-api/metering can reach it.
- Provider keys live in one Secret consumed by praxis env — correct pattern; just make
  sure that Secret never appears in the dashboard, logs, or CMs.

## 9. Environment-specific values in the live deployment (for reference, not copy-paste)

| Item | Live value | Fresh env |
|---|---|---|
| Namespace | `ai-gateway-dogfood` | ⟨pick⟩ |
| Apps domain | `dogfood-us-south-1-bxf-4x-…us-south.containers.appdomain.cloud` | `$APPS_DOMAIN` |
| Storage class | `ibmc-vpc-block-10iops-tier` | any RWO |
| Admin users | `yovadia@redhat.com, nitzikow@redhat.com, swatt@redhat.com` | ⟨pick⟩ |
| Monthly quota | 10,000,000,000 tokens | ⟨pick⟩ |
| Trusted group (fable access override) | `octo-eng` | ⟨pick⟩ |
| Subscription groups | `ai-eng`, `eco-eng`, `xe-eng`, `fcto-eng`, `octo-eng`, `prodsec-eng`, `cos-eng`, `ops-eng`, `ospo-eng`, `core-pe-eng`, `pnd-pe-eng`, `uie-eng`, `hybrid-pe-eng`, `ansible-pe-eng`, `hcm-pe-eng`, `cp-pe-eng`, `product-all`, `it-all`, `dogfood-testing`, `benchmark` | ⟨pick⟩ — Red Hat LDAP groups; invent names elsewhere |
| vLLM backend (Qwen3.8-27B) | `10.240.0.10:8000` | ⟨yours or delete⟩ |
| qwen-flash backend | nginx proxy → `qwen38-flash-next-….apps.emerg.pcbk.p1.openshiftapps.com` | ⟨yours or delete⟩ |

## 10. Source of truth

| Thing | Where |
|---|---|
| This guide | `ai-gateway-metering-service/docs/openshift-deploy-guide.md` |
| Live praxis routing | `oc get cm praxis-config -n ai-gateway-dogfood` |
| Live dashboard state | `oc get deploy metering-service -n ai-gateway-dogfood -o yaml` |
| Legacy (retired) arch | `docs/dogfood-runbook.md`, `docs/dogfood-env.md` — history only |
| Gateway repo | `yossiovadia/ai` (praxis-ai), Containerfile at root |
| Key server repo | `opendatahub-io/models-as-a-service`, build dir `maas-api/` |
| Dashboard repo | `noyitz/ai-gateway-metering-service` — deploy from `main` |

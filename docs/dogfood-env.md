# AI Gateway Dogfood Environment

Sandbox659 cluster running the full MaaS + Inference Gateway stack with external model routing, per-user metering, and multi-provider support.

## Quick Start

### Claude Code (all Anthropic models — Opus, Sonnet, Haiku)

```bash
export ANTHROPIC_BASE_URL="https://maas.apps.ocp.d4fcj.sandbox659.opentlc.com/llm/ext-opus"
export ANTHROPIC_API_KEY="<your-sk-oai-key>"
claude --model claude-opus-4-8
# Switch models via /model — no need to change ANTHROPIC_BASE_URL
```

### Codex (all OpenAI models — GPT-5.5, GPT-4.1-mini)

```bash
export MAAS_API_KEY="<your-sk-oai-key>"
codex
```

Codex config (`~/.codex/config.toml`):
```toml
model = "gpt-5.5"
model_provider = "maas"

[model_providers.maas]
name = "MaaS Gateway"
base_url = "https://maas.apps.ocp.d4fcj.sandbox659.opentlc.com/llm/ext-gpt55/v1"
wire_api = "responses"
env_key = "MAAS_API_KEY"
```

### API Keys

Keys are per-user, tied to the `engineering` subscription:
- Created via the MaaS API (`POST /v1/api-keys`)
- Format: `sk-oai-<random>`
- Same key works for both Claude Code and Codex

### Dashboard

https://metering-dashboard-openshift-ingress.apps.ocp.d4fcj.sandbox659.opentlc.com/dashboard

## Architecture

```
Client (Claude Code / Codex)
  │
  ▼
MaaS Gateway (Envoy + Istio)
  ├─ Lua filter: copies x-api-key → Authorization: Bearer (for Anthropic SDK clients)
  ├─ Kuadrant Wasm: API key validation via MaaS API /internal/v1/api-keys/validate
  ├─ ext_proc (payload-processing):
  │   ├─ body-field-to-header: extracts model from body → X-Gateway-Model-Name header
  │   ├─ model-provider-resolver: resolves ExternalModel by model name (header/body based)
  │   ├─ api-translation: translates between formats or passthrough
  │   ├─ apikey-injection: injects provider API key (Anthropic x-api-key / OpenAI Bearer)
  │   └─ external-metering: reports usage to metering service
  │
  ▼
Provider (api.anthropic.com / api.openai.com)
```

### Key Design: Body-Based Model Resolution

The `model-provider-resolver` plugin resolves models from the `X-Gateway-Model-Name` header (set by the previous plugin from the request body). This means:

- **Single URL per provider** — all Anthropic models share `/llm/ext-opus`, all OpenAI models share `/llm/ext-gpt55`
- **Model switching via body** — change `"model": "claude-sonnet-4-6"` in the request, no URL change needed
- The URL path is only used for Envoy routing and Kuadrant auth — the IPP plugins never use it

## Cluster Details

### OpenShift Access

- **Console**: https://console-openshift-console.apps.ocp.d4fcj.sandbox659.opentlc.com
- **Gateway**: https://maas.apps.ocp.d4fcj.sandbox659.opentlc.com

### CRDs — Two API Groups

Both API groups must have matching ExternalModels. This is a transition artifact — the MaaS controller uses `maas.opendatahub.io`, the IPP plugins use `inference.opendatahub.io`.

#### `inference.opendatahub.io/v1alpha1` (new — used by IPP plugins)

**ExternalProvider** — declares a provider with endpoint and credentials:
```yaml
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalProvider
metadata:
  name: anthropic
  namespace: llm
spec:
  provider: anthropic
  endpoint: api.anthropic.com
  auth:
    type: simple
    secretRef:
      name: anthropic-api-key
```

**ExternalModel** — maps a client-facing model name to a provider + upstream model:
```yaml
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-opus
  namespace: llm
spec:
  modelName: claude-opus-4-8          # what clients send in the body
  externalProviderRefs:
  - ref:
      name: anthropic                 # references ExternalProvider
    targetModel: claude-opus-4-8      # what gets sent upstream
    apiFormat: messages               # anthropic native format
    weight: 1
```

#### `maas.opendatahub.io/v1alpha1` (legacy — used by MaaS controller)

**ExternalModel** — flatter structure, provider info inline:
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-opus
  namespace: llm
spec:
  provider: anthropic
  endpoint: api.anthropic.com
  targetModel: claude-opus-4-8
  credentialRef:
    name: anthropic-api-key
```

**MaaSModelRef** — registers a model for routing and auth:
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: ext-opus
  namespace: llm
spec:
  modelRef:
    kind: ExternalModel
    name: ext-opus
```

**MaaSSubscription** — grants users access to models with rate limits:
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: engineering
  namespace: models-as-a-service    # MUST be in this namespace
spec:
  owner:
    users: [noy, yossi, admin]
  modelRefs:
  - name: ext-opus
    namespace: llm
    tokenRateLimits:
    - limit: 1000000000
      window: "24h"
```

**MaaSAuthPolicy** — creates Kuadrant AuthPolicies for model routes:
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: test-auth
  namespace: models-as-a-service    # MUST be in this namespace
spec:
  subjects:
    users: [noy, yossi, admin]
  modelRefs:
  - name: ext-opus
    namespace: llm
```

### Available Models

| ExternalModel | modelName | Provider | API Format |
|---|---|---|---|
| ext-opus | claude-opus-4-8 | anthropic | messages |
| ext-sonnet | claude-sonnet-4-6 | anthropic | messages |
| ext-claude-sonnet | claude-haiku-4-5-20251001 | anthropic | messages |
| ext-gpt55 | gpt-5.5 | openai | openai-responses |
| ext-openai | gpt-4.1-mini | openai | openai-chat |

### IPP Plugin Chain (ConfigMap `ipp-config`)

Request: `model-extractor` → `model-provider-resolver` → `api-translation` → `apikey-injection` → `external-metering`

Response: `api-translation` → `external-metering`

### Networking

Each ExternalModel needs:
1. **Headless Service + Endpoints** in `llm` namespace (with provider IP)
2. **DestinationRule** in `openshift-ingress` (TLS with SNI)
3. **ServiceEntry** in `openshift-ingress` (mesh external)
4. **HTTPRoute** in `llm` (with URLRewrite filter)

The MaaS controller creates Services (as ExternalName type) and HTTPRoutes automatically, but:
- ExternalName services don't work with Istio gateway — must be replaced with headless + Endpoints
- HTTPRoutes lack URLRewrite filter — must be patched after controller reconciliation

### EnvoyFilters

- **`payload-processing`** — ext_proc filter, `INSERT_BEFORE router`, uses `workloadSelector`
- **`xapikey-to-bearer`** — Lua filter at position 1, copies `x-api-key` header to `Authorization: Bearer` for Anthropic SDK clients

### Secrets

Provider API key secrets in `llm` namespace need label:
```
inference.networking.k8s.io/bbr-managed: "true"
```

## Repos

| Repo | Purpose |
|---|---|
| [ai-gateway-payload-processing](https://github.com/opendatahub-io/ai-gateway-payload-processing) | IPP plugins (model-provider-resolver, api-translation, apikey-injection, external-metering) |
| [llm-d-inference-payload-processor](https://github.com/llm-d/llm-d-inference-payload-processor) | IPP framework (ext_proc server, plugin interface, body mode, chunk processor) |
| [models-as-a-service](https://github.com/opendatahub-io/models-as-a-service) | MaaS controller + API (subscriptions, API keys, auth policies) |
| [ai-gateway-metering-service](https://github.com/noyitz/ai-gateway-metering-service) | Metering service + dashboard (this repo) |

### Combined Build

The deployed image is built from a local `combined-build` directory that cherry-picks multiple PRs:
- PRs 169+170 (body mode framework) from llm-d
- PRs 331, 332, 333 (migration, metering chunk processor, body mode declarations)
- PR 301 (multi-provider passthrough)
- Yossi's headroom branch as base
- Body-based model resolution (header/body lookup instead of URL path)

Image: `image-registry.openshift-image-registry.svc:5000/openshift-ingress/payload-processing-test:v2`

## Known Issues / Workarounds

1. **MaaS controller overwrites payload-processing image** — controller scaled to 0 to prevent; deployment annotated with `maas.opendatahub.io/skip-reconcile: "true"`
2. **HTTPRoutes lose URLRewrite** — must re-apply after any controller reconciliation
3. **ExternalName services don't work** — must replace with headless Service + Endpoints
4. **Gateway OOM at 1Gi** — needs 3Gi for the number of auth configs + wasm filters
5. **MaaSAuthPolicy must be in `models-as-a-service` namespace** — controller only watches there
6. **MaaSSubscription must be in `models-as-a-service` namespace** — MaaS API default
7. **Secrets need `bbr-managed` label** — apikey-injection plugin only watches labeled secrets
8. **maas-controller ClusterRole missing `delete` for serviceentries** — prevents ownerRef on ServiceEntry

# Headroom Integration Plan for Dogfood

## For Yossi — deploy headroom compression into the sandbox659 dogfood environment

### Cluster Access

```bash
oc login --server=https://api.ocp.d4fcj.sandbox659.opentlc.com:6443
# Get token from the OCP console or ask Noy
```

### Current Environment State

- **Gateway**: `https://maas.apps.ocp.d4fcj.sandbox659.opentlc.com`
- **payload-processing image**: `image-registry.openshift-image-registry.svc:5000/openshift-ingress/payload-processing-test:v6`
- **Image registry**: `default-route-openshift-image-registry.apps.ocp.d4fcj.sandbox659.opentlc.com`
- **MaaS controller**: scaled to 0 (must stay down — it overwrites our customizations)
- **Dashboard**: https://metering-dashboard-openshift-ingress.apps.ocp.d4fcj.sandbox659.opentlc.com/dashboard

The headroom plugin is **already compiled into the v6 image** — registered in `plugins.go`, implemented at `pkg/plugins/headroom/`. You only need to deploy the sidecar and update the ConfigMap.

### Full runbook for all env patches: 
https://github.com/noyitz/ai-gateway-metering-service/blob/main/docs/dogfood-runbook.md

---

## Why Option E, not Option D

Your architecture doc evaluates 4 options. We propose a 5th that combines the best of all:

### The problem with Option D (sidecar + /v1/compress-raw)

Option D calls `/v1/compress-raw` — a custom stateless endpoint that bypasses headroom's full pipeline. Here's what we lose:

| Headroom Feature | Available in /v1/compress-raw | Available in library compress() |
|-----------------|------------------------------|--------------------------------|
| ContentRouter (smart strategy selection) | Yes | Yes |
| SmartCrusher / Kompress / CodeCompressor | Yes | Yes |
| **CacheAligner** (prefix stabilization for KV cache hits) | **No** | **Yes** |
| **Session tracking** (knows turn distance from history) | **No** | **Yes** |
| **CCR cache** (skip re-compression of seen content) | **No** | **Yes** |
| Reversibility (original caching) | No | Yes |

**CacheAligner alone is worth more than compression.** Anthropic charges $1.88/MTok for cache reads vs. $18.75/MTok for new input — a 10x difference. CacheAligner stabilizes prompt prefixes so the provider's KV cache hits. Without it, compressed text varies slightly each time → cache miss every request.

**At 1K users, Option D costs 40 CPU + 40Gi** (one 4-CPU sidecar per IPP replica × 10 replicas). Option E shares a GPU pool across all users.

### Option E: Headroom as a shared compression service

```
Client → MaaS Gateway → ext_proc (IPP) → Headroom Service → back to IPP → Provider
                              ↑                    ↑
                         IPP plugin calls    Shared service with
                         /v1/compress with    session state, GPU,
                         full message array   cache alignment
```

**Confirmed**: headroom's Python library supports this natively:
```python
from headroom import compress

result = compress(messages=[...], model="claude-opus-4-8")
result.messages           # compressed messages, same format
result.tokens_saved       # savings count
result.compression_ratio  # e.g. 0.35 = 65% saved
```

The docstring says: *"No proxy, no config needed. Just pass messages and get compressed messages back."*

| | Option A | Option D | Option E |
|-|----------|----------|----------|
| Session aware | Yes | No | Yes |
| Cache alignment | Yes | No | Yes |
| MaaS auth/metering | No | Yes | Yes |
| Single URL for users | No | Yes | Yes |
| User-side changes | URL change | None | None |
| GPU sharing | Shared | Per-pod (expensive) | Shared |
| Scale to 1K users | Good | Bad | Best |

---

## Deployment Steps (Option D for dogfood now)

For dogfood, Option D is fine — 2-3 users, no GPU, sidecar is quick to deploy. Option E is the production evolution.

### Step 1: Build headroom sidecar image

```bash
REGISTRY="default-route-openshift-image-registry.apps.ocp.d4fcj.sandbox659.opentlc.com"
echo "$(oc whoami -t)" | docker login "$REGISTRY" -u "$(oc whoami)" --password-stdin

cd /path/to/combined-build/deploy/examples/headroom
docker build --platform linux/amd64 -t ${REGISTRY}/openshift-ingress/headroom-sidecar:latest .
docker push ${REGISTRY}/openshift-ingress/headroom-sidecar:latest
```

If the Dockerfile doesn't exist, create one:
```dockerfile
FROM python:3.12-slim
RUN pip install "headroom-ai[ml]"
EXPOSE 8787
CMD ["python", "-c", "from headroom.transforms.content_router import ContentRouter; from flask import Flask, request, jsonify; app = Flask(__name__); router = ContentRouter(); \n@app.route('/v1/compress-raw', methods=['POST'])\ndef compress():\n    data = request.json\n    result = router.compress(data.get('text', ''))\n    return jsonify({'compressed': result.compressed})\napp.run(host='0.0.0.0', port=8787)"]
```

Or use the simpler approach — run the compress-raw server from `deploy/examples/headroom/compress-raw-server.py`.

### Step 2: Add sidecar to payload-processing

```bash
oc patch deploy payload-processing -n openshift-ingress --type='json' -p='[
  {"op": "add", "path": "/spec/template/spec/containers/-", "value": {
    "name": "headroom",
    "image": "image-registry.openshift-image-registry.svc:5000/openshift-ingress/headroom-sidecar:latest",
    "ports": [{"containerPort": 8787}],
    "resources": {
      "requests": {"cpu": "2", "memory": "2Gi"},
      "limits": {"cpu": "4", "memory": "4Gi"}
    }
  }}
]'
```

### Step 3: Update ConfigMap ipp-config

```bash
oc apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ipp-config
  namespace: openshift-ingress
data:
  config.yaml: |
    plugins:
      - name: model-extractor
        type: body-field-to-header
        parameters:
          fieldName: model
          headerName: X-Gateway-Model-Name
      - name: model-provider-resolver
        type: model-provider-resolver
      - name: headroom
        type: headroom
        parameters:
          headroomURL: "http://localhost:8787"
          timeoutSeconds: 10
          failOpen: true
          protectRecentTurns: 2
          minCompressChars: 500
      - name: api-translation
        type: api-translation
        parameters:
          responseBodyMode: "none"
      - name: apikey-injection
        type: apikey-injection
      - name: external-metering
        type: external-metering
        parameters:
          meteringURL: "http://metering-service.openshift-ingress.svc:8080"
          failOpen: true
          source: "maas-gateway"
    profiles:
      - name: default
        plugins:
          request:
            - pluginRef: model-extractor
            - pluginRef: model-provider-resolver
            - pluginRef: headroom
            - pluginRef: api-translation
            - pluginRef: apikey-injection
            - pluginRef: external-metering
          response:
            - pluginRef: external-metering
EOF
```

### Step 4: Restart and verify

```bash
oc rollout restart deploy/payload-processing -n openshift-ingress
sleep 20
oc logs -n openshift-ingress deploy/payload-processing | grep headroom
```

### Step 5: Test

Send a multi-turn request with tool outputs:
```bash
GATEWAY_URL="maas.apps.ocp.d4fcj.sandbox659.opentlc.com"
API_KEY="sk-oai-1QKwarMxHRwEqbbXA_eJqUnQwYav91cNwd5J2G2pLYiCD8EWqAyVvqAkC8nNq"

curl -sk -w "\nHTTP %{http_code}\n" \
  -H "x-api-key: $API_KEY" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -X POST "https://${GATEWAY_URL}/llm/ext-opus/v1/messages" \
  -d '{
    "model": "claude-opus-4-8",
    "max_tokens": 50,
    "messages": [
      {"role": "user", "content": "read this file"},
      {"role": "assistant", "content": "I will read the file."},
      {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "x", "content": "Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat. Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur. Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia deserunt mollit anim id est laborum. This is a very long tool output that should trigger compression by the headroom plugin because it exceeds 500 characters."}]},
      {"role": "assistant", "content": "I read the file. It contains lorem ipsum text."},
      {"role": "user", "content": "summarize what you found"}
    ]
  }'
```

Check logs:
```bash
oc logs -n openshift-ingress deploy/payload-processing --since=30s | grep -i headroom
```

Expected: `headroom-tokens-saved` in logs, `x-headroom-tokens-saved` in response headers.

## IMPORTANT: Things that break when you touch the env

The MaaS controller is scaled to 0. If you scale it up for ANY reason:

1. **payload-processing image gets overwritten** → re-apply:
   ```bash
   oc set image deploy/payload-processing -n openshift-ingress \
     payload-processing=image-registry.openshift-image-registry.svc:5000/openshift-ingress/payload-processing-test:v6
   ```

2. **Services get converted to ExternalName** → re-create as headless:
   ```bash
   # See dogfood-runbook.md step 11
   ```

3. **HTTPRoutes lose URLRewrite** → re-apply:
   ```bash
   # See dogfood-runbook.md step 12
   ```

## Key Files

| File | Purpose |
|------|---------|
| `combined-build/pkg/plugins/headroom/plugin.go` | IPP headroom plugin (already in v6 image) |
| `combined-build/pkg/plugins/headroom/client.go` | HTTP client for sidecar |
| `combined-build/deploy/examples/headroom/` | Sidecar Dockerfile + compress-raw server |
| `combined-build/pkg/plugins/plugins.go` | Plugin registration (headroom already registered) |
| `combined-build/pkg/plugins/common/state/state-keys.go` | CycleState keys: HeadroomTokensBefore/After/Saved |

## Future: Option E for Production

When ready for production scale (1K+ users), replace the sidecar with a shared headroom service:

```python
# headroom-service.py (~20 lines)
from headroom import compress
from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI()

class CompressRequest(BaseModel):
    messages: list
    model: str

@app.post("/v1/compress")
def compress_messages(req: CompressRequest):
    result = compress(messages=req.messages, model=req.model)
    return {
        "messages": result.messages,
        "tokens_saved": result.tokens_saved,
        "compression_ratio": result.compression_ratio
    }
```

Deploy as a shared Deployment (3-5 GPU-backed replicas), update the IPP plugin to call the service URL instead of localhost. This gives session tracking, CacheAligner, CCR cache, and efficient GPU sharing.

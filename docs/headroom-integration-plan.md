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

## Deployment Steps (Option E — shared headroom service)

### Step 1: Build the headroom compression service

Create a FastAPI wrapper around headroom's library API. This is NOT a proxy — it receives messages, compresses them, and returns compressed messages back.

```python
# headroom_service.py
from headroom import compress
from fastapi import FastAPI
from pydantic import BaseModel
import uvicorn

app = FastAPI()

class CompressRequest(BaseModel):
    messages: list
    model: str = "default"

@app.post("/v1/compress")
def compress_messages(req: CompressRequest):
    result = compress(messages=req.messages, model=req.model)
    return {
        "messages": result.messages,
        "tokens_before": result.tokens_before,
        "tokens_after": result.tokens_after,
        "tokens_saved": result.tokens_saved,
        "compression_ratio": result.compression_ratio
    }

@app.get("/health")
def health():
    return {"status": "ok"}

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8787)
```

Dockerfile:
```dockerfile
FROM python:3.12-slim
RUN pip install "headroom-ai[ml]" fastapi uvicorn
COPY headroom_service.py /app/headroom_service.py
WORKDIR /app
EXPOSE 8787
CMD ["python", "headroom_service.py"]
```

Build and push:
```bash
REGISTRY="default-route-openshift-image-registry.apps.ocp.d4fcj.sandbox659.opentlc.com"
echo "$(oc whoami -t)" | docker login "$REGISTRY" -u "$(oc whoami)" --password-stdin
docker build --platform linux/amd64 --provenance=false -t ${REGISTRY}/openshift-ingress/headroom-service:latest .
docker push ${REGISTRY}/openshift-ingress/headroom-service:latest
```

### Step 2: Deploy headroom as a standalone service

```bash
oc apply -n openshift-ingress -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: headroom-service
  labels:
    app: headroom-service
spec:
  replicas: 1
  selector:
    matchLabels:
      app: headroom-service
  template:
    metadata:
      labels:
        app: headroom-service
    spec:
      containers:
      - name: headroom
        image: image-registry.openshift-image-registry.svc:5000/openshift-ingress/headroom-service:latest
        ports:
        - containerPort: 8787
        resources:
          requests:
            cpu: "2"
            memory: 2Gi
          limits:
            cpu: "4"
            memory: 4Gi
        livenessProbe:
          httpGet:
            path: /health
            port: 8787
          initialDelaySeconds: 30
---
apiVersion: v1
kind: Service
metadata:
  name: headroom-service
spec:
  ports:
  - port: 8787
  selector:
    app: headroom-service
EOF
```

### Step 3: Update the IPP headroom plugin

The current plugin (`pkg/plugins/headroom/plugin.go`) calls `/v1/compress-raw` with a text blob. For Option E, it needs to call `/v1/compress` with the full message array and get compressed messages back.

Update `client.go` to send messages + model, receive compressed messages:
```go
// Before (Option D):
func (c *Client) CompressRaw(text string) (string, error)

// After (Option E):
func (c *Client) Compress(messages []any, model string) ([]any, *CompressResult, error)
```

Update `plugin.go` ProcessRequest to:
1. Read model from CycleState
2. Send full `request.Body["messages"]` to headroom service
3. Replace `request.Body["messages"]` with compressed result
4. Write savings to CycleState

### Step 4: Update ConfigMap ipp-config

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
          headroomURL: "http://headroom-service.openshift-ingress.svc:8787"
          timeoutSeconds: 10
          failOpen: true
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

Note: `headroomURL` points to the **service** (`headroom-service.openshift-ingress.svc:8787`), not localhost. The `protectRecentTurns` and `minCompressChars` config is no longer needed — headroom's library handles smart selection internally via ContentRouter.

### Step 5: Rebuild payload-processing image

After updating the plugin code, rebuild and push:
```bash
cd /path/to/combined-build
REGISTRY="default-route-openshift-image-registry.apps.ocp.d4fcj.sandbox659.opentlc.com"
docker build --platform linux/amd64 --no-cache --provenance=false --build-arg CGO_ENABLED=0 \
  -t ${REGISTRY}/openshift-ingress/payload-processing-test:v7 .
docker push ${REGISTRY}/openshift-ingress/payload-processing-test:v7

oc set image deploy/payload-processing -n openshift-ingress \
  payload-processing=image-registry.openshift-image-registry.svc:5000/openshift-ingress/payload-processing-test:v7
```

### Step 6: Test

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
oc logs -n openshift-ingress deploy/headroom-service --since=30s
```

Expected: headroom service logs show compression, IPP logs show token savings, metering dashboard shows reduced input tokens.

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

## Production Scale (1K+ users)

For production, add:
- GPU-backed replicas (L4/T4) — <100ms latency vs. ~3s CPU-only
- 3-5 replicas behind the headroom-service Service (shared pool)
- Redis for session state persistence across replicas (optional — in-process memory works for sticky sessions)
- Horizontal Pod Autoscaler based on request latency

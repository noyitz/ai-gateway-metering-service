# Dogfood Environment Runbook

Step-by-step to recreate the sandbox659 environment from scratch. Run after `deploy.sh` completes MaaS installation.

## Prerequisites

- MaaS deployed via `deploy.sh --operator-type rhoai`
- `oc` logged in as admin
- Docker available for image builds
- Repos cloned: `combined-build`, `ai-gateway-metering-service`, `models-as-a-service`

## 1. Scale down MaaS controller

The controller constantly overwrites our customizations. Scale it down and keep it down except when you need auth/subscription reconciliation.

```bash
oc scale deploy maas-controller -n redhat-ods-applications --replicas=0
```

## 2. Fix maas-controller ClusterRole

Add `delete` and `patch` for serviceentries (needed for ownerRef):

```bash
oc get clusterrole maas-controller-role -o json | python3 -c "
import json, sys
role = json.load(sys.stdin)
for rule in role.get('rules', []):
    if 'networking.istio.io' in rule.get('apiGroups', []) and 'serviceentries' in rule.get('resources', []):
        for verb in ['delete', 'patch']:
            if verb not in rule['verbs']:
                rule['verbs'].append(verb)
json.dump(role, sys.stdout)
" | oc apply -f -
```

## 3. Build and push custom payload-processing image

```bash
cd /path/to/combined-build
REGISTRY="default-route-openshift-image-registry.apps.ocp.<cluster>.opentlc.com"
docker build --platform linux/amd64 --no-cache --provenance=false --build-arg CGO_ENABLED=0 \
  -t ${REGISTRY}/openshift-ingress/payload-processing-test:v6 .
echo "$(oc whoami -t)" | docker login "$REGISTRY" -u "$(oc whoami)" --password-stdin
docker push ${REGISTRY}/openshift-ingress/payload-processing-test:v6
```

## 4. Build and push custom maas-controller image (X-MaaS-Username fix)

```bash
cd /path/to/models-as-a-service
docker build --platform linux/amd64 --no-cache --provenance=false --build-arg CGO_ENABLED=0 \
  -f maas-controller/Dockerfile \
  -t ${REGISTRY}/redhat-ods-applications/maas-controller:username-fix .
docker push ${REGISTRY}/redhat-ods-applications/maas-controller:username-fix
```

## 5. Deploy payload-processing with custom image and config

```bash
# ConfigMap with plugin chain
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
            - pluginRef: api-translation
            - pluginRef: apikey-injection
            - pluginRef: external-metering
          response:
            - pluginRef: external-metering
EOF

# Patch deployment
oc patch deploy payload-processing -n openshift-ingress --type='json' -p='[
  {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "image-registry.openshift-image-registry.svc:5000/openshift-ingress/payload-processing-test:v6"},
  {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--config-file", "/etc/ipp/config.yaml"]},
  {"op": "replace", "path": "/spec/template/spec/volumes", "value": [{"name": "ipp-config", "configMap": {"name": "ipp-config"}}]},
  {"op": "replace", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [{"name": "ipp-config", "mountPath": "/etc/ipp"}]}
]'
```

## 6. Fix EnvoyFilter — use workloadSelector, not targetRefs

The MaaS-created EnvoyFilter uses `targetRefs` which doesn't work on this Istio version:

```bash
oc apply -f - <<'EOF'
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: payload-processing
  namespace: openshift-ingress
spec:
  workloadSelector:
    labels:
      gateway.networking.k8s.io/gateway-name: maas-default-gateway
  configPatches:
  - applyTo: HTTP_FILTER
    match:
      context: GATEWAY
      listener:
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
            subFilter:
              name: envoy.filters.http.router
    patch:
      operation: INSERT_BEFORE
      value:
        name: envoy.filters.http.ext_proc.bbr
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
          allow_mode_override: true
          failure_mode_allow: false
          grpc_service:
            envoy_grpc:
              cluster_name: outbound|9004||payload-processing.openshift-ingress.svc.cluster.local
          processing_mode:
            request_body_mode: FULL_DUPLEX_STREAMED
            request_header_mode: SEND
            request_trailer_mode: SEND
            response_body_mode: FULL_DUPLEX_STREAMED
            response_header_mode: SEND
            response_trailer_mode: SEND
EOF
```

## 7. Add Lua filter for x-api-key → Authorization header

Claude Code sends `x-api-key` but Kuadrant auth only accepts `Authorization: Bearer`. This Lua filter copies the header before auth runs:

```bash
oc apply -f - <<'EOF'
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: xapikey-to-bearer
  namespace: openshift-ingress
spec:
  workloadSelector:
    labels:
      gateway.networking.k8s.io/gateway-name: maas-default-gateway
  configPatches:
  - applyTo: HTTP_FILTER
    match:
      context: GATEWAY
      listener:
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
            subFilter:
              name: istio.metadata_exchange
    patch:
      operation: INSERT_AFTER
      value:
        name: envoy.filters.http.lua.xapikey
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
          inline_code: |
            function envoy_on_request(request_handle)
              local xkey = request_handle:headers():get("x-api-key")
              local auth = request_handle:headers():get("authorization")
              if xkey and xkey ~= "" and (auth == nil or auth == "") then
                request_handle:headers():add("authorization", "Bearer " .. xkey)
              end
            end
EOF
```

## 8. Increase gateway memory to 3Gi

The number of auth configs + wasm filters OOMs at 1Gi:

```bash
oc patch deploy maas-default-gateway-openshift-default -n openshift-ingress --type='json' \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"3Gi"}]'
```

## 9. Create ExternalProviders and ExternalModels

Both `inference.opendatahub.io` AND `maas.opendatahub.io` API groups need matching CRDs.

```bash
# inference.opendatahub.io — used by IPP plugins
oc apply -f - <<'EOF'
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
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalProvider
metadata:
  name: openai
  namespace: llm
spec:
  provider: openai
  endpoint: api.openai.com
  auth:
    type: simple
    secretRef:
      name: openai-api-key
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-opus
  namespace: llm
spec:
  modelName: claude-opus-4-8
  externalProviderRefs:
  - ref:
      name: anthropic
    targetModel: claude-opus-4-8
    apiFormat: messages
    weight: 1
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-sonnet
  namespace: llm
spec:
  modelName: claude-sonnet-4-6
  externalProviderRefs:
  - ref:
      name: anthropic
    targetModel: claude-sonnet-4-6
    apiFormat: messages
    weight: 1
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-claude-sonnet
  namespace: llm
spec:
  modelName: claude-haiku-4-5-20251001
  externalProviderRefs:
  - ref:
      name: anthropic
    targetModel: claude-haiku-4-5-20251001
    apiFormat: messages
    weight: 1
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-gpt55
  namespace: llm
spec:
  modelName: gpt-5.5
  externalProviderRefs:
  - ref:
      name: openai
    targetModel: gpt-5.5
    apiFormat: openai-responses
    weight: 1
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-openai
  namespace: llm
spec:
  modelName: gpt-4.1-mini
  externalProviderRefs:
  - ref:
      name: openai
    targetModel: gpt-4.1-mini
    apiFormat: openai-chat
    weight: 1
EOF

# maas.opendatahub.io — used by MaaS controller
oc apply -f - <<'EOF'
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
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-sonnet
  namespace: llm
spec:
  provider: anthropic
  endpoint: api.anthropic.com
  targetModel: claude-sonnet-4-6
  credentialRef:
    name: anthropic-api-key
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-claude-sonnet
  namespace: llm
spec:
  provider: anthropic
  endpoint: api.anthropic.com
  targetModel: claude-haiku-4-5-20251001
  credentialRef:
    name: anthropic-api-key
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-gpt55
  namespace: llm
spec:
  provider: openai
  endpoint: api.openai.com
  targetModel: gpt-5.5
  credentialRef:
    name: openai-api-key
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: ext-openai
  namespace: llm
spec:
  provider: openai
  endpoint: api.openai.com
  targetModel: gpt-4.1-mini
  credentialRef:
    name: openai-api-key
EOF
```

## 10. Label secrets for apikey-injection

```bash
oc label secret anthropic-api-key openai-api-key -n llm inference.networking.k8s.io/bbr-managed=true
```

## 11. Fix services — headless with Endpoints (not ExternalName)

The controller creates ExternalName services which don't work with Istio gateway. Must replace with headless + Endpoints:

```bash
ANTHROPIC_IP=$(dig +short api.anthropic.com | head -1)
OPENAI_IP=$(dig +short api.openai.com | head -1)

for svc in ext-opus ext-sonnet ext-claude-sonnet ext-gpt55 ext-openai; do
  oc delete svc $svc -n llm --ignore-not-found
  if echo "$svc" | grep -qE "opus|sonnet|claude"; then IP=$ANTHROPIC_IP; else IP=$OPENAI_IP; fi
  oc apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $svc
  namespace: llm
spec:
  ports:
  - port: 443
  clusterIP: None
---
apiVersion: v1
kind: Endpoints
metadata:
  name: $svc
  namespace: llm
subsets:
- addresses:
  - ip: $IP
  ports:
  - port: 443
EOF
done
```

## 12. Fix HTTPRoutes — add URLRewrite

Controller creates HTTPRoutes without URLRewrite, so providers get `/llm/ext-opus/v1/messages` instead of `/v1/messages`:

```bash
for route in ext-opus ext-sonnet ext-claude-sonnet ext-gpt55 ext-openai; do
  oc get httproute $route -n llm -o json | python3 -c "
import json, sys
r = json.load(sys.stdin); r.pop('status', None); r['metadata'].pop('managedFields', None)
for rule in r['spec']['rules']:
    if not any(f['type'] == 'URLRewrite' for f in rule.get('filters', [])):
        rule.setdefault('filters', []).append({'type': 'URLRewrite', 'urlRewrite': {'path': {'type': 'ReplacePrefixMatch', 'replacePrefixMatch': '/'}}})
json.dump(r, sys.stdout)
" | oc replace -f -
done
```

## 13. Create DestinationRules for TLS

```bash
oc apply -f - <<'EOF'
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: anthropic-tls
  namespace: openshift-ingress
spec:
  host: ext-opus.llm.svc.cluster.local
  trafficPolicy:
    tls:
      mode: SIMPLE
      sni: api.anthropic.com
---
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: openai-tls
  namespace: openshift-ingress
spec:
  host: ext-gpt55.llm.svc.cluster.local
  trafficPolicy:
    tls:
      mode: SIMPLE
      sni: api.openai.com
EOF
```

Note: create additional DestinationRules for each service hostname (ext-sonnet, ext-openai, etc.) pointing to the correct SNI.

## 14. Scale up controller for auth/subscription reconciliation

Scale up temporarily, wait for reconciliation, scale back down:

```bash
# Create MaaSModelRefs
oc apply -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: ext-opus
  namespace: llm
spec:
  modelRef:
    kind: ExternalModel
    name: ext-opus
---
# Repeat for ext-sonnet, ext-claude-sonnet, ext-gpt55, ext-openai
EOF

# Create MaaSAuthPolicy (use group for easy management)
oc apply -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: test-auth
  namespace: models-as-a-service
spec:
  subjects:
    users: [admin, noy, yossi]
    groups:
    - name: system:authenticated
  modelRefs:
  - name: ext-opus
    namespace: llm
  - name: ext-sonnet
    namespace: llm
  - name: ext-claude-sonnet
    namespace: llm
  - name: ext-gpt55
    namespace: llm
  - name: ext-openai
    namespace: llm
EOF

# Create MaaSSubscription
oc apply -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: engineering
  namespace: models-as-a-service
spec:
  owner:
    users: [noy, yossi, admin]
  modelRefs:
  - name: ext-opus
    namespace: llm
    tokenRateLimits:
    - limit: 1000000000
      window: "24h"
  - name: ext-sonnet
    namespace: llm
    tokenRateLimits:
    - limit: 1000000000
      window: "24h"
  - name: ext-claude-sonnet
    namespace: llm
    tokenRateLimits:
    - limit: 1000000000
      window: "24h"
  - name: ext-gpt55
    namespace: llm
    tokenRateLimits:
    - limit: 1000000000
      window: "24h"
  - name: ext-openai
    namespace: llm
    tokenRateLimits:
    - limit: 1000000000
      window: "24h"
EOF

# Scale up controller to reconcile auth policies and TRLPs
oc scale deploy maas-controller -n redhat-ods-applications --replicas=1

# Use custom controller image for X-MaaS-Username support
oc set image deploy/maas-controller -n redhat-ods-applications \
  manager=image-registry.openshift-image-registry.svc:5000/redhat-ods-applications/maas-controller:username-fix

# Wait for Ready status
sleep 30
oc get maasmodelref -n llm
oc get authpolicy -n llm
oc get tokenratelimitpolicy -n llm
oc get maassubscription -n models-as-a-service

# Scale back down
oc scale deploy maas-controller -n redhat-ods-applications --replicas=0
```

## 15. Re-apply fixes after controller reconciliation

The controller overwrites services, HTTPRoutes, and the payload-processing image. Re-run steps 5, 11, and 12.

## 16. Deploy metering service + PostgreSQL

```bash
cd /path/to/ai-gateway-metering-service

# Build and push metering service image
DOCKER_BUILDKIT=0 docker build --platform linux/amd64 \
  -t ${REGISTRY}/openshift-ingress/metering-service:latest .
docker push ${REGISTRY}/openshift-ingress/metering-service:latest

# Deploy PostgreSQL
oc apply -n openshift-ingress -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: metering-db
stringData:
  DATABASE_URL: "postgres://metering:metering-dev@metering-postgresql:5432/metering?sslmode=disable"
  POSTGRESQL_USER: metering
  POSTGRESQL_PASSWORD: metering-dev
  POSTGRESQL_DATABASE: metering
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: metering-postgresql
spec:
  serviceName: metering-postgresql
  replicas: 1
  selector:
    matchLabels:
      app: metering-postgresql
  template:
    metadata:
      labels:
        app: metering-postgresql
    spec:
      containers:
      - name: postgresql
        image: registry.redhat.io/rhel9/postgresql-16:latest
        ports:
        - containerPort: 5432
        env:
        - name: POSTGRESQL_USER
          value: metering
        - name: POSTGRESQL_PASSWORD
          value: metering-dev
        - name: POSTGRESQL_DATABASE
          value: metering
---
apiVersion: v1
kind: Service
metadata:
  name: metering-postgresql
spec:
  ports:
  - port: 5432
  selector:
    app: metering-postgresql
  clusterIP: None
EOF

# Deploy metering service
oc apply -n openshift-ingress -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: metering-service
spec:
  replicas: 1
  selector:
    matchLabels:
      app: metering-service
  template:
    metadata:
      labels:
        app: metering-service
    spec:
      containers:
      - name: metering-service
        image: image-registry.openshift-image-registry.svc:5000/openshift-ingress/metering-service:latest
        ports:
        - containerPort: 8080
        env:
        - name: DATABASE_URL
          valueFrom:
            secretKeyRef:
              name: metering-db
              key: DATABASE_URL
        - name: DASHBOARD_PATH
          value: /dashboard
---
apiVersion: v1
kind: Service
metadata:
  name: metering-service
spec:
  ports:
  - port: 8080
  selector:
    app: metering-service
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: metering-dashboard
spec:
  host: metering-dashboard-openshift-ingress.apps.ocp.<cluster>.opentlc.com
  to:
    kind: Service
    name: metering-service
  port:
    targetPort: 8080
  tls:
    termination: edge
EOF
```

## 17. Create API keys

```bash
oc port-forward svc/maas-api -n redhat-ods-applications 28443:8443 &
sleep 2

# For each user:
curl -sk -X POST "https://localhost:28443/v1/api-keys" \
  -H 'X-MaaS-Username: noy' \
  -H 'X-MaaS-Group: ["system:authenticated"]' \
  -H "Content-Type: application/json" \
  -d '{"name": "noy-key", "description": "Engineering team"}'

curl -sk -X POST "https://localhost:28443/v1/api-keys" \
  -H 'X-MaaS-Username: yossi' \
  -H 'X-MaaS-Group: ["system:authenticated"]' \
  -H "Content-Type: application/json" \
  -d '{"name": "yossi-key", "description": "Engineering team"}'

kill %1
```

## Checklist — things the controller overwrites on every scale-up

After scaling the maas-controller up and back down, you MUST re-apply:

- [ ] Payload-processing image → step 5
- [ ] Services (ExternalName → headless) → step 11
- [ ] HTTPRoutes (add URLRewrite) → step 12
- [ ] Verify `oc get pods` — gateway not OOMing, payload-processing running correct image

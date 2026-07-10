# AI Gateway Metering Service

A development metering backend for the [AI Inference Gateway](https://github.com/opendatahub-io/ai-gateway-payload-processing) — provides CloudEvents-compatible token usage ingestion, per-user balance checks, and usage aggregation for testing the `external-metering` IPP plugin.

**Use this to test metering without deploying OpenMeter or a production billing system.**

## What it does

| Endpoint | Purpose |
|----------|---------|
| `POST /api/v1/events` | Ingest CloudEvents v1.0 token usage events |
| `GET /api/v1/customers/{id}/entitlements/{key}/value` | Check user balance (quota - usage) |
| `GET /api/v1/team-usage` | Team-level usage aggregation |
| `GET /health`, `GET /ready` | Liveness and readiness probes |

## Quick Start

### Docker Compose (local)

```bash
docker compose up
```

### Kubernetes

```bash
kubectl apply -f deploy/
```

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | — | PostgreSQL connection string |
| `PORT` | No | `8080` | HTTP listen port |

## CloudEvents Format

The service accepts [CloudEvents v1.0](https://cloudevents.io) with this data schema:

```json
{
  "specversion": "1.0",
  "id": "evt-<uuid>",
  "source": "maas-gateway",
  "type": "inference.tokens.used",
  "subject": "alice",
  "time": "2026-06-10T12:00:00Z",
  "datacontenttype": "application/json",
  "data": {
    "user": "alice",
    "group": "engineering",
    "subscription": "premium",
    "provider": "anthropic",
    "model": "claude-opus-4-8",
    "prompt_tokens": 150,
    "completion_tokens": 80,
    "total_tokens": 230,
    "cached_input_tokens": 12000,
    "cache_creation_tokens": 100,
    "reasoning_tokens": 0,
    "duration_ms": 1200
  }
}
```

This format is compatible with [OpenMeter](https://openmeter.io) and can be adapted for other billing backends.

## Supported Token Types

| Token Type | Description | OpenAI field | Anthropic field |
|-----------|-------------|-------------|-----------------|
| Input (new) | Non-cached input tokens | `prompt_tokens` | `input_tokens` |
| Output | Generated tokens | `completion_tokens` | `output_tokens` |
| Cached read | Tokens read from cache | `prompt_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| Cache write | Tokens written to cache | — | `cache_creation_input_tokens` |
| Reasoning | Chain-of-thought tokens | `completion_tokens_details.reasoning_tokens` | — |

## Simulating Metering Providers

This service acts as a **drop-in simulator** for metering backends. The `external-metering` IPP plugin sends CloudEvents to whatever URL is configured — point it at this service for development, OpenMeter for staging, or Monetize360 for production.

```
Development:  meteringURL → ai-gateway-metering-service (this repo)
Staging:      meteringURL → OpenMeter
Production:   meteringURL → Monetize360 / custom billing
```

The CloudEvents v1.0 format is the integration contract — any backend that accepts this schema works.

## Model Pricing

Cost calculation uses per-model pricing from the `model_pricing` table. Prices
are sourced from [LiteLLM's community-maintained database](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) (MIT licensed).

**On startup**, the service:
1. Fetches the latest pricing from LiteLLM's GitHub (10s timeout)
2. Filters to relevant models (Claude, GPT, Gemini)
3. UPSERTs into `model_pricing` (corrects stale prices, adds new models)
4. Falls back to a bundled snapshot if the fetch fails (air-gapped / offline)

**To refresh pricing without restarting:**
```bash
curl -X POST https://<metering-dashboard-route>/api/v1/admin/pricing/refresh
# Returns: { "updated": 3, "total": 239, "source": "fetched", "changed": [...] }
```

**To refresh on restart:**
```bash
oc rollout restart deployment/metering-service -n openshift-ingress
```

Pricing fields per model: `input_cost_per_mtok`, `output_cost_per_mtok`,
`cache_read_cost_per_mtok`, `cache_write_cost_per_mtok` (all USD per million tokens).

## Database

PostgreSQL 14+. Schema is auto-migrated on startup:

- `usage_events` — per-request token usage records
- `model_pricing` — per-model cost rates (auto-synced from LiteLLM on startup)

## Related

- [AI Inference Gateway](https://github.com/opendatahub-io/ai-gateway-payload-processing) — the gateway and plugin framework
- [external-metering plugin](https://github.com/opendatahub-io/ai-gateway-payload-processing/tree/main/pkg/plugins/external-metering) — the IPP plugin that sends events to this service
- [OpenMeter](https://openmeter.io) — production-grade usage metering
- [RHAISTRAT-1919](https://redhat.atlassian.net/browse/RHAISTRAT-1919) — tracking Jira

## License

Apache License 2.0

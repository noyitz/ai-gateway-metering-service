#!/usr/bin/env bash
# manage-keys.sh — mint MaaS API keys on the dogfood cluster (admin onboarding flow).
#
# Usage:
#   ./manage-keys.sh <email> <group> <admin=y|n> [first_name last_name] [key_name]
#
# Examples:
#   ./manage-keys.sh newuser@redhat.com octo-eng n
#   ./manage-keys.sh newuser@redhat.com octo-eng n Jane Doe
#   ./manage-keys.sh newuser@redhat.com octo-eng n Jane Doe jane-claude
#
# Behavior:
#   - Creates a key via maas-api POST /v1/api-keys on behalf of <email>.
#   - The plaintext key is printed ONCE (maas-api stores only the hash).
#   - When first/last name are given they are stored on the key and
#     auto-populate the metering dashboard's user profile the first time
#     the user logs in (no manual seeding needed).
#   - admin=y additionally adds the "admin" group to the key (the metering
#     dashboard's admin view is controlled by ADMIN_USERS, not key groups).
#
# Requires: oc + kubectl with access to the cluster, run from a machine
# that can reach the cluster API.
set -euo pipefail

usage() {
	echo "usage: $0 <email> <group> <admin=y|n> [first_name last_name] [key_name]" >&2
	exit 1
}
[ $# -ge 3 ] || usage

EMAIL="$1"
GROUP="$2"
ADMIN="$3"
FIRST="${4:-}"
LAST="${5:-}"
KEY_NAME="${6:-${EMAIL%%@*}-key}"

case "$ADMIN" in y|n) ;; *) usage ;; esac

NS="${NS:-ai-gateway-dogfood}"
MAAS_API_DEPLOY="${MAAS_API_DEPLOY:-maas-api}"
PF_PORT="${PF_PORT:-18081}"
TENANT="${MAAS_TENANT:-models-as-a-service}"

GROUPS="[$GROUP"
if [ "$ADMIN" = "y" ]; then
	GROUPS="$GROUPS, admin"
fi
GROUPS="$GROUPS]"

# SA bearer token (same auth the in-cluster metering service uses)
TOKEN="$(oc exec -n "$NS" "deploy/$MAAS_API_DEPLOY" -- cat /var/run/secrets/kubernetes.io/serviceaccount/token)"

PF_PID=""
cleanup() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT

kubectl -n "$NS" port-forward "svc/$MAAS_API_DEPLOY" "$PF_PORT:8080" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do
	curl -s -o /dev/null "http://127.0.0.1:$PF_PORT/health" 2>/dev/null && break
	sleep 0.5
done

RESPONSE="$(curl -sS -X POST "http://127.0.0.1:$PF_PORT/v1/api-keys" \
	-H "Content-Type: application/json" \
	-H "Authorization: Bearer $TOKEN" \
	-H "X-MaaS-Username: $EMAIL" \
	-H "X-MaaS-Group: $GROUPS" \
	-H "X-MaaS-Tenant: $TENANT" \
	-d "{\"name\": \"$KEY_NAME\"$( [ -n "$FIRST" ] && printf ', "firstName": "%s"' "$FIRST" )$( [ -n "$LAST" ] && printf ', "lastName": "%s"' "$LAST" )}")" || {
	echo "key creation failed" >&2
	exit 1
}

echo "$RESPONSE" | python3 -m json.tool 2>/dev/null || echo "$RESPONSE"
echo
echo "Note: the key above is shown only once — copy it now."

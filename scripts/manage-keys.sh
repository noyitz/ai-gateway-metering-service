#!/usr/bin/env bash
# Manage API keys on the dogfood cluster (PriceTag stack).
#
# This is the canonical key-management tool: maas-api owns the keys, and
# pricing, metering, and the dashboard's self-service key page all live in
# this repo. (A stale copy existed under praxis-ai/scripts/ — removed.)
#
# Usage:
#   ./scripts/manage-keys.sh create <username> [group] [--admin] [--name "First Last"]
#   ./scripts/manage-keys.sh list [username]
#   ./scripts/manage-keys.sh list-groups
#   ./scripts/manage-keys.sh list-admins
#   ./scripts/manage-keys.sh set-admin <username>
#   ./scripts/manage-keys.sh remove-admin <username>
#   ./scripts/manage-keys.sh set-group <username> <group>
#   ./scripts/manage-keys.sh revoke <username> [key-name]
#   ./scripts/manage-keys.sh revoke-user <username>
#   ./scripts/manage-keys.sh delete <username>
#
# Requires: MAAS_ADMIN_USER env var — the admin identity maas-api sees
# (maas-api trusts in-cluster identity headers; without a key this tool
# acts on a target user's behalf).
#
# Examples:
#   ./scripts/manage-keys.sh create jane.doe@example.com
#   ./scripts/manage-keys.sh create jane.doe@example.com executive --admin
#   ./scripts/manage-keys.sh create jane.doe@example.com --name "Jane Doe"
#   ./scripts/manage-keys.sh list
#   ./scripts/manage-keys.sh list jane.doe@example.com
#   ./scripts/manage-keys.sh revoke jane.doe@example.com dogfood-jane.doe
#   ./scripts/manage-keys.sh revoke-user test-user@example.com

set -euo pipefail

NAMESPACE="ai-gateway-dogfood"

show_help() {
    cat <<'HELP'
Usage: ./scripts/manage-keys.sh <command> [options]

Commands:
  create <email> [group] [--admin] [--name "First Last"]
                                      Create an API key for a user; --name
                                      also records their dashboard display name
  list [email]                        List active keys (all or for one user)
  list-groups                         Show all groups and their member counts
  list-admins                         Show current dashboard admins
  set-admin <email>                   Grant dashboard admin access
  remove-admin <email>                Revoke dashboard admin access
  set-group <email> <group>            Change a user's group (re-keys)
  revoke <email> [key-name]           Disable one (or all of) a user's keys
  revoke-user <email>                 Disable all of a user's keys
  delete <email>                      Permanently remove user and usage data

Environment:
  MAAS_ADMIN_USER (required)          Admin identity maas-api sees
  PF_PORT, MAAS_API_DEPLOY            Port-forward target (defaults below)

Options:
  --help, -h    Show this help

Examples:
  ./scripts/manage-keys.sh create jane.doe@example.com
  ./scripts/manage-keys.sh create jane.doe@example.com executive --admin
  ./scripts/manage-keys.sh list
  ./scripts/manage-keys.sh list jane.doe@example.com
  ./scripts/manage-keys.sh list-groups
  ./scripts/manage-keys.sh list-admins
  ./scripts/manage-keys.sh set-admin someone@example.com
  ./scripts/manage-keys.sh remove-admin someone@example.com
  ./scripts/manage-keys.sh set-group someone@example.com sw-eng
  ./scripts/manage-keys.sh revoke jane.doe@example.com
  ./scripts/manage-keys.sh revoke-user test-user@example.com
  ./scripts/manage-keys.sh delete test-user@example.com

Group defaults to "ai-eng" if omitted.
HELP
    exit 0
}

[[ "${1:-}" == "--help" || "${1:-}" == "-h" || -z "${1:-}" ]] && show_help

ACTION="$1"
shift

if ! oc whoami > /dev/null 2>&1; then
    echo "ERROR: not logged in to OpenShift. Run: oc login ..."
    exit 1
fi

# ── Port-forward ─────────────────────────────────────────────

PF_PORT="${PF_PORT:-18080}"
MAAS_API_DEPLOY="${MAAS_API_DEPLOY:-maas-api}"
API_BASE="http://localhost:$PF_PORT"

oc -n "$NAMESPACE" port-forward "svc/$MAAS_API_DEPLOY" "$PF_PORT:8080" > /dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null' EXIT
for _ in $(seq 1 20); do
    curl -s -o /dev/null "$API_BASE/health" 2>/dev/null && break
    sleep 0.5
done

# maas-api trusts in-cluster identity headers; MAAS_ADMIN_USER is the
# admin identity it sees. No default: failing closed beats silently
# acting as someone, and this repo is public.
ADMIN_USER="${MAAS_ADMIN_USER:-}"
if [[ -z "$ADMIN_USER" ]]; then
    echo "ERROR: MAAS_ADMIN_USER is required (the admin identity maas-api sees)." >&2
    echo "  export MAAS_ADMIN_USER=you@redhat.com" >&2
    exit 1
fi
ADMIN_GROUP='["ai-eng"]'

# ── Helpers ──────────────────────────────────────────────────

api() {
    curl -s "$@" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $ADMIN_USER" \
        -H "X-MaaS-Group: $ADMIN_GROUP"
}

# Usernames land in SQL literals (psql -c "... '$USERNAME'"), JSON bodies,
# and identity headers; groups land in JSON arrays. A stray quote or comma
# breaks out of all of them, so reject anything that isn't shaped like the
# plain emails/groups this stack actually uses before it reaches either.
require_email() {
    [[ "$1" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] \
        || { echo "ERROR: not a valid email address: '$1'" >&2; exit 1; }
}

require_group() {
    [[ "$1" =~ ^[A-Za-z0-9._-]+$ ]] \
        || { echo "ERROR: not a valid group name: '$1' (letters, digits, . _ -)" >&2; exit 1; }
}

# ── Admin helpers ────────────────────────────────────────────

get_admin_list() {
    oc -n "$NAMESPACE" get deployment metering-service \
        -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="ADMIN_USERS")].value}' 2>/dev/null
}

set_admin_list() {
    local new_list="$1"
    oc -n "$NAMESPACE" set env deployment/metering-service "ADMIN_USERS=$new_list" > /dev/null 2>&1
}

add_admin() {
    local email="$1"
    local current
    current=$(get_admin_list)
    if echo ",$current," | grep -q ",$email,"; then
        echo "$email is already an admin"
        return 0
    fi
    if [[ -z "$current" ]]; then
        set_admin_list "$email"
    else
        set_admin_list "$current,$email"
    fi
    echo "Added $email as admin (deployment will restart)"
}

remove_admin() {
    local email="$1"
    local current
    current=$(get_admin_list)
    local new_list
    new_list=$(echo "$current" | tr ',' '\n' | grep -v "^${email}$" | paste -sd ',' -)
    if [[ "$current" == "$new_list" ]]; then
        echo "$email is not an admin"
        return 0
    fi
    set_admin_list "$new_list"
    echo "Removed $email from admins (deployment will restart)"
}

# ── Actions ──────────────────────────────────────────────────

# revoke-user is `revoke` without the optional key-name filter
[[ "$ACTION" == "revoke-user" ]] && ACTION="revoke"

case "$ACTION" in

create)
    if [[ -z "${1:-}" ]]; then
        echo "Usage: $0 create <email> [group] [--admin] [--name \"First Last\"]"
        echo "  e.g.: $0 create noyitz@redhat.com --name 'Noy Itzikowitz'"
        exit 1
    fi
    USERNAME="$1"
    KEY_NAME="dogfood-${USERNAME%%@*}"
    MAKE_ADMIN=false
    GROUP="ai-eng"
    DISPLAY_NAME=""
    shift
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --admin) MAKE_ADMIN=true; shift ;;
            --name)
                [[ -z "${2:-}" ]] && { echo "ERROR: --name requires a value" >&2; exit 1; }
                DISPLAY_NAME="$2"; shift 2 ;;
            *) GROUP="$1"; shift ;;
        esac
    done
    require_email "$USERNAME"
    require_group "$GROUP"
    if [[ -n "$DISPLAY_NAME" ]]; then
        # Each token: letters with . - ' allowed inside (O'Brien, Jr.).
        # One token = first name only; the rest is the last name (the
        # profile schema allows an empty last). Pattern in a variable so
        # the apostrophe doesn't break [[ ]] parsing; a SQL-escaped copy
        # goes into the upsert below.
        local_name_re="[A-Za-z][A-Za-z.'-]*"
        [[ "$DISPLAY_NAME" =~ ^${local_name_re}([[:space:]]+${local_name_re})*$ ]] \
            || { echo "ERROR: --name wants a plain 'First Last' display name (letters, ., -, '); got: '$DISPLAY_NAME'" >&2; exit 1; }
    fi

    # Single identity header (the target user): maas-api mints on the
    # caller's behalf, and a stacked admin+target pair would rely on
    # first-wins header semantics.
    RESPONSE=$(curl -s -X POST "$API_BASE/v1/api-keys" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $USERNAME" \
        -H "X-MaaS-Group: [\"$GROUP\"]" \
        -d "{\"name\":\"$KEY_NAME\",\"description\":\"Dogfood gateway key for $USERNAME\",\"expiresIn\":\"8760h\"}")

    KEY=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('key',''))" 2>/dev/null)
    EXPIRES=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('expiresAt','?'))" 2>/dev/null)

    if [[ -z "$KEY" ]]; then
        echo "ERROR: failed to create key"
        echo "$RESPONSE" | python3 -m json.tool 2>/dev/null || echo "$RESPONSE"
        exit 1
    fi

    # Display name: capture it at the moment the admin knows who this is.
    # The dashboard resolves "First Last" from user_profiles and falls back
    # to the raw username, so an upsert here makes their usage show named
    # from the first request — no follow-up visit to the Display Names card.
    NAME_SET=false
    if [[ -n "$DISPLAY_NAME" ]]; then
        FIRST="${DISPLAY_NAME%%[[:space:]]*}"
        LAST="${DISPLAY_NAME#*[[:space:]]}"
        [[ "$LAST" == "$DISPLAY_NAME" ]] && LAST=""   # single token: first name only
        # :var is psql's variable syntax — it quotes/escapes the value itself,
        # so apostrophes (O'Brien) need no manual escaping here. Note psql -c
        # sends its string to the server un-parsed (no interpolation), so the
        # SQL goes in on stdin with oc exec -i.
        if oc -n "$NAMESPACE" exec -i postgresql-0 -- psql -U aigateway -d aigateway -q \
            -v uname="$USERNAME" -v fname="$FIRST" -v lname="$LAST" <<'SQL'
INSERT INTO user_profiles (username, first_name, last_name, updated_at)
VALUES (:'uname', :'fname', :'lname', NOW())
ON CONFLICT (username) DO UPDATE SET
    first_name = EXCLUDED.first_name,
    last_name = EXCLUDED.last_name,
    updated_at = NOW();
SQL
        then
            NAME_SET=true
        else
            echo "WARN: key was created, but the display-name upsert failed — set it on the dashboard's Display Names card" >&2
        fi
    fi

    # Setup instructions live on the welcome page — one source of truth,
    # correct per-tool recipes (route, dialect, /v1 placement). Derive the
    # URL from the cluster so this script never hardcodes a host that can
    # drift. Override with WELCOME_URL=... if a deployment fronts a host.
    WELCOME_URL="${WELCOME_URL:-https://$(oc -n "$NAMESPACE" get route dashboard -o jsonpath='{.spec.host}' 2>/dev/null)/welcome}"

    echo ""
    echo "=========================================="
    echo "  API Key Created"
    echo "=========================================="
    echo ""
    echo "  User:    $USERNAME"
    echo "  Group:   $GROUP"
    echo "  Expires: $EXPIRES"
    if [[ "$NAME_SET" == "true" ]]; then
        echo "  Display: $DISPLAY_NAME (recorded)"
    fi
    echo ""
    echo "  Send to the user — the key and one link:"
    echo ""
    echo "    $KEY"
    echo "    $WELCOME_URL"
    echo ""
    if [[ "$NAME_SET" != "true" ]]; then
        echo "  Next: add their display name on the dashboard admin page (Display Names card),"
        echo "        or re-run with --name \"First Last\"."
        echo ""
    fi

    if [[ "$MAKE_ADMIN" == "true" ]]; then
        add_admin "$USERNAME"
    fi
    ;;

list)
    USERNAME="${1:-}"
    if [[ -n "$USERNAME" ]]; then
        require_email "$USERNAME"
        RESPONSE=$(curl -s -X POST "$API_BASE/v1/api-keys/search" \
            -H "Content-Type: application/json" \
            -H "X-MaaS-Username: $USERNAME" \
            -H "X-MaaS-Group: $ADMIN_GROUP" \
            -d "{}")
    else
        RESPONSE=$(api -X POST "$API_BASE/v1/api-keys/search" -d "{}")
    fi

    echo "$RESPONSE" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
except (json.JSONDecodeError, ValueError):
    print('No keys found.')
    sys.exit(0)
if data is None or not isinstance(data, dict):
    print('No keys found.')
    sys.exit(0)
keys = [k for k in (data.get('data') or []) if k.get('status') == 'active']
if not keys:
    print('No keys found.')
    sys.exit(0)
hdr = '{:<38} {:<25} {:<30} {:<10} {}'.format('ID', 'Name', 'User', 'Status', 'Last Used')
print(hdr)
print('-' * len(hdr))
for k in keys:
    last = k.get('lastUsedAt', 'never') or 'never'
    if len(last) > 19:
        last = last[:19]
    print('{:<38} {:<25} {:<30} {:<10} {}'.format(k['id'], k['name'], k['username'], k['status'], last))
print('\nTotal: {} key(s)'.format(len(keys)))
"
    ;;

list-groups)
    RESPONSE=$(oc -n "$NAMESPACE" exec postgresql-0 -- psql -U aigateway -d aigateway -t -q -c "
        SELECT g, count(DISTINCT username) AS users, count(*) AS keys
        FROM api_keys, unnest(user_groups) AS g
        WHERE status = 'active'
        GROUP BY g
        ORDER BY users DESC, g;
    " 2>/dev/null)

    if [[ -z "$RESPONSE" ]]; then
        echo "No groups found."
        exit 0
    fi

    printf '%-20s %s %s\n' 'GROUP' 'USERS' 'KEYS'
    printf '%-20s %s %s\n' '-----' '-----' '----'
    echo "$RESPONSE" | while IFS='|' read -r grp users keys; do
        grp=$(echo "$grp" | xargs)
        users=$(echo "$users" | xargs)
        keys=$(echo "$keys" | xargs)
        [[ -z "$grp" ]] && continue
        printf '%-20s %5s %5s\n' "$grp" "$users" "$keys"
    done
    ;;

revoke)
    if [[ -z "${1:-}" ]]; then
        echo "Usage: $0 revoke <email> [key-name]"
        exit 1
    fi
    USERNAME="$1"
    KEY_NAME="${2:-}"
    require_email "$USERNAME"

    # Search as the target user to see their keys
    RESPONSE=$(curl -s -X POST "$API_BASE/v1/api-keys/search" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $USERNAME" \
        -H "X-MaaS-Group: $ADMIN_GROUP" \
        -d "{}")

    # KEY_NAME_FILTER reaches python via the environment: the key name is
    # operator input and must not be spliced into the interpreter source.
    KEYS=$(KEY_NAME_FILTER="$KEY_NAME" python3 -c "
import json, os, sys
data = json.load(sys.stdin)
name = os.environ.get('KEY_NAME_FILTER', '')
for k in (data.get('data') or []):
    if k['status'] != 'active':
        continue
    if name and k['name'] != name:
        continue
    print(k['id'] + '|' + k['name'])
" <<< "$RESPONSE" 2>/dev/null)

    if [[ -z "$KEYS" ]]; then
        if [[ -n "$KEY_NAME" ]]; then
            echo "No active key named '$KEY_NAME' for $USERNAME"
        else
            echo "No active keys found for $USERNAME"
        fi
        exit 1
    fi

    COUNT=0
    while IFS='|' read -r ID NAME; do
        curl -s -X DELETE "$API_BASE/v1/api-keys/$ID" \
            -H "Content-Type: application/json" \
            -H "X-MaaS-Username: $USERNAME" \
            -H "X-MaaS-Group: $ADMIN_GROUP" > /dev/null
        echo "Revoked: $NAME ($ID)"
        COUNT=$((COUNT + 1))
    done <<< "$KEYS"
    echo "Revoked $COUNT key(s) for $USERNAME"
    ;;

delete)
    if [[ -z "${1:-}" ]]; then
        echo "Usage: $0 delete <email>"
        exit 1
    fi
    USERNAME="$1"
    require_email "$USERNAME"

    # Count what will be deleted
    KEY_COUNT=$(curl -s -X POST "$API_BASE/v1/api-keys/search" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $USERNAME" \
        -H "X-MaaS-Group: $ADMIN_GROUP" \
        -d "{}" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('data',[])))" 2>/dev/null)

    # One row: <events> <profiles> <unclaimed invites>
    read -r EVENT_COUNT PROFILE_COUNT INVITE_COUNT <<< "$(oc -n "$NAMESPACE" exec postgresql-0 -- psql -U aigateway -d aigateway -t -q \
        -c "SELECT (SELECT COUNT(*) FROM usage_events WHERE username = '$USERNAME')
                 || ' ' || (SELECT COUNT(*) FROM user_profiles WHERE username = '$USERNAME')
                 || ' ' || (SELECT COUNT(*) FROM key_invites WHERE claimed_at IS NULL
                            AND revoked_at IS NULL
                            AND person_slug IN (SELECT person_slug FROM person_identities
                                                WHERE username = '$USERNAME'));" 2>/dev/null | tr -d '[:space:]')"

    echo ""
    echo "WARNING: This will permanently delete all data for $USERNAME:"
    echo ""
    echo "  - $KEY_COUNT API key(s) (active and revoked)"
    echo "  - $EVENT_COUNT metering event(s)"
    echo "  - $PROFILE_COUNT display-name profile(s), $INVITE_COUNT unclaimed key invite(s)"
    echo ""
    echo "  This cannot be undone. If you just want to disable access,"
    echo "  use 'revoke' instead — it keeps the history."
    echo ""
    read -p "  Type 'yes' to confirm: " CONFIRM

    if [[ "$CONFIRM" != "yes" ]]; then
        echo "Cancelled."
        exit 0
    fi

    # Revoke all active keys first
    KEYS=$(curl -s -X POST "$API_BASE/v1/api-keys/search" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $USERNAME" \
        -H "X-MaaS-Group: $ADMIN_GROUP" \
        -d "{}" | python3 -c "
import sys, json
for k in json.load(sys.stdin).get('data', []):
    if k['status'] == 'active':
        print(k['id'])
" 2>/dev/null)

    while read -r ID; do
        [[ -z "$ID" ]] && continue
        curl -s -X DELETE "$API_BASE/v1/api-keys/$ID" \
            -H "Content-Type: application/json" \
            -H "X-MaaS-Username: $USERNAME" \
            -H "X-MaaS-Group: $ADMIN_GROUP" > /dev/null
    done <<< "$KEYS"

    # Hard-delete what the API intentionally soft-deletes, in FK-safe
    # order (key_invites still resolves person_identities via subquery).
    # maas-api validates keys with a fresh Postgres lookup on every
    # request (Service.ValidateAPIKey -> PostgresStore.GetByHash — no
    # in-memory key cache), so this takes effect on the next request, no
    # maas-api restart. Ephemeral tokens are stateless JWTs (<= 1h TTL)
    # and lapse on their own. The org directory's people row is left
    # alone: the org chart is HR data, not user data.
    if ! oc -n "$NAMESPACE" exec postgresql-0 -- psql -U aigateway -d aigateway -q -c "
        DELETE FROM key_invites WHERE claimed_at IS NULL
            AND revoked_at IS NULL
            AND person_slug IN (SELECT person_slug FROM person_identities
                                WHERE username = '$USERNAME');
        DELETE FROM person_identities WHERE username = '$USERNAME';
        DELETE FROM user_profiles WHERE username = '$USERNAME';
        DELETE FROM usage_events WHERE username = '$USERNAME';
        DELETE FROM api_keys WHERE username = '$USERNAME';
    " ; then
        echo "ERROR: hard delete failed — check the error above; data may be partially deleted" >&2
        exit 1
    fi

    echo "Deleted all data for $USERNAME"
    ;;

set-group)
    if [[ -z "${1:-}" || -z "${2:-}" ]]; then
        echo "Usage: $0 set-group <email> <group>"
        exit 1
    fi
    USERNAME="$1"
    NEW_GROUP="$2"
    require_email "$USERNAME"
    require_group "$NEW_GROUP"

    # Revoke existing keys
    KEYS=$(curl -s -X POST "$API_BASE/v1/api-keys/search" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $USERNAME" \
        -H "X-MaaS-Group: $ADMIN_GROUP" \
        -d "{}" | python3 -c "
import sys, json
for k in json.load(sys.stdin).get('data', []):
    if k['status'] == 'active':
        print(k['id'] + '|' + k['name'])
" 2>/dev/null)

    if [[ -z "$KEYS" ]]; then
        echo "No active keys found for $USERNAME — creating new key in group $NEW_GROUP"
    else
        while IFS='|' read -r ID NAME; do
            curl -s -X DELETE "$API_BASE/v1/api-keys/$ID" \
                -H "Content-Type: application/json" \
                -H "X-MaaS-Username: $USERNAME" \
                -H "X-MaaS-Group: $ADMIN_GROUP" > /dev/null
            echo "Revoked old key: $NAME"
        done <<< "$KEYS"
    fi

    # Create new key in the new group (single identity header, the
    # target user — see create for why not stacked)
    KEY_NAME="dogfood-${USERNAME%%@*}"
    RESPONSE=$(curl -s -X POST "$API_BASE/v1/api-keys" \
        -H "Content-Type: application/json" \
        -H "X-MaaS-Username: $USERNAME" \
        -H "X-MaaS-Group: [\"$NEW_GROUP\"]" \
        -d "{\"name\":\"$KEY_NAME\",\"description\":\"Dogfood gateway key for $USERNAME\",\"expiresIn\":\"8760h\"}")

    KEY=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('key',''))" 2>/dev/null)
    if [[ -z "$KEY" ]]; then
        echo "ERROR: failed to create new key"
        echo "$RESPONSE" | python3 -m json.tool 2>/dev/null || echo "$RESPONSE"
        exit 1
    fi

    echo ""
    echo "Moved $USERNAME to group: $NEW_GROUP"
    echo "New key: $KEY"
    echo ""
    echo "NOTE: the user must update their ANTHROPIC_API_KEY with this new key."
    ;;

list-admins)
    ADMINS=$(get_admin_list)
    if [[ -z "$ADMINS" ]]; then
        echo "No admins configured."
    else
        echo "Dashboard admins:"
        echo "$ADMINS" | tr ',' '\n' | while read -r admin; do
            echo "  $admin"
        done
    fi
    ;;

set-admin)
    if [[ -z "${1:-}" ]]; then
        echo "Usage: $0 set-admin <email>"
        exit 1
    fi
    require_email "$1"
    add_admin "$1"
    ;;

remove-admin)
    if [[ -z "${1:-}" ]]; then
        echo "Usage: $0 remove-admin <email>"
        exit 1
    fi
    require_email "$1"
    remove_admin "$1"
    ;;

*)
    echo "Unknown action: $ACTION"
    echo "Usage: $0 <create|list|list-groups|list-admins|set-admin|remove-admin|set-group|revoke|delete> ..."
    exit 1
    ;;
esac

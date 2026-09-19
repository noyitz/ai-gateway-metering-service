#!/usr/bin/env bash
# Provision the object-store backup target for the dogfood PriceTag cluster:
# a dedicated Cloud Object Storage instance, a bucket, and one Writer service
# key, delivered as the Kubernetes secret CloudNativePG's barman object store
# consumes. Run once; safe to re-run.
#
# Why a service key (HMAC) and not an IAM API key: COS requires every
# CreateBucket to name its owning instance, which the S3 protocol carries
# only via IBM extension headers (aws-cli cannot send them) or inside
# instance-bound HMAC credentials. HMAC keys are IBM COS's own credential
# format — no AWS involved — and they're what barman-cloud has always used.
#
# Secret hygiene: the key JSON is written only to a 0700 temp file, piped
# straight into the cluster secret, and removed at the end. It never
# appears in stdout or the transcript. Re-running rotates it: the old
# service key is destroyed and the secret replaced in the same run.
set -euo pipefail

# ---- knobs (override via environment) ------------------------------------
RG="${COS_RG:-maas-poc}"
REGION="${COS_REGION:-us-south}"
ENDPOINT="https://s3.${REGION}.cloud-object-storage.appdomain.cloud"
# Reuses the account's active COS instance. A dedicated instance (e.g.
# COS_INSTANCE=aigateway-db-backups) is the cleaner shape, but the
# `standard global` create never materializes on this account (accepted,
# shows briefly, then vanishes — billing/entitlement side), so the bucket
# lands in the working instance instead. The Writer service key is scoped
# to the whole instance — noted as a caveat, tighten if a bucket-scoped
# credential ever becomes scriptable.
INSTANCE_NAME="${COS_INSTANCE:-myvpc-cos}"
# Bucket names are globally unique across IBM COS — keep the random suffix.
BUCKET="${COS_BUCKET:-aigateway-dogfood-backups-b6bd93}"
SVC_KEY="${COS_SERVICE_KEY:-aigateway-backup-writer}"
NS="${BACKUP_NS:-ai-gateway-dogfood}"
SECRET="${BACKUP_SECRET:-cnpg-backup-cos}"
# Leftovers from the first (IAM service-ID) revision of this script; best
# effort, ignore failures.
OLD_SID="${COS_SERVICE_ID:-aigateway-dogfood-backup}"
OLD_KEY="${COS_API_KEY_NAME:-aigateway-backup-key}"

KEYFILE=""
cleanup() { [ -n "$KEYFILE" ] && rm -f "$KEYFILE" "${KEYFILE}.check"; }
trap cleanup EXIT

say() { printf '%s\n' "$*"; }

# ---- 0. prerequisites -----------------------------------------------------
ibmcloud account show >/dev/null 2>&1 || { say "FAIL: not logged in to ibmcloud"; exit 1; }
ibmcloud target -g "$RG" >/dev/null
KEYFILE=$(mktemp); chmod 700 "$KEYFILE"

# ---- 1. dedicated COS instance (idempotent) --------------------------------
INST_GUID=$(ibmcloud resource service-instance "$INSTANCE_NAME" -o json 2>/dev/null \
  | python3 -c "import json,sys;d=json.load(sys.stdin);print(d[0]['id'].split(':')[-3] if d else '')" 2>/dev/null || true)
if [ -z "$INST_GUID" ]; then
  say "creating COS instance $INSTANCE_NAME (plan standard, rg $RG)"
  ibmcloud resource service-instance-create "$INSTANCE_NAME" cloud-object-storage standard global -g "$RG" -f >/dev/null
  INST_GUID=$(ibmcloud resource service-instance "$INSTANCE_NAME" -o json \
    | python3 -c "import json,sys;print(json.load(sys.stdin)[0]['id'].split(':')[-3])")
fi
say "instance: $INSTANCE_NAME ($INST_GUID)"

# ---- 2. tidy leftovers from the IAM-key revision --------------------------
ibmcloud iam service-api-key-delete "$OLD_KEY" "$OLD_SID" -f -q >/dev/null 2>&1 || true
for P in $(ibmcloud iam service-policies "$OLD_SID" -o json 2>/dev/null \
    | python3 -c "import json,sys;[print(p['id']) for p in json.load(sys.stdin)]" 2>/dev/null || true); do
  ibmcloud iam service-policy-delete "$OLD_SID" "$P" -f -q >/dev/null 2>&1 || true
done
ibmcloud iam service-id-delete "$OLD_SID" -f -q >/dev/null 2>&1 || true

# ---- 3. Writer service key on the instance (rotate on re-run) --------------
# COS Writer = create/modify/delete buckets + read/write objects, bound to
# this dedicated instance only.
ibmcloud resource service-key-delete "$SVC_KEY" -f -q >/dev/null 2>&1 \
  && say "destroyed previous service key" || true
say "creating Writer service key $SVC_KEY"
# "HMAC": true asks COS for the legacy keypair; without it IBM hands back
# an IAM apikey, which cannot create buckets through the S3 API (no
# instance-binding in the request).
ibmcloud resource service-key-create "$SVC_KEY" Writer --instance-name "$INSTANCE_NAME" \
  -p '{"HMAC":true}' -o json > "$KEYFILE"
read -r AK SK < <(python3 -c "
import json, sys
d = json.load(open('$KEYFILE'))
c = (d[0] if isinstance(d, list) else d).get('credentials', {})
h = c.get('cos_hmac_keys')
if not h: sys.exit('no HMAC in response')
print(h['access_key_id'], h['secret_access_key'])")
[ -n "$AK" ] && [ -n "$SK" ] || { say "FAIL: service key has no cos_hmac_keys"; exit 1; }

export AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" \
       AWS_DEFAULT_REGION="$REGION" AWS_REQUEST_CHECKSUM_CALCULATION=when_required
aws_args=(--endpoint-url "$ENDPOINT")

# ---- 4. bucket (idempotent) --------------------------------------------------
if aws "${aws_args[@]}" s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
  say "bucket $BUCKET already exists"
else
  say "creating bucket $BUCKET ($REGION)"
  aws "${aws_args[@]}" s3api create-bucket --bucket "$BUCKET" \
    --create-bucket-configuration LocationConstraint="$REGION" >/dev/null \
    || { say "FAIL: bucket create"; exit 1; }
fi

# ---- 5. end-to-end verification with the key CNPG will actually use ---------
say "verifying put/head/delete round-trip with the Writer key"
echo "provision-check $(date -u +%FT%TZ)" > "$KEYFILE.check"
aws "${aws_args[@]}" s3api put-object --bucket "$BUCKET" --key _provision-check \
  --body "$KEYFILE.check" >/dev/null
aws "${aws_args[@]}" s3api head-object --bucket "$BUCKET" --key _provision-check >/dev/null
aws "${aws_args[@]}" s3api delete-object --bucket "$BUCKET" --key _provision-check >/dev/null
say "round-trip OK"

# ---- 6. cluster secret (barman credentials for CNPG) ------------------------
say "writing secret $SECRET to namespace $NS"
python3 - "$AK" "$SK" "$NS" "$SECRET" <<'PY' | oc -n "$NS" apply -f -
import json, sys
sec = {
    "apiVersion": "v1", "kind": "Secret",
    "metadata": {"name": sys.argv[4], "namespace": sys.argv[3]},
    "stringData": {"ACCESS_KEY_ID": sys.argv[1], "SECRET_ACCESS_KEY": sys.argv[2]},
}
print(json.dumps(sec))
PY

# ---- summary ----------------------------------------------------------------
say ""
say "backup target ready:"
say "  bucket:   $BUCKET"
say "  endpoint: $ENDPOINT"
say "  region:   $REGION"
say "  secret:   $NS/$SECRET (ACCESS_KEY_ID, SECRET_ACCESS_KEY)"

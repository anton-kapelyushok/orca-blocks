set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
CONFIG=/etc/overlaybd-snapshotter/config.json
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
ENV_FILE="$WORK/mutable.env"
COMMIT_DIR="$WORK/commit"
BASE_REF="${REGISTRY_HOST}/${REPO}:${BASE_TAG}"
DERIVED_REF="${REGISTRY_HOST}/${REPO}:${DERIVED_TAG}"
REGISTRY_URL="http://${REGISTRY_HOST}"
mkdir -p "$COMMIT_DIR"

echo "Executing local: source $ENV_FILE"
# DEMO-CMD: . "$ENV_FILE"
. "$ENV_FILE"

echo "base_ref=$BASE_REF"
echo "derived_ref=$DERIVED_REF"
echo "writable_data=$WD"
echo "writable_index=$WI"

echo "Executing local: ls -lh $WD $WI"
# DEMO-CMD: ls -lh "$WD" "$WI"
ls -lh "$WD" "$WI"

COMMIT_OBD="$COMMIT_DIR/demo-touch-commit.obd"
commit_start_ms=$(date +%s%3N)
echo "Executing local: /opt/overlaybd/bin/overlaybd-commit -z -f $WD $WI $COMMIT_OBD"
# DEMO-CMD: /opt/overlaybd/bin/overlaybd-commit -z -f "$WD" "$WI" "$COMMIT_OBD" | tee "$COMMIT_DIR/overlaybd-commit.out"
/opt/overlaybd/bin/overlaybd-commit -z -f "$WD" "$WI" "$COMMIT_OBD" | tee "$COMMIT_DIR/overlaybd-commit.out"
commit_end_ms=$(date +%s%3N)
commit_ms=$((commit_end_ms - commit_start_ms))

LAYER_SIZE="$(stat -c%s "$COMMIT_OBD")"
LAYER_DIGEST="sha256:$(sha256sum "$COMMIT_OBD" | awk '{print $1}')"
echo "committed_layer=$COMMIT_OBD"
echo "committed_layer_digest=$LAYER_DIGEST"
echo "committed_layer_bytes=$LAYER_SIZE"

echo "Executing local: curl base manifest/config from $REGISTRY_URL"
# DEMO-CMD: curl -fsS -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' "$REGISTRY_URL/v2/$REPO/manifests/$BASE_TAG" >"$COMMIT_DIR/base-manifest.json"
curl -fsS -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  "$REGISTRY_URL/v2/$REPO/manifests/$BASE_TAG" >"$COMMIT_DIR/base-manifest.json"
BASE_CONFIG_DIGEST="$(jq -r '.config.digest' "$COMMIT_DIR/base-manifest.json")"
curl -fsS "$REGISTRY_URL/v2/$REPO/blobs/$BASE_CONFIG_DIGEST" >"$COMMIT_DIR/base-config.json"

upload_blob() {
  local file="$1"
  local digest="$2"
  local location
  local separator="?"
  location="$(curl -fsSI -X POST "$REGISTRY_URL/v2/$REPO/blobs/uploads/" | awk -F': ' 'tolower($1)=="location" {gsub("\r","",$2); print $2}' | tail -1)"
  if [[ "$location" == /* ]]; then
    location="$REGISTRY_URL$location"
  fi
  if [[ "$location" == *"?"* ]]; then
    separator="&"
  fi
  echo "Executing local: curl -X PUT upload blob $digest"
# DEMO-CMD: curl -fsS -X PUT -H 'Content-Type: application/octet-stream' --data-binary "@$file" "${location}${separator}digest=${digest}" >/dev/null
  curl -fsS -X PUT \
    -H 'Content-Type: application/octet-stream' \
    --data-binary "@$file" \
    "${location}${separator}digest=${digest}" >/dev/null
}

upload_start_ms=$(date +%s%3N)
upload_blob "$COMMIT_OBD" "$LAYER_DIGEST"
upload_layer_end_ms=$(date +%s%3N)

NOW="$(date -u +%FT%TZ)"
NEW_CONFIG="$COMMIT_DIR/derived-config.json"
echo "Executing local: jq append new diff_id/history to image config"
# DEMO-CMD: jq '.rootfs.diff_ids += [$diff] | .history += [...]' "$COMMIT_DIR/base-config.json" >"$NEW_CONFIG"
jq \
  --arg now "$NOW" \
  --arg diff "$LAYER_DIGEST" \
  --arg touch "$TOUCH_PATH" \
  '.created=$now
   | .rootfs.diff_ids += [$diff]
   | .history += [{"created": $now, "created_by": ("overlaybd native commit demo: touch " + $touch)}]' \
  "$COMMIT_DIR/base-config.json" >"$NEW_CONFIG"

CONFIG_SIZE="$(stat -c%s "$NEW_CONFIG")"
CONFIG_DIGEST="sha256:$(sha256sum "$NEW_CONFIG" | awk '{print $1}')"
upload_blob "$NEW_CONFIG" "$CONFIG_DIGEST"
upload_config_end_ms=$(date +%s%3N)

NEW_MANIFEST="$COMMIT_DIR/derived-manifest.json"
echo "Executing local: jq append OverlayBD layer to manifest"
# DEMO-CMD: jq '.layers += [{"annotations": {"containerd.io/snapshot/overlaybd/blob-digest": $layer_digest, ...}}]' "$COMMIT_DIR/base-manifest.json" >"$NEW_MANIFEST"
jq \
  --arg config_digest "$CONFIG_DIGEST" \
  --arg layer_digest "$LAYER_DIGEST" \
  --arg layer_size "$LAYER_SIZE" \
  --argjson config_size "$CONFIG_SIZE" \
  '.config.digest=$config_digest
   | .config.size=$config_size
   | .layers += [{
       "mediaType": "application/vnd.oci.image.layer.v1.tar",
       "digest": $layer_digest,
       "size": ($layer_size | tonumber),
       "annotations": {
         "containerd.io/snapshot/overlaybd/blob-digest": $layer_digest,
         "containerd.io/snapshot/overlaybd/blob-fs-type": "ext4",
         "containerd.io/snapshot/overlaybd/blob-size": $layer_size,
         "containerd.io/snapshot/overlaybd/version": "0.1.0"
       }
     }]' \
  "$COMMIT_DIR/base-manifest.json" >"$NEW_MANIFEST"

manifest_size="$(stat -c%s "$NEW_MANIFEST")"
push_manifest_start_ms=$(date +%s%3N)
echo "Executing local: curl -X PUT $REGISTRY_URL/v2/$REPO/manifests/$DERIVED_TAG"
# DEMO-CMD: curl -fsS -X PUT -H 'Content-Type: application/vnd.docker.distribution.manifest.v2+json' --data-binary "@$NEW_MANIFEST" "$REGISTRY_URL/v2/$REPO/manifests/$DERIVED_TAG"
curl -fsS -X PUT \
  -H 'Content-Type: application/vnd.docker.distribution.manifest.v2+json' \
  --data-binary "@$NEW_MANIFEST" \
  "$REGISTRY_URL/v2/$REPO/manifests/$DERIVED_TAG" >/dev/null
push_manifest_end_ms=$(date +%s%3N)

echo "Executing local: cleanup mutable container/snapshot"
# DEMO-CMD: $CTR -n "$NS" containers rm "$NAME"; $CTR -n "$NS" snapshots --snapshotter overlaybd rm "$NAME"
$CTR -n "$NS" containers rm "$NAME" || true
$CTR -n "$NS" snapshots --snapshotter overlaybd rm "$NAME" || true

upload_layer_ms=$((upload_layer_end_ms - upload_start_ms))
upload_config_ms=$((upload_config_end_ms - upload_layer_end_ms))
push_manifest_ms=$((push_manifest_end_ms - push_manifest_start_ms))
uploaded_bytes=$((LAYER_SIZE + CONFIG_SIZE + manifest_size))

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| overlaybd-commit | ${commit_ms} ms |"
echo "| upload committed layer | ${upload_layer_ms} ms |"
echo "| upload derived config | ${upload_config_ms} ms |"
echo "| push derived manifest | ${push_manifest_ms} ms |"
echo "| committed layer bytes | ${LAYER_SIZE} |"
echo "| derived config bytes | ${CONFIG_SIZE} |"
echo "| derived manifest bytes | ${manifest_size} |"
echo "| approximate transferred bytes | ${uploaded_bytes} |"
echo
echo "derived_image=$DERIVED_REF"
echo "rwMode after commit: $(jq -r .rwMode "$CONFIG")"

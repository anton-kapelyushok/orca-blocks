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
echo "upperdir=$UPPERDIR"
echo "overlaybd_config=$OBD_CONFIG_PATH"

echo "Executing local: find $UPPERDIR -maxdepth 4 -print"
# DEMO-CMD: find "$UPPERDIR" -maxdepth 4 -print | head -80
find "$UPPERDIR" -maxdepth 4 -print | head -80

DIFF_TAR="$COMMIT_DIR/demo-touch-upperdir-diff.tar"
export_start_ms=$(date +%s%3N)
echo "Executing local: export overlay upperdir as OCI diff tar $DIFF_TAR"
# DEMO-CMD: python3 - "$UPPERDIR" "$DIFF_TAR" "$UID_MAP" "$GID_MAP" <<'PY'
python3 - "$UPPERDIR" "$DIFF_TAR" "$UID_MAP" "$GID_MAP" <<'PY'
import os
import stat
import sys
import tarfile

upperdir, out_path, uid_map_text, gid_map_text = sys.argv[1:5]


def parse_id_map(text):
    rows = []
    for part in text.replace(";", "\n").splitlines():
        fields = part.split()
        if len(fields) == 3:
            inside, outside, length = map(int, fields)
            rows.append((inside, outside, length))
    return rows


uid_map = parse_id_map(uid_map_text)
gid_map = parse_id_map(gid_map_text)


def to_container_id(host_id, rows):
    for inside, outside, length in rows:
        if outside <= host_id < outside + length:
            return inside + (host_id - outside)
    return host_id


def tar_name(path):
    rel = os.path.relpath(path, upperdir)
    if rel == ".":
        return "."
    return rel.lstrip("./")


def add_regular_whiteout(tar, parent_arcname, basename, st):
    wh_name = os.path.join(parent_arcname, ".wh." + basename) if parent_arcname != "." else ".wh." + basename
    info = tarfile.TarInfo(wh_name)
    info.size = 0
    info.mode = 0o000
    info.mtime = int(st.st_mtime)
    info.uid = to_container_id(st.st_uid, uid_map)
    info.gid = to_container_id(st.st_gid, gid_map)
    tar.addfile(info)


def add_opaque_marker(tar, dir_arcname, st):
    marker = os.path.join(dir_arcname, ".wh..wh..opq") if dir_arcname != "." else ".wh..wh..opq"
    info = tarfile.TarInfo(marker)
    info.size = 0
    info.mode = 0o000
    info.mtime = int(st.st_mtime)
    info.uid = to_container_id(st.st_uid, uid_map)
    info.gid = to_container_id(st.st_gid, gid_map)
    tar.addfile(info)


def overlay_xattr(path, name):
    try:
        return os.getxattr(path, name)
    except OSError:
        return None


def fill_common(info, st):
    info.mode = stat.S_IMODE(st.st_mode)
    info.uid = to_container_id(st.st_uid, uid_map)
    info.gid = to_container_id(st.st_gid, gid_map)
    info.mtime = int(st.st_mtime)
    return info


with tarfile.open(out_path, "w", format=tarfile.PAX_FORMAT) as tar:
    for root, dirs, files in os.walk(upperdir, topdown=True):
        dirs.sort()
        files.sort()
        entries = list(dirs) + list(files)
        root_arc = tar_name(root)
        root_st = os.lstat(root)
        if root_arc != ".":
            info = fill_common(tarfile.TarInfo(root_arc + "/"), root_st)
            info.type = tarfile.DIRTYPE
            tar.addfile(info)
        if overlay_xattr(root, "trusted.overlay.opaque") in (b"y", b"x"):
            add_opaque_marker(tar, root_arc, root_st)

        for name in entries:
            path = os.path.join(root, name)
            arc = tar_name(path)
            st = os.lstat(path)
            mode = st.st_mode
            is_whiteout = stat.S_ISCHR(mode) and os.major(st.st_rdev) == 0 and os.minor(st.st_rdev) == 0
            is_whiteout = is_whiteout or overlay_xattr(path, "trusted.overlay.whiteout") is not None
            if is_whiteout:
                add_regular_whiteout(tar, root_arc, name, st)
                if name in dirs:
                    dirs.remove(name)
                continue
            if stat.S_ISDIR(mode):
                continue
            if stat.S_ISREG(mode):
                info = fill_common(tarfile.TarInfo(arc), st)
                info.size = st.st_size
                with open(path, "rb") as f:
                    tar.addfile(info, f)
            elif stat.S_ISLNK(mode):
                info = fill_common(tarfile.TarInfo(arc), st)
                info.type = tarfile.SYMTYPE
                info.linkname = os.readlink(path)
                tar.addfile(info)
            elif stat.S_ISFIFO(mode):
                info = fill_common(tarfile.TarInfo(arc), st)
                info.type = tarfile.FIFOTYPE
                tar.addfile(info)
            elif stat.S_ISCHR(mode) or stat.S_ISBLK(mode):
                info = fill_common(tarfile.TarInfo(arc), st)
                info.type = tarfile.CHRTYPE if stat.S_ISCHR(mode) else tarfile.BLKTYPE
                info.devmajor = os.major(st.st_rdev)
                info.devminor = os.minor(st.st_rdev)
                tar.addfile(info)
PY
export_end_ms=$(date +%s%3N)
export_ms=$((export_end_ms - export_start_ms))

APPLY_DATA="$COMMIT_DIR/demo-touch-writable-data"
APPLY_INDEX="$COMMIT_DIR/demo-touch-writable-index"
APPLY_CONFIG="$COMMIT_DIR/demo-touch-apply-config.v1.json"
APPLY_RESULT="$COMMIT_DIR/demo-touch-apply-result.log"
COMMIT_OBD="$COMMIT_DIR/demo-touch-commit.obd"

create_start_ms=$(date +%s%3N)
echo "Executing local: /opt/overlaybd/bin/overlaybd-create $APPLY_DATA $APPLY_INDEX 64"
# DEMO-CMD: /opt/overlaybd/bin/overlaybd-create "$APPLY_DATA" "$APPLY_INDEX" 64
/opt/overlaybd/bin/overlaybd-create "$APPLY_DATA" "$APPLY_INDEX" 64
create_end_ms=$(date +%s%3N)
create_ms=$((create_end_ms - create_start_ms))

echo "Executing local: jq --arg data $APPLY_DATA --arg index $APPLY_INDEX --arg result $APPLY_RESULT '.upper={data: \$data, index: \$index} | .resultFile=\$result' $OBD_CONFIG_PATH >$APPLY_CONFIG"
# DEMO-CMD: jq --arg data "$APPLY_DATA" --arg index "$APPLY_INDEX" --arg result "$APPLY_RESULT" '.upper={data: $data, index: $index} | .resultFile=$result' "$OBD_CONFIG_PATH" >"$APPLY_CONFIG"
jq \
  --arg data "$APPLY_DATA" \
  --arg index "$APPLY_INDEX" \
  --arg result "$APPLY_RESULT" \
  '.upper={data: $data, index: $index} | .resultFile=$result' \
  "$OBD_CONFIG_PATH" >"$APPLY_CONFIG"

apply_start_ms=$(date +%s%3N)
echo "Executing local: /opt/overlaybd/bin/overlaybd-apply $DIFF_TAR $APPLY_CONFIG"
# DEMO-CMD: /opt/overlaybd/bin/overlaybd-apply "$DIFF_TAR" "$APPLY_CONFIG" | tee "$COMMIT_DIR/overlaybd-apply.out"
set +e
/opt/overlaybd/bin/overlaybd-apply "$DIFF_TAR" "$APPLY_CONFIG" >"$COMMIT_DIR/overlaybd-apply.out" 2>&1
apply_rc=$?
set -e
cat "$COMMIT_DIR/overlaybd-apply.out"
if [[ "$apply_rc" -ne 0 ]]; then
  if [[ -f "$APPLY_RESULT" ]] && grep -q "success" "$APPLY_RESULT"; then
    echo "overlaybd-apply exited $apply_rc after writing success; continuing"
  else
    echo "overlaybd-apply failed with exit code $apply_rc"
    [[ -f "$APPLY_RESULT" ]] && cat "$APPLY_RESULT"
    exit "$apply_rc"
  fi
fi
apply_end_ms=$(date +%s%3N)
apply_ms=$((apply_end_ms - apply_start_ms))

commit_start_ms=$(date +%s%3N)
echo "Executing local: /opt/overlaybd/bin/overlaybd-commit -z -f $APPLY_DATA $APPLY_INDEX $COMMIT_OBD"
# DEMO-CMD: /opt/overlaybd/bin/overlaybd-commit -z -f "$APPLY_DATA" "$APPLY_INDEX" "$COMMIT_OBD" | tee "$COMMIT_DIR/overlaybd-commit.out"
/opt/overlaybd/bin/overlaybd-commit -z -f "$APPLY_DATA" "$APPLY_INDEX" "$COMMIT_OBD" | tee "$COMMIT_DIR/overlaybd-commit.out"
commit_end_ms=$(date +%s%3N)
commit_ms=$((commit_end_ms - commit_start_ms))

LAYER_SIZE="$(stat -c%s "$COMMIT_OBD")"
LAYER_DIGEST="sha256:$(sha256sum "$COMMIT_OBD" | awk '{print $1}')"
DIFF_ID="$LAYER_DIGEST"
echo "exported_diff_tar=$DIFF_TAR"
echo "committed_layer=$COMMIT_OBD"
echo "committed_layer_digest=$LAYER_DIGEST"
echo "committed_layer_bytes=$LAYER_SIZE"
echo "diff_id=$DIFF_ID"

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
  --arg diff "$DIFF_ID" \
  --arg touch "$TOUCH_PATH" \
  '.created=$now
   | .rootfs.diff_ids += [$diff]
   | .history += [{"created": $now, "created_by": ("overlayfs upperdir diff demo: touch " + $touch)}]' \
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
echo "| export upperdir diff tar | ${export_ms} ms |"
echo "| overlaybd-create writable pair | ${create_ms} ms |"
echo "| overlaybd-apply diff tar | ${apply_ms} ms |"
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

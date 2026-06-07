#!/usr/bin/env bash
set -euo pipefail

IMAGE=${1:-${IMAGE:-}}
ROOTFS_PATH=${2:-${ROOTFS_PATH:-}}
ASSET_DIR=${ASSET_DIR:-firecracker-assets}
ROOTFS_SIZE_MB=${ROOTFS_SIZE_MB:-2048}
FORCE=${FORCE:-false}
CONTAINER_RUNTIME=${CONTAINER_RUNTIME:-docker}

log() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }

usage() {
  cat >&2 <<'EOF'
usage:
  scripts/build-rootfs-from-image.sh IMAGE[:TAG] [OUT.ext4]

examples:
  scripts/build-rootfs-from-image.sh alpine:3.22 firecracker-assets/image-rootfs-alpine.ext4
  ROOTFS_SIZE_MB=4096 scripts/build-rootfs-from-image.sh ubuntu:24.04

env:
  CONTAINER_RUNTIME=docker|podman  runtime used on the host to pull/export the image
  ROOTFS_SIZE_MB=2048              ext4 image size
  FORCE=true                       overwrite OUT.ext4
EOF
}

if [[ -z "$IMAGE" ]]; then
  usage
  exit 2
fi

need "$CONTAINER_RUNTIME"
need go
need mkfs.ext4
need mount
need tar
need umount

mkdir -p "$ASSET_DIR"
ASSET_DIR=$(cd "$ASSET_DIR" && pwd)

safe_name=$(printf '%s' "$IMAGE" | tr '/:@' '---' | tr -cs 'A-Za-z0-9._-' '-')
if [[ -z "$ROOTFS_PATH" ]]; then
  ROOTFS_PATH="$ASSET_DIR/image-rootfs-${safe_name}.ext4"
fi
case "$ROOTFS_PATH" in
  /*) ;;
  *) ROOTFS_PATH="$(pwd)/$ROOTFS_PATH" ;;
esac

if [[ -e "$ROOTFS_PATH" && "$FORCE" != "true" ]]; then
  echo "refusing to overwrite existing rootfs: $ROOTFS_PATH" >&2
  echo "set FORCE=true or choose a different output path" >&2
  exit 1
fi

WORK_DIR=$(mktemp -d)
MOUNT_DIR=""
CID=""
cleanup() {
  if [[ -n "$CID" ]]; then
    "$CONTAINER_RUNTIME" rm -f "$CID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$MOUNT_DIR" ]] && mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    sudo umount "$MOUNT_DIR" || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

INIT_BIN="$WORK_DIR/orca-init"
INSPECT_JSON="$WORK_DIR/image-inspect.json"
ROOTFS_TAR="$WORK_DIR/rootfs.tar"

log "building static orca init"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o "$INIT_BIN" ./cmd/orca-init

log "pulling $IMAGE"
"$CONTAINER_RUNTIME" pull "$IMAGE"

log "capturing image metadata"
"$CONTAINER_RUNTIME" image inspect "$IMAGE" >"$INSPECT_JSON"

log "exporting merged image filesystem"
case "$CONTAINER_RUNTIME" in
  docker)
    CID=$("$CONTAINER_RUNTIME" create --entrypoint /bin/sh "$IMAGE" -c true)
    "$CONTAINER_RUNTIME" export "$CID" >"$ROOTFS_TAR"
    ;;
  podman)
    CID=$("$CONTAINER_RUNTIME" create --entrypoint /bin/sh "$IMAGE" -c true)
    "$CONTAINER_RUNTIME" export "$CID" >"$ROOTFS_TAR"
    ;;
  *)
    echo "unsupported CONTAINER_RUNTIME=$CONTAINER_RUNTIME" >&2
    exit 2
    ;;
esac
"$CONTAINER_RUNTIME" rm -f "$CID" >/dev/null
CID=""

log "creating ext4 rootfs $ROOTFS_PATH"
rm -f "$ROOTFS_PATH"
truncate -s "${ROOTFS_SIZE_MB}M" "$ROOTFS_PATH"
mkfs.ext4 -F "$ROOTFS_PATH" >/dev/null

MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$ROOTFS_PATH" "$MOUNT_DIR"

log "extracting image filesystem"
sudo tar --numeric-owner -xf "$ROOTFS_TAR" -C "$MOUNT_DIR"

log "injecting Orca init and metadata"
sudo mkdir -p "$MOUNT_DIR"/{dev,proc,sys,run,tmp,etc,orca}
if [[ -e "$MOUNT_DIR/init" && ! -e "$MOUNT_DIR/orca/original-init" ]]; then
  sudo mv "$MOUNT_DIR/init" "$MOUNT_DIR/orca/original-init"
fi
sudo install -m 0755 "$INIT_BIN" "$MOUNT_DIR/init"
sudo install -m 0644 "$INSPECT_JSON" "$MOUNT_DIR/etc/orca-image-inspect.json"
printf '%s\n' "$IMAGE" | sudo tee "$MOUNT_DIR/etc/orca-image-ref" >/dev/null
printf 'image=%s\nrootfs_size_mb=%s\ncontainer_runtime=%s\n' "$IMAGE" "$ROOTFS_SIZE_MB" "$CONTAINER_RUNTIME" |
  sudo tee "$MOUNT_DIR/etc/orca-rootfs-from-image" >/dev/null

log "unmounting rootfs"
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
MOUNT_DIR=""

log "created $ROOTFS_PATH"
ls -lh "$ROOTFS_PATH"

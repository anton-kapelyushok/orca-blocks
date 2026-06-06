#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
FIRECRACKER_VERSION=${FIRECRACKER_VERSION:-latest}
FIRECRACKER_KERNEL_PREFIX=${FIRECRACKER_KERNEL_PREFIX:-}

log() {
  printf '\n==> %s\n' "$*"
}

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 2
  fi
}

need curl
need tar
need uname

mkdir -p "$ASSET_DIR"
ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
ARCH=$(uname -m)
RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases"

if [[ "$ARCH" != "x86_64" && "$ARCH" != "aarch64" ]]; then
  echo "unsupported Firecracker arch: $ARCH" >&2
  exit 2
fi

if [[ "$FIRECRACKER_VERSION" == "latest" ]]; then
  FIRECRACKER_VERSION=$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$RELEASE_URL/latest")")
fi

CI_VERSION=${FIRECRACKER_VERSION%.*}
TARBALL="firecracker-${FIRECRACKER_VERSION}-${ARCH}.tgz"

if [[ ! -x "$ASSET_DIR/firecracker" ]]; then
  log "downloading Firecracker $FIRECRACKER_VERSION for $ARCH"
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT
  curl -fL "$RELEASE_URL/download/${FIRECRACKER_VERSION}/${TARBALL}" -o "$tmpdir/$TARBALL"
  tar -xzf "$tmpdir/$TARBALL" -C "$tmpdir"
  install -m 0755 "$tmpdir/release-${FIRECRACKER_VERSION}-${ARCH}/firecracker-${FIRECRACKER_VERSION}-${ARCH}" "$ASSET_DIR/firecracker"
  rm -rf "$tmpdir"
  trap - EXIT
else
  echo "using cached $ASSET_DIR/firecracker"
fi

if [[ ! -f "$ASSET_DIR/vmlinux" ]]; then
  if [[ -z "$FIRECRACKER_KERNEL_PREFIX" ]]; then
    FIRECRACKER_KERNEL_PREFIX="firecracker-ci/${CI_VERSION}/${ARCH}/"
  fi
  log "looking for Firecracker CI kernel under $FIRECRACKER_KERNEL_PREFIX"
  listing=$(curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?prefix=${FIRECRACKER_KERNEL_PREFIX}")
  kernel_key=$(printf '%s\n' "$listing" | grep -oE "${FIRECRACKER_KERNEL_PREFIX}vmlinux-[0-9]+[.][0-9]+[.][0-9]+" | sort -V | tail -1 || true)
  if [[ -z "$kernel_key" ]]; then
    log "no kernel found for release prefix; falling back to latest dated CI kernel"
    prefixes=$(curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?delimiter=/&prefix=firecracker-ci/")
    dated_prefix=$(printf '%s\n' "$prefixes" | grep -oE "firecracker-ci/[0-9]{8}-[^/]+-0/" | sort | tail -1)
    FIRECRACKER_KERNEL_PREFIX="${dated_prefix}${ARCH}/"
    listing=$(curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?prefix=${FIRECRACKER_KERNEL_PREFIX}")
    kernel_key=$(printf '%s\n' "$listing" | grep -oE "${FIRECRACKER_KERNEL_PREFIX}vmlinux-[0-9]+[.][0-9]+[.][0-9]+" | sort -V | tail -1 || true)
  fi
  if [[ -z "$kernel_key" ]]; then
    echo "could not find Firecracker CI kernel for $ARCH" >&2
    exit 1
  fi
  echo "kernel key: $kernel_key"
  curl -fL "https://s3.amazonaws.com/spec.ccfc.min/${kernel_key}" -o "$ASSET_DIR/vmlinux"
else
  echo "using cached $ASSET_DIR/vmlinux"
fi

log "Firecracker assets"
"$ASSET_DIR/firecracker" --version
ls -lh "$ASSET_DIR/firecracker" "$ASSET_DIR/vmlinux"

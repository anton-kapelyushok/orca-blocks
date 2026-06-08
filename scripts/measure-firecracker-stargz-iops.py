#!/usr/bin/env python3
import json
import os
import random
import re
import shlex
import hashlib
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


ASSET_DIR = Path(os.environ.get("ASSET_DIR", "firecracker-assets"))
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/firecracker-stargz-iops"))
RESULTS_FILE = Path(os.environ.get("RESULTS_FILE", "docs/benchmarks/firecracker-stargz-iops-results.txt"))
BASE_IMAGE = os.environ.get("BASE_IMAGE", "alpine:3.22")
REGISTRY_PORT = int(os.environ.get("REGISTRY_PORT", "15002"))
REGISTRY_LOCAL = os.environ.get("REGISTRY_LOCAL", f"127.0.0.1:{REGISTRY_PORT}")
REGISTRY_NAME = os.environ.get("REGISTRY_NAME", "orca-firecracker-stargz-registry")
STARGZ_VERSION = os.environ.get("STARGZ_VERSION", "v0.18.2")
STARGZ_TOOLS_DIR = Path(os.environ.get("STARGZ_TOOLS_DIR", ".tmp/stargz-tools"))
SIZE_MB = int(os.environ.get("DISK_BENCH_SIZE_MB", "256"))
RANDOM_OPS = int(os.environ.get("DISK_BENCH_RANDOM_OPS", "65536"))
SEQ_BLOCK_BYTES = int(os.environ.get("DISK_BENCH_SEQ_BLOCK_BYTES", str(1024 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("DISK_BENCH_RANDOM_BLOCK_BYTES", "4096"))
IO_DEPTH = int(os.environ.get("DISK_BENCH_IO_DEPTH", "1"))
TIMEOUT_SECONDS = int(os.environ.get("TIMEOUT_SECONDS", "180"))
WAIT_SECONDS = int(os.environ.get("WAIT_SECONDS", "60"))
ROOTFS_SIZE_MB = int(os.environ.get("ROOTFS_SIZE_MB", "2048"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "3072"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "1"))
ALPINE_VERSION = os.environ.get("ALPINE_VERSION", "3.22.1")
ALPINE_ARCH = os.environ.get("ALPINE_ARCH", "x86_64")
GUEST_DNS = os.environ.get("GUEST_DNS", "1.1.1.1")
FORCE_ROOTFS = os.environ.get("FORCE_ROOTFS", "").lower() in {"1", "true", "yes", "on"}
FORCE_IMAGE = os.environ.get("FORCE_IMAGE", "").lower() in {"1", "true", "yes", "on"}
RESET_REGISTRY = os.environ.get("RESET_REGISTRY", "").lower() in {"1", "true", "yes", "on"}
KEEP_REGISTRY = os.environ.get("KEEP_REGISTRY", "true").lower() in {"1", "true", "yes", "on"}
PREBUILT_DISK_BENCH = os.environ.get("DISK_BENCH_BIN", "").strip()
IMAGE_TAG_SUFFIX = os.environ.get("IMAGE_TAG_SUFFIX", "").strip()
GUEST_INIT_VERSION = 3


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def run(cmd, check=True):
    print(f"$ {cmd}", flush=True)
    result = subprocess.run(cmd, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n", flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def phase_start(name):
    print(f"{now_utc()} phase={name} start", flush=True)
    return {"name": name, "started_at": now_utc(), "_start_ms": now_ms()}


def phase_end(phase, **extra):
    phase["finished_at"] = now_utc()
    phase["duration_ms"] = now_ms() - phase.pop("_start_ms")
    phase.update(extra)
    print(
        "{finished_at} phase={name} done duration_ms={duration_ms}".format(**phase),
        flush=True,
    )
    return phase


def ensure_stargz_tools():
    STARGZ_TOOLS_DIR.mkdir(parents=True, exist_ok=True)
    ctr_remote = STARGZ_TOOLS_DIR / "ctr-remote"
    snapshotter = STARGZ_TOOLS_DIR / "containerd-stargz-grpc"
    if ctr_remote.exists() and snapshotter.exists():
        return ctr_remote.resolve(), snapshotter.resolve()
    archive = STARGZ_TOOLS_DIR / f"stargz-snapshotter-{STARGZ_VERSION}-linux-amd64.tar.gz"
    url = f"https://github.com/containerd/stargz-snapshotter/releases/download/{STARGZ_VERSION}/stargz-snapshotter-{STARGZ_VERSION}-linux-amd64.tar.gz"
    if not archive.exists():
        run("curl -L --fail -o %s %s" % (q(archive), q(url)))
    run("tar -xzf %s -C %s ctr-remote containerd-stargz-grpc" % (q(archive), q(STARGZ_TOOLS_DIR)))
    run("chmod +x %s %s" % (q(ctr_remote), q(snapshotter)))
    return ctr_remote.resolve(), snapshotter.resolve()


def build_disk_bench(bin_path):
    bin_path.parent.mkdir(parents=True, exist_ok=True)
    if PREBUILT_DISK_BENCH:
        run("cp %s %s" % (q(PREBUILT_DISK_BENCH), q(bin_path)))
        run("chmod 0755 %s" % q(bin_path))
        return
    if bin_path.exists():
        return
    run(
        "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/disk-bench"
        % q(bin_path)
    )


def wait_for_registry():
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        out = run(
            "docker exec %s wget -qO- http://127.0.0.1:%d/v2/ >/dev/null 2>&1 && echo ok || true"
            % (q(REGISTRY_NAME), REGISTRY_PORT),
            check=False,
        ).strip()
        if out == "ok":
            return
        time.sleep(1)
    raise RuntimeError("registry did not become ready")


def ensure_registry():
    if RESET_REGISTRY:
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(REGISTRY_NAME), check=False)
    running = run("docker inspect -f '{{.State.Running}}' %s 2>/dev/null || true" % q(REGISTRY_NAME), check=False).strip()
    if running == "true":
        print("reusing registry container %s" % REGISTRY_NAME, flush=True)
    else:
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(REGISTRY_NAME), check=False)
        run("docker run -d --name %s --network host -e REGISTRY_HTTP_ADDR=0.0.0.0:%d registry:2" % (q(REGISTRY_NAME), REGISTRY_PORT))
    wait_for_registry()


def registry_has_image(ref):
    if "/" not in ref or ":" not in ref.rsplit("/", 1)[-1]:
        return False
    host_and_repo, tag = ref.rsplit(":", 1)
    if "/" not in host_and_repo:
        return False
    host, repo = host_and_repo.split("/", 1)
    url = "http://%s/v2/%s/manifests/%s" % (host, repo, tag)
    cmd = (
        "curl -fsS -H %s %s >/dev/null 2>&1 && echo yes || true"
        % (
            q("Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"),
            q(url),
        )
    )
    return run(cmd, check=False).strip() == "yes"


def image_tag_suffix():
    if IMAGE_TAG_SUFFIX:
        return IMAGE_TAG_SUFFIX
    payload = json.dumps(
        {
            "base_image": BASE_IMAGE,
            "size_mb": SIZE_MB,
            "seq_block_bytes": SEQ_BLOCK_BYTES,
            "random_block_bytes": RANDOM_BLOCK_BYTES,
            "random_ops": RANDOM_OPS,
            "io_depth": IO_DEPTH,
            "disk_bench": "cmd/disk-bench",
        },
        sort_keys=True,
    )
    digest = hashlib.sha256(payload.encode()).hexdigest()[:12]
    return "size%dm-rand%d-qd%d-%s" % (SIZE_MB, RANDOM_OPS, IO_DEPTH, digest)


class ContainerdNode:
    def __init__(self, name, work, ctr_remote):
        self.name = name
        self.work = work / name
        self.ctr_remote = ctr_remote
        self.root = self.work / "containerd-root"
        self.state = self.work / "containerd-state"
        self.sock = self.work / "containerd.sock"
        self.log = None
        self.proc = None

    def start(self):
        self.work.mkdir(parents=True, exist_ok=True)
        self.root.mkdir(parents=True, exist_ok=True)
        self.state.mkdir(parents=True, exist_ok=True)
        self.log = open(self.work / "containerd.log", "w", encoding="utf-8")
        self.proc = subprocess.Popen(
            ["containerd", "--address", str(self.sock), "--root", str(self.root), "--state", str(self.state), "--log-level", "warn"],
            stdout=self.log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        deadline = time.time() + WAIT_SECONDS
        while time.time() < deadline:
            if self.sock.exists():
                return
            if self.proc.poll() is not None:
                raise RuntimeError("%s containerd exited early:\n%s" % (self.name, (self.work / "containerd.log").read_text(errors="replace")))
            time.sleep(0.2)
        raise RuntimeError(f"{self.name} containerd socket did not appear")

    def ctr(self, args):
        return run("%s --address %s --namespace default %s" % (q(self.ctr_remote), q(self.sock), args))

    def stop(self):
        if self.proc and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        if self.log:
            self.log.close()


def build_benchmark_image(work, bin_path, tag):
    if not FORCE_IMAGE and registry_has_image(tag):
        print("reusing benchmark image %s" % tag, flush=True)
        return False
    context = work / "image-context"
    context.mkdir(parents=True, exist_ok=True)
    run("cp %s %s" % (q(bin_path), q(context / "disk-bench")))
    (context / "Dockerfile").write_text(
        "\n".join(
            [
                f"FROM {BASE_IMAGE}",
                "ARG SIZE_MB",
                "COPY disk-bench /disk-bench",
                "RUN chmod +x /disk-bench && mkdir -p /data && dd if=/dev/urandom of=/data/bench.dat bs=1M count=${SIZE_MB} status=none",
                'ENTRYPOINT ["/disk-bench"]',
                "",
            ]
        ),
        encoding="utf-8",
    )
    run("docker build --network host --build-arg SIZE_MB=%d -t %s %s" % (SIZE_MB, q(tag), q(context)))
    run("docker push %s" % q(tag))
    return True


def optimize_and_push(optimizer, source, target):
    if not FORCE_IMAGE and registry_has_image(target):
        print("reusing eStargz image %s" % target, flush=True)
        return False
    optimizer.ctr("images pull --plain-http %s" % q(source))
    optimizer.ctr("images optimize --oci %s %s" % (q(source), q(target)))
    optimizer.ctr("images push --plain-http %s" % q(target))
    return True


def drop_caches():
    run("sync", check=False)
    run("sh -c 'echo 3 > /proc/sys/vm/drop_caches'", check=False)


def extract_results(output):
    return [json.loads(m) for m in re.findall(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)]


def extract_step_results(output):
    return [json.loads(m) for m in re.findall(r"WORKFLOW_STEP_RESULT=(\{[^\r\n]*\})", output)]


def docker_first_output(image):
    drop_caches()
    start = now_ms()
    out = run(
        "docker run --rm --network host --entrypoint /bin/sh %s -lc %s"
        % (q(image), q("echo ORCA_FIRST_USER_OUTPUT"))
    )
    if "ORCA_FIRST_USER_OUTPUT" not in out:
        raise RuntimeError("docker first-output marker missing:\n%s" % out)
    return {
        "runtime": "docker",
        "operation": "first_user_output",
        "wall_ms": now_ms() - start,
        "bench_duration_ms": "",
        "mb_per_sec": "",
        "iops": "",
        "notes": "dockerd already ready",
    }


def docker_bench(image, mode):
    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    args = (
        "-kind docker-%s -mode %s -path /data/bench.dat -size-bytes %d "
        "-block-bytes %d -random-ops %d -io-depth %d"
        % (mode, mode, SIZE_MB * 1024 * 1024, block_bytes, RANDOM_OPS, IO_DEPTH)
    )
    drop_caches()
    start = now_ms()
    out = run("docker run --rm --network host %s %s" % (q(image), args))
    results = extract_results(out)
    if len(results) != 1:
        raise RuntimeError("expected one docker benchmark result, got %d:\n%s" % (len(results), out))
    results[0]["wall_ms"] = now_ms() - start
    results[0]["runtime"] = "docker"
    results[0]["operation"] = "%s_read" % mode
    results[0]["bench_duration_ms"] = results[0].get("duration_ms")
    results[0]["notes"] = "dockerd already ready"
    return results[0]


def alpine_url():
    major_minor = ".".join(ALPINE_VERSION.split(".")[:2])
    return "https://dl-cdn.alpinelinux.org/alpine/v%s/releases/%s/alpine-minirootfs-%s-%s.tar.gz" % (
        major_minor,
        ALPINE_ARCH,
        ALPINE_VERSION,
        ALPINE_ARCH,
    )


def ensure_guest_rootfs(work, ctr_remote, snapshotter):
    rootfs_dir = WORK_PARENT / "guest-rootfs-cache"
    rootfs_dir.mkdir(parents=True, exist_ok=True)
    rootfs = rootfs_dir / f"alpine-containerd-stargz-{ALPINE_VERSION}-{ROOTFS_SIZE_MB}m.ext4"
    meta = rootfs.with_suffix(".json")
    desired = {
        "alpine_version": ALPINE_VERSION,
        "rootfs_size_mb": ROOTFS_SIZE_MB,
        "stargz_version": STARGZ_VERSION,
        "guest_init_version": GUEST_INIT_VERSION,
    }
    if FORCE_ROOTFS:
        rootfs.unlink(missing_ok=True)
        meta.unlink(missing_ok=True)
    if rootfs.exists() and meta.exists():
        try:
            if json.loads(meta.read_text(encoding="utf-8")) == desired:
                print("reusing guest rootfs %s" % rootfs, flush=True)
                return rootfs.resolve()
        except Exception:
            pass
    phase = phase_start("build_guest_rootfs")
    tarball = rootfs_dir / ("alpine-minirootfs-%s-%s.tar.gz" % (ALPINE_VERSION, ALPINE_ARCH))
    if not tarball.exists():
        run("curl -fL -o %s %s" % (q(tarball), q(alpine_url())))
    rootfs.unlink(missing_ok=True)
    run("truncate -s %dM %s" % (ROOTFS_SIZE_MB, q(rootfs)))
    run("mkfs.ext4 -F %s >/dev/null" % q(rootfs))
    mount_dir = Path(run("mktemp -d %s" % q(str(work / "mnt.XXXXXX"))).strip())
    mounted = False
    try:
        run("mount -o loop %s %s" % (q(rootfs), q(mount_dir)))
        mounted = True
        run("tar -xzf %s -C %s" % (q(tarball), q(mount_dir)))
        run("cp /etc/resolv.conf %s" % q(mount_dir / "etc/resolv.conf"))
        run("chroot %s /sbin/apk add --no-cache ca-certificates containerd runc iproute2 iptables e2fsprogs" % q(mount_dir))
        run("install -m 0755 %s %s" % (q(ctr_remote), q(mount_dir / "usr/local/bin/ctr-remote")))
        run("install -m 0755 %s %s" % (q(snapshotter), q(mount_dir / "usr/local/bin/containerd-stargz-grpc")))
        run("mkdir -p %s" % q(mount_dir / "etc/containerd"))
        init_path = mount_dir / "init"
        init_path.write_text(guest_init_script(), encoding="utf-8")
        run("chmod 0755 %s" % q(init_path))
        run("mkdir -p %s" % q(mount_dir / "dev"))
        run("mkdir -p %s" % q(mount_dir / "proc"))
        run("mkdir -p %s" % q(mount_dir / "sys"))
        run("mkdir -p %s" % q(mount_dir / "run"))
        run("sync")
    finally:
        if mounted:
            run("umount %s" % q(mount_dir), check=False)
        run("rmdir %s >/dev/null 2>&1 || true" % q(mount_dir), check=False)
    meta.write_text(json.dumps(desired, indent=2, sort_keys=True), encoding="utf-8")
    phase_end(phase, rootfs=str(rootfs))
    return rootfs.resolve()


def guest_init_script():
    return r'''#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

log() {
  echo "guest-init: $*" > /dev/console
}

cmdline_value() {
  key="$1"
  for arg in $(cat /proc/cmdline); do
    case "$arg" in
      "$key="*) echo "${arg#*=}"; return 0 ;;
    esac
  done
  return 1
}

mount -t proc proc /proc || true
mount -t sysfs sysfs /sys || true
mount -t devtmpfs devtmpfs /dev || true
mkdir -p /run /tmp /sys/fs/cgroup /var/lib/containerd /run/containerd /var/lib/containerd-stargz-grpc /run/containerd-stargz-grpc
mount -t tmpfs tmpfs /run || true
mkdir -p /run/containerd /run/containerd-stargz-grpc
mount -t tmpfs tmpfs /tmp || true
mount -t cgroup2 none /sys/fs/cgroup || true

GUEST_CIDR="$(cmdline_value guest_cidr)"
GATEWAY="$(cmdline_value gateway)"
DNS="$(cmdline_value dns || echo 1.1.1.1)"
IMAGE="$(cmdline_value image)"
MODE="$(cmdline_value mode)"
SIZE_BYTES="$(cmdline_value size_bytes)"
SEQ_BLOCK_BYTES="$(cmdline_value seq_block_bytes)"
RANDOM_BLOCK_BYTES="$(cmdline_value random_block_bytes)"
RANDOM_OPS="$(cmdline_value random_ops)"
IO_DEPTH="$(cmdline_value io_depth)"

ip link set lo up || true
ip addr add "$GUEST_CIDR" dev eth0
ip link set eth0 up
ip route add default via "$GATEWAY"
printf 'nameserver %s\n' "$DNS" > /etc/resolv.conf

cat >/etc/containerd/config.toml <<EOF
version = 2
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
EOF

log "starting stargz snapshotter"
containerd-stargz-grpc \
  -address /run/containerd-stargz-grpc/containerd-stargz-grpc.sock \
  -root /var/lib/containerd-stargz-grpc \
  -log-level warn >/tmp/stargz.log 2>&1 &
STARGZ_PID="$!"

i=0
while [ "$i" -lt 300 ]; do
  [ -S /run/containerd-stargz-grpc/containerd-stargz-grpc.sock ] && break
  if ! kill -0 "$STARGZ_PID" 2>/dev/null; then
    log "stargz exited early"
    cat /tmp/stargz.log >/dev/console 2>&1 || true
    poweroff -f
  fi
  i=$((i + 1))
  sleep 0.1
done

log "starting containerd"
containerd --address /run/containerd/containerd.sock --config /etc/containerd/config.toml --log-level warn >/tmp/containerd.log 2>&1 &
CONTAINERD_PID="$!"
i=0
while [ "$i" -lt 300 ]; do
  [ -S /run/containerd/containerd.sock ] && break
  if ! kill -0 "$CONTAINERD_PID" 2>/dev/null; then
    log "containerd exited early"
    cat /tmp/containerd.log >/dev/console 2>&1 || true
    poweroff -f
  fi
  i=$((i + 1))
  sleep 0.1
done

uptime_ms() {
  awk '{ printf "%d", $1 * 1000 }' /proc/uptime 2>/dev/null || printf "0"
}

step_result() {
  step="$1"
  wall_ms="$2"
  extra="${3:-}"
  printf 'WORKFLOW_STEP_RESULT={"step":"%s","wall_ms":%s%s}\n' "$step" "$wall_ms" "$extra" >/dev/console
}

log "containerd ready"

run_bench() {
  mode="$1"
  block="$2"
  name="bench-$mode-$$"
  log "run mode=$mode"
  ctr-remote --address /run/containerd/containerd.sock --namespace default run --rm --snapshotter stargz --net-host \
    "$IMAGE" "$name" /disk-bench \
    -kind "firecracker-stargz-$mode" \
    -mode "$mode" \
    -path /data/bench.dat \
    -size-bytes "$SIZE_BYTES" \
    -block-bytes "$block" \
    -random-ops "$RANDOM_OPS" \
    -io-depth "$IO_DEPTH" >/dev/console 2>&1
}

run_first_output() {
    START_MS="$(uptime_ms)"
    log "rpull image=$IMAGE"
    ctr-remote --address /run/containerd/containerd.sock --namespace default images rpull --plain-http --snapshotter stargz "$IMAGE" >/dev/console 2>&1
    RPULL_DONE_MS="$(uptime_ms)"
    FIRST_NAME="first-output-$$"
    ctr-remote --address /run/containerd/containerd.sock --namespace default run --rm --snapshotter stargz --net-host \
      "$IMAGE" "$FIRST_NAME" /bin/sh -lc 'echo ORCA_FIRST_USER_OUTPUT' >/dev/console 2>&1
    FIRST_DONE_MS="$(uptime_ms)"
    step_result "first_user_output" "$((FIRST_DONE_MS - START_MS))" ",\"rpull_ms\":$((RPULL_DONE_MS - START_MS))"
}

run_timed_bench() {
  mode="$1"
  block="$2"
  step="$3"
  START_MS="$(uptime_ms)"
  log "rpull image=$IMAGE"
  ctr-remote --address /run/containerd/containerd.sock --namespace default images rpull --plain-http --snapshotter stargz "$IMAGE" >/dev/console 2>&1
  RPULL_DONE_MS="$(uptime_ms)"
  run_bench "$mode" "$block"
  DONE_MS="$(uptime_ms)"
  step_result "$step" "$((DONE_MS - START_MS))" ",\"rpull_ms\":$((RPULL_DONE_MS - START_MS))"
}

case "$MODE" in
  first_user_output) run_first_output ;;
  sequential) run_timed_bench sequential "$SEQ_BLOCK_BYTES" sequential_read ;;
  random) run_timed_bench random "$RANDOM_BLOCK_BYTES" random_read ;;
  *)
    log "unknown mode=$MODE"
    poweroff -f
    ;;
esac
log "done"
poweroff -f
'''


def setup_tap():
    third = random.randint(180, 249)
    tap = "tapsgz%d" % random.randint(1000, 9999)
    host_ip = "172.30.%d.1" % third
    guest_ip = "172.30.%d.2" % third
    host_cidr = host_ip + "/30"
    guest_cidr = guest_ip + "/30"
    guest_mac = "06:00:ac:1e:%02x:%02x" % (third, random.randint(2, 254))
    run("ip link del %s >/dev/null 2>&1 || true" % q(tap), check=False)
    run("ip tuntap add dev %s mode tap" % q(tap))
    run("ip addr add %s dev %s" % (q(host_cidr), q(tap)))
    run("ip link set dev %s up" % q(tap))
    run("sysctl -w net.ipv4.ip_forward=1 >/dev/null")
    run("iptables -t nat -C POSTROUTING -s %s -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s %s -j MASQUERADE" % (q(guest_cidr), q(guest_cidr)))
    run("iptables -C FORWARD -i %s -j ACCEPT 2>/dev/null || iptables -A FORWARD -i %s -j ACCEPT" % (q(tap), q(tap)))
    run("iptables -C FORWARD -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || iptables -A FORWARD -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" % (q(tap), q(tap)))
    return {"tap": tap, "host_ip": host_ip, "guest_cidr": guest_cidr, "guest_mac": guest_mac}


def cleanup_tap(net):
    run("iptables -t nat -D POSTROUTING -s %s -j MASQUERADE >/dev/null 2>&1 || true" % q(net["guest_cidr"]), check=False)
    run("iptables -D FORWARD -i %s -j ACCEPT >/dev/null 2>&1 || true" % q(net["tap"]), check=False)
    run("iptables -D FORWARD -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT >/dev/null 2>&1 || true" % q(net["tap"]), check=False)
    run("ip link del %s >/dev/null 2>&1 || true" % q(net["tap"]), check=False)


def firecracker_bench(rootfs_cache, image_path, work, mode):
    firecracker = ASSET_DIR / "firecracker"
    kernel = ASSET_DIR / "vmlinux"
    if not firecracker.exists() or not kernel.exists():
        raise RuntimeError("missing Firecracker assets under %s" % ASSET_DIR)
    if not Path("/dev/kvm").exists():
        raise RuntimeError("/dev/kvm is missing")
    run_dir = Path(run("mktemp -d %s" % q(str(work / "firecracker.XXXXXX"))).strip())
    rootfs = run_dir / "rootfs.ext4"
    run("cp --sparse=always --reflink=auto %s %s" % (q(rootfs_cache), q(rootfs)))
    serial_log = run_dir / "serial.log"
    firecracker_log = run_dir / "firecracker.log"
    socket_path = run_dir / "firecracker.sock"
    config_path = run_dir / "firecracker.json"
    net = setup_tap()
    proc = None
    try:
        guest_image = f"{net['host_ip']}:{REGISTRY_PORT}/{image_path}"
        boot_args = (
            "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init "
            "guest_cidr=%s gateway=%s dns=%s image=%s mode=%s size_bytes=%d "
            "seq_block_bytes=%d random_block_bytes=%d random_ops=%d io_depth=%d"
            % (
                net["guest_cidr"],
                net["host_ip"],
                GUEST_DNS,
                guest_image,
                mode,
                SIZE_MB * 1024 * 1024,
                SEQ_BLOCK_BYTES,
                RANDOM_BLOCK_BYTES,
                RANDOM_OPS,
                IO_DEPTH,
            )
        )
        config = {
            "boot-source": {"kernel_image_path": str(kernel.resolve()), "boot_args": boot_args},
            "drives": [
                {
                    "drive_id": "rootfs",
                    "path_on_host": str(rootfs.resolve()),
                    "is_root_device": True,
                    "is_read_only": False,
                }
            ],
            "network-interfaces": [
                {"iface_id": "eth0", "guest_mac": net["guest_mac"], "host_dev_name": net["tap"]}
            ],
            "machine-config": {"vcpu_count": VCPU_COUNT, "mem_size_mib": MEM_SIZE_MIB, "track_dirty_pages": False},
            "logger": {"log_path": str(firecracker_log), "level": "Info", "show_level": True, "show_log_origin": True},
        }
        config_path.write_text(json.dumps(config, indent=2), encoding="utf-8")
        drop_caches()
        phase = phase_start("firecracker_stargz_%s" % mode)
        serial = serial_log.open("wb")
        fc_err = firecracker_log.open("ab")
        proc = subprocess.Popen(
            [str(firecracker.resolve()), "--api-sock", str(socket_path), "--config-file", str(config_path)],
            stdout=serial,
            stderr=fc_err,
        )
        serial.close()
        fc_err.close()
        deadline = time.time() + TIMEOUT_SECONDS
        output = ""
        while time.time() < deadline:
            if serial_log.exists():
                output = serial_log.read_text(errors="replace")
            results = extract_results(output)
            step_results = extract_step_results(output)
            expected_step = {
                "first_user_output": "first_user_output",
                "sequential": "sequential_read",
                "random": "random_read",
            }.get(mode, mode)
            has_step = any(item.get("step") == expected_step for item in step_results)
            complete = has_step if mode == "first_user_output" else (has_step and len(results) >= 1)
            if complete:
                finished_phase = phase_end(phase, serial_log=str(serial_log), work_dir=str(run_dir))
                for row in results:
                    row["runtime"] = "firecracker-stargz"
                    row["serial_log"] = str(serial_log)
                    row["work_dir"] = str(run_dir)
                    row["image"] = guest_image
                if results:
                    return results[0], finished_phase
                return {
                    "runtime": "firecracker-stargz",
                    "serial_log": str(serial_log),
                    "work_dir": str(run_dir),
                    "image": guest_image,
                }, finished_phase
            if proc.poll() is not None and not results:
                raise RuntimeError("Firecracker exited before benchmark result:\n%s" % output[-5000:])
            time.sleep(0.1)
        raise TimeoutError("timed out waiting for Firecracker stargz benchmark; serial_log=%s" % serial_log)
    finally:
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        cleanup_tap(net)


def firecracker_operation(rootfs_cache, image_path, work, operation):
    mode = {
        "first_user_output": "first_user_output",
        "sequential_read": "sequential",
        "random_read": "random",
    }[operation]
    row, phase = firecracker_bench(rootfs_cache, image_path, work, mode)
    serial_log = Path(row["serial_log"])
    output = serial_log.read_text(errors="replace")
    step_results = {item["step"]: item for item in extract_step_results(output)}
    bench_results = {item["mode"]: item for item in extract_results(output)}
    if operation == "first_user_output":
        first = step_results.get("first_user_output")
        if not first:
            raise RuntimeError("missing firecracker first_user_output step in %s" % serial_log)
        return {
            "runtime": "firecracker-stargz",
            "operation": "first_user_output",
            "wall_ms": first["wall_ms"],
            "bench_duration_ms": "",
            "mb_per_sec": "",
            "iops": "",
            "notes": "containerd ready; includes stargz rpull (%sms)" % first.get("rpull_ms", ""),
            "serial_log": str(serial_log),
            "work_dir": row.get("work_dir", ""),
        }, phase
    bench_mode = "sequential" if operation == "sequential_read" else "random"
    step = step_results.get(operation)
    bench = bench_results.get(bench_mode)
    if not step or not bench:
        raise RuntimeError("missing firecracker %s workflow data in %s" % (operation, serial_log))
    return {
        "runtime": "firecracker-stargz",
        "operation": operation,
        "wall_ms": step["wall_ms"],
        "bench_duration_ms": bench.get("duration_ms", ""),
        "mb_per_sec": bench.get("mb_per_sec", ""),
        "iops": bench.get("iops", ""),
        "notes": "containerd ready; includes stargz rpull (%sms)" % step.get("rpull_ms", ""),
        "serial_log": str(serial_log),
        "work_dir": row.get("work_dir", ""),
    }, phase


def format_table(rows):
    lines = [
        "Runtime              Operation           Wall        Bench       Throughput      IOPS  Notes",
        "-------------------  ------------------  --------  -----------  ----------  --------  --------------------------------------------",
    ]
    for row in rows:
        wall = "" if row.get("wall_ms") == "" else "%s ms" % row.get("wall_ms")
        bench = "" if row.get("bench_duration_ms") == "" else "%s ms" % row.get("bench_duration_ms")
        throughput = "" if row.get("mb_per_sec") == "" else "%s MiB/s" % row.get("mb_per_sec")
        iops = "" if row.get("iops") == "" else str(row.get("iops"))
        lines.append("%-19s  %-18s  %8s  %11s  %10s  %8s  %s" % (
            row.get("runtime", ""),
            row.get("operation", ""),
            wall,
            bench,
            throughput,
            iops,
            row.get("notes", ""),
        ))
    return "\n".join(lines)


def append_results(started, rows, phases, summary):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    with RESULTS_FILE.open("a", encoding="utf-8") as f:
        f.write("\n")
        f.write("firecracker-stargz-iops run %s\n" % started)
        f.write("base_image=%s size_mb=%d random_ops=%d io_depth=%d mem_mib=%d vcpu=%d\n" % (BASE_IMAGE, SIZE_MB, RANDOM_OPS, IO_DEPTH, MEM_SIZE_MIB, VCPU_COUNT))
        f.write("\n")
        f.write(format_table(rows))
        f.write("\n\nphases:\n")
        for phase in phases:
            f.write("{name:28s} {duration_ms:7d}ms\n".format(**phase))
        f.write("\nSUMMARY_JSON=%s\n" % json.dumps(summary, sort_keys=True))


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root; this script creates tap devices, mounts rootfs images, and starts containerd")
    started = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    work = (WORK_PARENT / started).resolve()
    work.mkdir(parents=True, exist_ok=True)
    phases = []
    ctr_remote, snapshotter = ensure_stargz_tools()
    bin_path = work / "disk-bench"
    build_disk_bench(bin_path)
    tag_suffix = image_tag_suffix()
    normal_image = f"{REGISTRY_LOCAL}/orca-disk-bench-normal:{tag_suffix}"
    esgz_local = f"{REGISTRY_LOCAL}/orca-disk-bench-esgz:{tag_suffix}"
    optimizer = ContainerdNode("optimizer", work, ctr_remote)
    rows = []

    try:
        ensure_registry()
        optimizer.start()

        phase = phase_start("build_and_push_benchmark_image")
        built = build_benchmark_image(work, bin_path, normal_image)
        phases.append(phase_end(phase, image=normal_image, reused=not built))

        phase = phase_start("optimize_esgz_image")
        optimized = optimize_and_push(optimizer, normal_image, esgz_local)
        phases.append(phase_end(phase, image=esgz_local, reused=not optimized))

        phase = phase_start("docker_first_user_output")
        row = docker_first_output(normal_image)
        rows.append(row)
        phases.append(phase_end(phase, wall_ms=row["wall_ms"]))

        for mode in ["sequential", "random"]:
            phase = phase_start("docker_%s" % mode)
            row = docker_bench(normal_image, mode)
            rows.append(row)
            phases.append(phase_end(phase, mb_per_sec=row["mb_per_sec"], iops=row["iops"]))

        rootfs = ensure_guest_rootfs(work, ctr_remote, snapshotter)
        image_path = f"orca-disk-bench-esgz:{tag_suffix}"
        for operation in ["first_user_output", "sequential_read", "random_read"]:
            row, phase = firecracker_operation(rootfs, image_path, work, operation)
            rows.append(row)
            phases.append(phase)

        summary = {
            "started": started,
            "work_dir": str(work),
            "normal_image": normal_image,
            "esgz_local": esgz_local,
            "tag_suffix": tag_suffix,
            "force_image": FORCE_IMAGE,
            "guest_image_path": image_path,
            "rows": rows,
            "phases": phases,
        }
        append_results(started, rows, phases, summary)
        print("\n[firecracker-stargz-iops]")
        print(format_table(rows))
        print("\nresults_file=%s" % RESULTS_FILE)
        print("SUMMARY_JSON=%s" % json.dumps(summary, sort_keys=True))
    finally:
        optimizer.stop()
        if not KEEP_REGISTRY:
            run("docker rm -f %s >/dev/null 2>&1 || true" % q(REGISTRY_NAME), check=False)


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

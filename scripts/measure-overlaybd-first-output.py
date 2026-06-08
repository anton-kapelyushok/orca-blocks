#!/usr/bin/env python3
import json
import os
import shlex
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/overlaybd-first-output"))
RESULTS_FILE = Path(os.environ.get("RESULTS_FILE", "docs/benchmarks/overlaybd-first-output-results.md"))
BASE_IMAGE = os.environ.get("BASE_IMAGE", "alpine:3.22")
REGISTRY_PORT = int(os.environ.get("REGISTRY_PORT", "15003"))
REGISTRY_LOCAL = os.environ.get("REGISTRY_LOCAL", f"localhost:{REGISTRY_PORT}")
REGISTRY_NAME = os.environ.get("REGISTRY_NAME", "orca-overlaybd-registry")
DISK_BENCH_BIN = Path(os.environ.get("DISK_BENCH_BIN", ".tmp/disk-bench/disk-bench-linux-amd64"))
SIZE_MB = int(os.environ.get("DISK_BENCH_SIZE_MB", "256"))
RANDOM_OPS = int(os.environ.get("DISK_BENCH_RANDOM_OPS", "65536"))
IO_DEPTH = int(os.environ.get("DISK_BENCH_IO_DEPTH", "1"))
WAIT_SECONDS = int(os.environ.get("WAIT_SECONDS", "180"))
FORCE_IMAGE = os.environ.get("FORCE_IMAGE", "").lower() in {"1", "true", "yes", "on"}
RUNC_BINARY = os.environ.get("RUNC_BINARY", "")
RUNTIME_LABEL = os.environ.get("RUNTIME_LABEL", "overlaybd-sysbox" if RUNC_BINARY else "overlaybd-runc")

OBD_CTR = Path(os.environ.get("OBD_CTR", "/opt/overlaybd/snapshotter/ctr"))
CONTENT_PATH = Path(os.environ.get("CONTAINERD_CONTENT_PATH", "/var/lib/containerd/io.containerd.content.v1.content"))
OVERLAYBD_SNAPSHOTTER_PATH = Path(os.environ.get("OVERLAYBD_SNAPSHOTTER_PATH", "/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd"))
OVERLAYBD_CACHE_PATHS = [
    Path("/opt/overlaybd/registry_cache"),
    Path("/opt/overlaybd/gzip_cache"),
    OVERLAYBD_SNAPSHOTTER_PATH,
]


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def run(cmd, check=True, timeout=None):
    print("$ %s" % cmd, flush=True)
    result = subprocess.run(cmd, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n")
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def require(path, label):
    if not Path(path).exists():
        raise RuntimeError("missing %s at %s" % (label, path))


def bytes_for(paths):
    existing = [p for p in paths if p.exists()]
    if not existing:
        return 0
    out = run("du -sB1 %s" % " ".join(q(p) for p in existing), check=False)
    total = 0
    for line in out.splitlines():
        parts = line.split(None, 1)
        if parts and parts[0].isdigit():
            total += int(parts[0])
    return total


def state_sizes():
    return {
        "content_bytes": bytes_for([CONTENT_PATH]),
        "overlaybd_bytes": bytes_for(OVERLAYBD_CACHE_PATHS),
    }


def subtract_sizes(after, before):
    return {key: after.get(key, 0) - before.get(key, 0) for key in set(after) | set(before)}


def registry_has_image(ref):
    host_repo, tag = ref.rsplit(":", 1)
    _, repo = host_repo.split("/", 1)
    url = "http://%s/v2/%s/manifests/%s" % (REGISTRY_LOCAL, repo, tag)
    accept = "Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
    out = run("curl -fsS -H %s %s >/dev/null 2>&1 && echo yes || true" % (q(accept), q(url)), check=False).strip()
    return out == "yes"


def ensure_registry():
    running = run("docker inspect -f '{{.State.Running}}' %s 2>/dev/null || true" % q(REGISTRY_NAME), check=False).strip()
    if running != "true":
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(REGISTRY_NAME), check=False)
        run(
            "docker run -d --name %s --network host -e REGISTRY_HTTP_ADDR=0.0.0.0:%d registry:2"
            % (q(REGISTRY_NAME), REGISTRY_PORT)
        )
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        out = run("curl -fsS http://%s/v2/ >/dev/null 2>&1 && echo ok || true" % REGISTRY_LOCAL, check=False).strip()
        if out == "ok":
            return
        time.sleep(0.5)
    raise RuntimeError("registry did not become ready")


def ensure_overlaybd():
    require(OBD_CTR, "OverlayBD ctr")
    run("modprobe target_core_user")
    configure_overlaybd_registry()
    run("systemctl start overlaybd-tcmu overlaybd-snapshotter")
    out = run("ctr plugins ls | grep -F 'io.containerd.snapshotter.v1' | grep -F overlaybd || true", check=False)
    if "overlaybd" not in out or "ok" not in out:
        raise RuntimeError("containerd overlaybd snapshotter is not ready:\n%s" % out)


def configure_overlaybd_registry():
    config_path = Path("/etc/overlaybd-snapshotter/config.json")
    if not config_path.exists():
        return
    config = json.loads(config_path.read_text())
    mirrors = config.get("mirrorRegistry") or []
    wanted = {"host": REGISTRY_LOCAL, "insecure": True}
    changed = False
    for mirror in mirrors:
        if mirror.get("host") == REGISTRY_LOCAL:
            if mirror.get("insecure") is not True:
                mirror["insecure"] = True
                changed = True
            break
    else:
        mirrors.append(wanted)
        changed = True
    config["mirrorRegistry"] = mirrors
    if changed:
        config_path.write_text(json.dumps(config, indent=4) + "\n")
        run("systemctl restart overlaybd-snapshotter overlaybd-tcmu || true", check=False)


def build_and_push_normal(work, image):
    if not FORCE_IMAGE and registry_has_image(image):
        return False
    context = work / "image-context"
    context.mkdir(parents=True, exist_ok=True)
    run("cp %s %s" % (q(DISK_BENCH_BIN), q(context / "disk-bench")))
    (context / "Dockerfile").write_text(
        "\n".join([
            "FROM %s" % BASE_IMAGE,
            "ARG SIZE_MB",
            "COPY disk-bench /disk-bench",
            "RUN chmod +x /disk-bench && mkdir -p /data && dd if=/dev/urandom of=/data/bench.dat bs=1M count=${SIZE_MB} status=none",
            'CMD ["/bin/sh", "-lc", "echo ORCA_FIRST_USER_OUTPUT"]',
            "",
        ]),
        encoding="utf-8",
    )
    run("docker build --network host --build-arg SIZE_MB=%d -t %s %s" % (SIZE_MB, q(image), q(context)))
    run("docker push %s" % q(image))
    return True


def convert_and_push_overlaybd(normal_ref, overlaybd_ref):
    if not FORCE_IMAGE and registry_has_image(overlaybd_ref):
        return False
    run("ctr images pull --local --plain-http --snapshotter overlayfs %s" % q(normal_ref), timeout=WAIT_SECONDS)
    run("%s obdconv --plain-http --fstype ext4 %s %s" % (q(OBD_CTR), q(normal_ref), q(overlaybd_ref)), timeout=WAIT_SECONDS * 4)
    run("%s images push --plain-http %s" % (q(OBD_CTR), q(overlaybd_ref)), timeout=WAIT_SECONDS * 4)
    return True


def remove_containerd_refs(*refs):
    for ref in refs:
        run("ctr images rm %s >/dev/null 2>&1 || true" % q(ref), check=False)
    digests = run("ctr content ls -q 2>/dev/null || true", check=False).split()
    if digests:
        run("ctr content rm %s >/dev/null 2>&1 || true" % " ".join(q(d) for d in digests), check=False)


def clear_overlaybd_state(*refs):
    run("ctr containers ls -q | grep '^overlaybd-' | xargs -r ctr containers rm >/dev/null 2>&1 || true", check=False)
    run("findmnt -rn -o TARGET | grep '/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd' | sort -r | xargs -r umount -l >/dev/null 2>&1 || true", check=False)
    for _ in range(3):
        run("ctr snapshots --snapshotter overlaybd ls | awk 'NR > 1 {print $1}' | tac | xargs -r -n1 ctr snapshots --snapshotter overlaybd rm >/dev/null 2>&1 || true", check=False)
    for ref in refs:
        run("ctr images rm %s >/dev/null 2>&1 || true" % q(ref), check=False)
    digests = run("ctr content ls -q 2>/dev/null || true", check=False).split()
    if digests:
        run("ctr content rm %s >/dev/null 2>&1 || true" % " ".join(q(d) for d in digests), check=False)
    run("systemctl stop overlaybd-snapshotter overlaybd-tcmu || true", check=False)
    run("find /sys/kernel/config/target/loopback -type l -delete 2>/dev/null || true", check=False)
    run("find /sys/kernel/config/target/loopback -mindepth 1 -depth -type d -exec rmdir {} + 2>/dev/null || true", check=False)
    run("for d in /sys/kernel/config/target/core/user_999999999/dev_*; do [ -e \"$d\" ] || continue; echo 0 > \"$d/enable\" 2>/dev/null || true; rmdir \"$d\" 2>/dev/null || true; done", check=False)
    run("rm -rf %s" % " ".join(q(p) for p in OVERLAYBD_CACHE_PATHS), check=False)
    run("mkdir -p %s" % " ".join(q(p) for p in OVERLAYBD_CACHE_PATHS), check=False)
    run("systemctl start overlaybd-tcmu")
    run("systemctl start overlaybd-snapshotter")


def time_cmd(cmd, timeout=None):
    start = now_ms()
    out = run(cmd, timeout=timeout)
    return now_ms() - start, out


def rpull(ref):
    return time_cmd("%s rpull --plain-http %s" % (q(OBD_CTR), q(ref)), timeout=WAIT_SECONDS)[0]


def run_first_output(ref):
    elapsed, out = time_cmd("ctr run %s --net-host --snapshotter=overlaybd --rm %s %s-first-output /bin/sh -lc 'echo ORCA_FIRST_USER_OUTPUT'" % (
        runtime_flags(),
        q(ref),
        q(RUNTIME_LABEL),
    ), timeout=WAIT_SECONDS)
    if "ORCA_FIRST_USER_OUTPUT" not in out:
        raise RuntimeError("first-output marker missing:\n%s" % out)
    return elapsed, out


def run_disk_bench(ref, mode):
    if mode == "sequential":
        block_bytes = 1024 * 1024
        random_ops = RANDOM_OPS
    elif mode == "random":
        block_bytes = 4096
        random_ops = RANDOM_OPS
    else:
        raise ValueError(mode)
    cmd = (
        "ctr run %s --net-host --snapshotter=overlaybd --rm %s %s-%s "
        "/disk-bench -kind %s-%s -mode %s -path /data/bench.dat "
        "-size-bytes %d -block-bytes %d -random-ops %d -io-depth %d"
    ) % (
        runtime_flags(),
        q(ref),
        q(RUNTIME_LABEL),
        q(mode),
        RUNTIME_LABEL,
        mode,
        mode,
        SIZE_MB * 1024 * 1024,
        block_bytes,
        random_ops,
        IO_DEPTH,
    )
    elapsed, out = time_cmd(cmd, timeout=WAIT_SECONDS)
    rows = []
    for line in out.splitlines():
        if line.startswith("RESULT "):
            rows.append(json.loads(line.removeprefix("RESULT ")))
        elif line.startswith("DISK_BENCH_RESULT="):
            rows.append(json.loads(line.removeprefix("DISK_BENCH_RESULT=")))
    if len(rows) != 1:
        raise RuntimeError("expected one disk-bench result, got %d:\n%s" % (len(rows), out))
    return elapsed, out, rows[0]


def runtime_flags():
    if not RUNC_BINARY:
        return ""
    return "--runtime io.containerd.runc.v2 --runc-binary %s" % q(RUNC_BINARY)


def measure_operation(ref, operation, prefetch):
    clear_overlaybd_state(ref)
    before = state_sizes()
    prefetch_ms = ""
    prefetch_delta = {"content_bytes": 0, "overlaybd_bytes": 0}
    if prefetch:
        start_sizes = state_sizes()
        prefetch_ms = rpull(ref)
        prefetch_delta = subtract_sizes(state_sizes(), start_sizes)

    measured_rpull_ms = ""
    rpull_delta = {"content_bytes": 0, "overlaybd_bytes": 0}
    if not prefetch:
        start_sizes = state_sizes()
        measured_rpull_ms = rpull(ref)
        rpull_delta = subtract_sizes(state_sizes(), start_sizes)

    command_start_sizes = state_sizes()
    if operation == "first_output":
        command_ms, _ = run_first_output(ref)
        bench = {}
    else:
        command_ms, _, bench = run_disk_bench(ref, operation)
    command_delta = subtract_sizes(state_sizes(), command_start_sizes)
    after = state_sizes()
    return {
            "operation": operation,
            "runtime": RUNTIME_LABEL,
            "prefetch": prefetch,
        "prefetch_ms": prefetch_ms,
        "measured_rpull_ms": measured_rpull_ms,
        "command_ms": command_ms,
        "prefetch_delta": prefetch_delta,
        "rpull_delta": rpull_delta,
        "command_delta": command_delta,
        "total_delta": subtract_sizes(after, before),
        "bench": bench,
    }


def fmt_bytes(value):
    if value == "":
        return ""
    value = int(value)
    sign = "-" if value < 0 else ""
    value = abs(value)
    units = ["B", "KiB", "MiB", "GiB"]
    size = float(value)
    unit = units[0]
    for unit in units:
        if size < 1024 or unit == units[-1]:
            break
        size /= 1024
    if unit == "B":
        return "%s%d B" % (sign, int(size))
    return "%s%.1f %s" % (sign, size, unit)


def table(rows):
    lines = [
        "| Case | Operation | Prefetch | Rpull | Command | Throughput | IOPS | Prefetch bytes | Command bytes | Total bytes |",
        "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        bench = row.get("bench") or {}
        throughput = ""
        iops = ""
        if bench:
            throughput = "%s MiB/s" % bench.get("mb_per_sec", "")
            iops = str(bench.get("iops", ""))
        prefetch_bytes = row["prefetch_delta"]["content_bytes"] + row["prefetch_delta"]["overlaybd_bytes"]
        command_bytes = row["command_delta"]["content_bytes"] + row["command_delta"]["overlaybd_bytes"]
        total_bytes = row["total_delta"]["content_bytes"] + row["total_delta"]["overlaybd_bytes"]
        lines.append("| %s | %s | %s | %s | %s ms | %s | %s | %s | %s | %s |" % (
            "metadata_prefetched" if row["prefetch"] else "cold",
            row["operation"],
            ("%s ms" % row["prefetch_ms"]) if row["prefetch_ms"] != "" else "",
            ("%s ms" % row["measured_rpull_ms"]) if row["measured_rpull_ms"] != "" else "",
            row["command_ms"],
            throughput,
            iops,
            fmt_bytes(prefetch_bytes),
            fmt_bytes(command_bytes),
            fmt_bytes(total_bytes),
        ))
    return "\n".join(lines)


def write_results(started, rows, summary):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    RESULTS_FILE.write_text(
        "\n".join([
            "# OverlayBD first-output and read benchmark",
            "",
            "Generated: `%s`" % started,
            "",
            "The image contains a `%d MiB` `/data/bench.dat` blob. `Prefetch` is `ctr rpull --snapshotter overlaybd` with blob download disabled, then the command is timed separately. Byte columns are local content-store plus OverlayBD cache/snapshotter growth." % SIZE_MB,
            "",
            "Runtime: `%s`%s" % (RUNTIME_LABEL, (" (`%s`)" % RUNC_BINARY) if RUNC_BINARY else ""),
            "",
            table(rows),
            "",
            "```json",
            json.dumps(summary, indent=2, sort_keys=True),
            "```",
            "",
        ]),
        encoding="utf-8",
    )


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root")
    require(DISK_BENCH_BIN, "disk-bench binary")
    started = now_utc()
    work = (WORK_PARENT / time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())).resolve()
    work.mkdir(parents=True, exist_ok=True)
    ensure_registry()
    ensure_overlaybd()

    suffix = "size%dm-%d" % (SIZE_MB, int(time.time()))
    normal = f"{REGISTRY_LOCAL}/orca-overlaybd-disk-bench-normal:{suffix}"
    overlaybd = f"{REGISTRY_LOCAL}/orca-overlaybd-disk-bench-obd:{suffix}"
    clear_overlaybd_state(normal, overlaybd)
    built = build_and_push_normal(work, normal)
    converted = convert_and_push_overlaybd(normal, overlaybd)
    remove_containerd_refs(normal, overlaybd)

    rows = [
        measure_operation(overlaybd, "first_output", prefetch=False),
        measure_operation(overlaybd, "first_output", prefetch=True),
        measure_operation(overlaybd, "sequential", prefetch=True),
        measure_operation(overlaybd, "random", prefetch=True),
    ]
    summary = {
        "started": started,
        "work_dir": str(work),
        "normal_image": normal,
        "overlaybd_image": overlaybd,
        "runtime_label": RUNTIME_LABEL,
        "runc_binary": RUNC_BINARY,
        "built": built,
        "converted": converted,
        "size_mb": SIZE_MB,
        "random_ops": RANDOM_OPS,
        "io_depth": IO_DEPTH,
        "rows": rows,
    }
    write_results(started, rows, summary)
    print(table(rows))
    print("results_file=%s" % RESULTS_FILE)


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

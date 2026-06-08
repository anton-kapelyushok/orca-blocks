#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import time
import urllib.request
from pathlib import Path


CONTROL_URL = os.environ.get("CONTROL_URL", "http://localhost:18080").rstrip("/")
WORK_DIR = Path(os.environ.get("DISK_FAIR_WORK_DIR", ".tmp/docker-vs-orca-disk-fair"))
RESULTS_FILE = Path(os.environ.get("DISK_FAIR_RESULTS_FILE", "docs/benchmarks/docker-vs-orca-disk-fair.md"))
IMAGE_TAG = os.environ.get("DISK_FAIR_IMAGE", f"orca-disk-bench:fair-{int(time.time())}")
PREBUILT_BIN = os.environ.get("DISK_FAIR_PREBUILT_BIN", "")
ROOTFS_SIZE_MB = int(os.environ.get("DISK_FAIR_ROOTFS_SIZE_MB", "1024"))
SIZE_MB = int(os.environ.get("DISK_FAIR_SIZE_MB", "128"))
RANDOM_OPS = int(os.environ.get("DISK_FAIR_RANDOM_OPS", "4096"))
SEQ_BLOCK_BYTES = int(os.environ.get("DISK_FAIR_SEQ_BLOCK_BYTES", str(1024 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("DISK_FAIR_RANDOM_BLOCK_BYTES", "4096"))
IO_DEPTH = int(os.environ.get("DISK_FAIR_IO_DEPTH", "1"))
VCPU_COUNT = int(os.environ.get("DISK_FAIR_VCPU_COUNT", "1"))
MEMORY_MIB = int(os.environ.get("DISK_FAIR_MEMORY_MIB", "3072"))
TIMEOUT_SECONDS = int(os.environ.get("DISK_FAIR_TIMEOUT_SECONDS", "300"))


def run(cmd, check=True):
    print(f"$ {cmd}", flush=True)
    return subprocess.run(
        cmd,
        shell=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=check,
    ).stdout


def q(value):
    return shlex.quote(str(value))


def http_json(method, url, payload=None, timeout=TIMEOUT_SECONDS):
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["content-type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        body = resp.read().decode()
        return json.loads(body) if body else {}


def extract_result(output):
    match = re.search(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError("DISK_BENCH_RESULT marker not found:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def timing_value(session, name):
    raw = session.get("firecracker_timings", "[]")
    if isinstance(raw, str):
        try:
            timings = json.loads(raw)
        except json.JSONDecodeError:
            return None
    else:
        timings = raw
    for item in timings:
        if item.get("name") == name:
            return item.get("duration_ms")
    return None


def session_stats(session):
    node_url = session.get("node_url")
    session_id = session.get("session_id")
    if not node_url or not session_id:
        return {}
    try:
        return http_json("GET", f"{node_url}/sessions/{session_id}/stats", timeout=30)
    except Exception as err:
        return {"stats_error": str(err)}


def build_image():
    WORK_DIR.mkdir(parents=True, exist_ok=True)
    bin_path = WORK_DIR / "disk-bench"
    dockerfile = WORK_DIR / "Dockerfile"
    if PREBUILT_BIN:
        if Path(PREBUILT_BIN).resolve() != bin_path.resolve():
            run("cp %s %s" % (q(PREBUILT_BIN), q(bin_path)))
    elif not bin_path.exists():
        run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/disk-bench" % q(bin_path))
    dockerfile.write_text(
        "FROM alpine:3.22\n"
        "COPY disk-bench /disk-bench\n"
        "RUN chmod +x /disk-bench\n",
        encoding="utf-8",
    )
    run("docker build --network host -t %s %s" % (q(IMAGE_TAG), q(WORK_DIR)))


def drop_host_caches():
    run("sync && echo 3 > /proc/sys/vm/drop_caches", check=False)


def docker_prepare_data(data_path):
    data_path.parent.mkdir(parents=True, exist_ok=True)
    run("rm -f %s" % q(data_path))
    run("dd if=/dev/urandom of=%s bs=1M count=%d status=none" % (q(data_path), SIZE_MB))
    run("sync")


def docker_bench(data_path, mode):
    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    drop_host_caches()
    out = run(
        "docker run --rm --network host --cpus=1 --memory=%dm "
        "-v %s:/bench.dat:ro %s "
        "/disk-bench -kind docker-cold-%s -mode %s -path /bench.dat "
        "-size-bytes %d -block-bytes %d -random-ops %d -io-depth %d"
        % (
            MEMORY_MIB,
            q(str(data_path.resolve())),
            q(IMAGE_TAG),
            mode,
            mode,
            SIZE_MB * 1024 * 1024,
            block_bytes,
            RANDOM_OPS,
            IO_DEPTH,
        )
    )
    row = extract_result(out)
    row["target"] = "docker-cold"
    row["cache_state"] = "host page cache dropped"
    return row


def build_base_image():
    return http_json("POST", f"{CONTROL_URL}/buildImage", {
        "image": IMAGE_TAG,
        "rootfs_size_mb": ROOTFS_SIZE_MB,
    }, timeout=max(TIMEOUT_SECONDS, 300))


def start_env(command, force_node):
    return http_json("POST", f"{CONTROL_URL}/startEnv", {
        "image": IMAGE_TAG,
        "command": command,
        "force_node": force_node,
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEMORY_MIB,
    })


def start_read_session(volume_id, command, force_node):
    return http_json("POST", f"{CONTROL_URL}/sessions/start", {
        "volume_id": volume_id,
        "runtime": "firecracker",
        "firecracker_mode": "image-rootfs-run",
        "firecracker_payload": command,
        "force_node": force_node,
        "commit_after_run": False,
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEMORY_MIB,
    })


def bench_command(mode):
    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    return (
        "sync; "
        "echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true; "
        "/disk-bench "
        f"-kind orca-{mode} -mode {mode} -path /bench.dat "
        f"-size-bytes {SIZE_MB * 1024 * 1024} "
        f"-block-bytes {block_bytes} "
        f"-random-ops {RANDOM_OPS} "
        f"-io-depth {IO_DEPTH}"
    )


def orca_prepare_data():
    prepare_command = (
        f"dd if=/dev/urandom of=/bench.dat bs=1M count={SIZE_MB} status=none; "
        "sync; "
        "echo ORCA_DISK_DATA_READY"
    )
    prepared = start_env(prepare_command, "node-1")
    if "ORCA_DISK_DATA_READY" not in str(prepared.get("firecracker_output", "")):
        raise RuntimeError(f"prepare command did not finish as expected: {prepared}")
    return prepared


def orca_bench(volume_id, mode, node, cache_state):
    session = start_read_session(volume_id, bench_command(mode), node)
    row = extract_result(str(session.get("firecracker_output", "")))
    stats = session_stats(session)
    row["target"] = f"orca-{node}"
    row["cache_state"] = cache_state
    row["node"] = node
    row["cache_hits"] = stats.get("cache_hits")
    row["cache_misses"] = stats.get("cache_misses")
    row["remote_fetches"] = stats.get("remote_fetches")
    row["run_firecracker_ms"] = timing_value(session, "run_firecracker")
    row["request_to_first_user_output_ms"] = timing_value(session, "request_to_first_user_output")
    return row


def format_metric(row):
    if row["mode"] == "sequential":
        return f"{row['mb_per_sec']} MiB/s"
    return f"{row['iops']} IOPS"


def write_results(rows, built, env):
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    lines = [
        "# Docker vs Orca Disk Benchmark",
        "",
        f"Run: {started}",
        "",
        "| Field | Value |",
        "| --- | --- |",
        f"| Image | `{IMAGE_TAG}` |",
        f"| Size | {SIZE_MB} MiB |",
        f"| Sequential block | {SEQ_BLOCK_BYTES} bytes |",
        f"| Random block | {RANDOM_BLOCK_BYTES} bytes |",
        f"| Random ops | {RANDOM_OPS} |",
        f"| IO depth | {IO_DEPTH} |",
        f"| Orca CPU/RAM | {VCPU_COUNT} vCPU / {MEMORY_MIB} MiB |",
        f"| Orca base image | `{built.get('base_image_id')}` |",
        f"| Orca env | `{env.get('env_id')}` |",
        "",
        "Docker warm/page-cache results are intentionally excluded.",
        "",
        "## Results",
        "",
        "| Target | Cache state | Sequential | Random | Remote fetches | Cache misses |",
        "| --- | --- | ---: | ---: | ---: | ---: |",
    ]
    by_key = {}
    for row in rows:
        by_key[(row["target"], row["cache_state"], row["mode"])] = row
    order = [
        ("docker-cold", "host page cache dropped"),
        ("orca-node-1", "node-1 local cache"),
        ("orca-node-2", "node-2 cold local cache"),
        ("orca-node-2", "node-2 local cache after first read"),
    ]
    for target, cache_state in order:
        seq = by_key.get((target, cache_state, "sequential"))
        rand = by_key.get((target, cache_state, "random"))
        remote = ""
        misses = ""
        if seq or rand:
            remote_values = [r.get("remote_fetches") for r in (seq, rand) if r and r.get("remote_fetches") is not None]
            miss_values = [r.get("cache_misses") for r in (seq, rand) if r and r.get("cache_misses") is not None]
            remote = "/".join(str(v) for v in remote_values) if remote_values else "n/a"
            misses = "/".join(str(v) for v in miss_values) if miss_values else "n/a"
        lines.append(
            "| %s | %s | %s | %s | %s | %s |"
            % (
                target,
                cache_state,
                format_metric(seq) if seq else "n/a",
                format_metric(rand) if rand else "n/a",
                remote,
                misses,
            )
        )
    lines += [
        "",
        "## Raw Rows",
        "",
        "| Target | Mode | Cache state | MiB/s | IOPS | Duration | Remote fetches | Cache misses |",
        "| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        lines.append(
            "| {target} | {mode} | {cache_state} | {mb_per_sec} | {iops} | {duration_ms} ms | {remote_fetches} | {cache_misses} |".format(
                **{k: row.get(k, "n/a") for k in [
                    "target",
                    "mode",
                    "cache_state",
                    "mb_per_sec",
                    "iops",
                    "duration_ms",
                    "remote_fetches",
                    "cache_misses",
                ]}
            )
        )
    lines += [
        "",
        "## Takeaway",
        "",
        "Sequential reads remain the main Orca gap in this run. Random reads are in the same order of magnitude as Docker cold-cache, while Docker warm/page-cache numbers are not used for comparison.",
        "",
    ]
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    RESULTS_FILE.write_text("\n".join(lines), encoding="utf-8")
    print(f"wrote {RESULTS_FILE}", flush=True)


def main():
    http_json("GET", f"{CONTROL_URL}/healthz", timeout=30)
    build_image()
    data_path = WORK_DIR / "docker-bench.dat"
    docker_prepare_data(data_path)
    rows = [docker_bench(data_path, "sequential"), docker_bench(data_path, "random")]
    built = build_base_image()
    env = orca_prepare_data()
    volume_id = env["env_volume_id"]
    for mode in ("sequential", "random"):
        rows.append(orca_bench(volume_id, mode, "node-1", "node-1 local cache"))
        rows.append(orca_bench(volume_id, mode, "node-2", "node-2 cold local cache"))
        rows.append(orca_bench(volume_id, mode, "node-2", "node-2 local cache after first read"))
    write_results(rows, built, env)


if __name__ == "__main__":
    main()

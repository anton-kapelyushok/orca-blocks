#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path


CONTROL_URL = os.environ.get("CONTROL_URL", "http://localhost:18080").rstrip("/")
WORK_DIR = Path(os.environ.get("ORCA_IOPS_WORK_DIR", ".tmp/orca-iops"))
RESULTS_FILE = Path(os.environ.get("ORCA_IOPS_RESULTS_FILE", "docs/benchmarks/orca-iops-results.txt"))
IMAGE_TAG = os.environ.get("ORCA_IOPS_IMAGE", f"orca-disk-bench:bench-{int(time.time())}")
PREBUILT_BIN = os.environ.get("ORCA_IOPS_PREBUILT_BIN", "")
ROOTFS_SIZE_MB = int(os.environ.get("ORCA_IOPS_ROOTFS_SIZE_MB", "1024"))
SIZE_MB = int(os.environ.get("ORCA_IOPS_SIZE_MB", "128"))
RANDOM_OPS = int(os.environ.get("ORCA_IOPS_RANDOM_OPS", "4096"))
SEQ_BLOCK_BYTES = int(os.environ.get("ORCA_IOPS_SEQ_BLOCK_BYTES", str(1024 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("ORCA_IOPS_RANDOM_BLOCK_BYTES", "4096"))
IO_DEPTH = int(os.environ.get("ORCA_IOPS_IO_DEPTH", "1"))
VCPU_COUNT = int(os.environ.get("ORCA_IOPS_VCPU_COUNT", "1"))
MEMORY_MIB = int(os.environ.get("ORCA_IOPS_MEMORY_MIB", "1024"))
TIMEOUT_SECONDS = int(os.environ.get("ORCA_IOPS_TIMEOUT_SECONDS", "240"))
MODES = [m.strip() for m in os.environ.get("ORCA_IOPS_MODES", "sequential,random").split(",") if m.strip()]
READ_NODES = [n.strip() for n in os.environ.get("ORCA_IOPS_READ_NODES", "node-1,node-2,node-2").split(",") if n.strip()]


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
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as err:
        body = err.read().decode(errors="replace")
        raise RuntimeError(f"{method} {url} failed: {err.code} {err.reason}: {body}") from err


def extract_disk_result(output):
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
        print(f"using prebuilt disk-bench binary {PREBUILT_BIN}", flush=True)
        if Path(PREBUILT_BIN).resolve() != bin_path.resolve():
            run("cp %s %s" % (q(PREBUILT_BIN), q(bin_path)))
    elif bin_path.exists():
        print(f"reusing existing disk-bench binary at {bin_path}", flush=True)
    else:
        print(f"building disk-bench binary for {IMAGE_TAG}", flush=True)
        run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/disk-bench" % q(bin_path))
    dockerfile.write_text(
        "FROM alpine:3.22\n"
        "COPY disk-bench /disk-bench\n"
        "RUN chmod +x /disk-bench\n",
        encoding="utf-8",
    )
    print(f"building tiny benchmark container image {IMAGE_TAG}", flush=True)
    run("docker build -t %s %s" % (q(IMAGE_TAG), q(WORK_DIR)))


def build_base_image():
    print(f"building Orca base image from {IMAGE_TAG}", flush=True)
    return http_json("POST", f"{CONTROL_URL}/buildImage", {
        "image": IMAGE_TAG,
        "rootfs_size_mb": ROOTFS_SIZE_MB,
    }, timeout=max(TIMEOUT_SECONDS, 300))


def start_env(command, force_node):
    payload = {
        "image": IMAGE_TAG,
        "command": command,
        "force_node": force_node,
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEMORY_MIB,
    }
    return http_json("POST", f"{CONTROL_URL}/startEnv", payload)


def resume_env(env_id, command, force_node):
    payload = {
        "env_id": env_id,
        "command": command,
        "force_node": force_node,
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEMORY_MIB,
    }
    return http_json("POST", f"{CONTROL_URL}/resumeEnv", payload)


def start_read_session(volume_id, command, force_node):
    payload = {
        "volume_id": volume_id,
        "runtime": "firecracker",
        "firecracker_mode": "image-rootfs-run",
        "firecracker_payload": command,
        "force_node": force_node,
        "commit_after_run": False,
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEMORY_MIB,
    }
    return http_json("POST", f"{CONTROL_URL}/sessions/start", payload)


def bench_command(mode):
    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    kind = f"orca-{mode}"
    return (
        "sync; "
        "echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true; "
        "/disk-bench "
        f"-kind {kind} -mode {mode} -path /bench.dat "
        f"-size-bytes {SIZE_MB * 1024 * 1024} "
        f"-block-bytes {block_bytes} "
        f"-random-ops {RANDOM_OPS} "
        f"-io-depth {IO_DEPTH}"
    )


def append_results(lines):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    with RESULTS_FILE.open("a", encoding="utf-8") as f:
        for line in lines:
            f.write(line.rstrip() + "\n")


def main():
    http_json("GET", f"{CONTROL_URL}/healthz", timeout=30)
    build_image()
    built = build_base_image()
    print("base image built:", json.dumps({
        "base_image_id": built.get("base_image_id"),
        "volume_id": built.get("volume_id"),
        "snapshot_id": built.get("snapshot_id"),
        "duration_ms": built.get("duration_ms"),
    }, sort_keys=True), flush=True)

    prepare_command = (
        f"dd if=/dev/urandom of=/bench.dat bs=1M count={SIZE_MB} status=none; "
        "sync; "
        "ls -lh /bench.dat; "
        "echo ORCA_IOPS_DATA_READY"
    )
    print("creating benchmark data through Orca on node-1", flush=True)
    prepared = start_env(prepare_command, "node-1")
    if "ORCA_IOPS_DATA_READY" not in str(prepared.get("firecracker_output", "")):
        raise RuntimeError(f"prepare command did not finish as expected: {prepared}")
    env_id = prepared["env_id"]
    env_volume_id = prepared["env_volume_id"]

    rows = []
    for mode in MODES:
        for index, node in enumerate(READ_NODES, start=1):
            label = f"{node}-read-{index}"
            print(f"running Orca {mode} benchmark on {label}", flush=True)
            session = start_read_session(env_volume_id, bench_command(mode), node)
            result = extract_disk_result(str(session.get("firecracker_output", "")))
            stats = session_stats(session)
            row = {
                "label": label,
                "node": node,
                "mode": mode,
                "mb_per_sec": result.get("mb_per_sec"),
                "iops": result.get("iops"),
                "duration_ms": result.get("duration_ms"),
                "ops": result.get("ops"),
                "bytes_read": result.get("bytes_read"),
                "block_bytes": result.get("block_bytes"),
                "io_depth": result.get("io_depth"),
                "request_to_first_user_output_ms": timing_value(session, "request_to_first_user_output"),
                "run_firecracker_ms": timing_value(session, "run_firecracker"),
                "cache_hits": stats.get("cache_hits"),
                "cache_misses": stats.get("cache_misses"),
                "remote_fetches": stats.get("remote_fetches"),
                "zero_fills": stats.get("zero_fills"),
                "env_id": env_id,
                "session_id": session.get("session_id"),
            }
            rows.append(row)
            print(json.dumps(row, sort_keys=True), flush=True)

    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    lines = [
        "",
        f"orca-iops run {started}",
        f"control_url={CONTROL_URL}",
        f"image={IMAGE_TAG}",
        f"base_image_id={built.get('base_image_id')}",
        f"env_id={env_id}",
        f"size_mb={SIZE_MB} random_ops={RANDOM_OPS} io_depth={IO_DEPTH} vcpus={VCPU_COUNT} memory_mib={MEMORY_MIB}",
        "",
        "label\tmode\tmb_per_sec\tiops\tduration_ms\tblock_bytes\tcache_hits\tcache_misses\tremote_fetches\trun_firecracker_ms\trequest_to_first_user_output_ms",
    ]
    for row in rows:
        lines.append(
            "{label}\t{mode}\t{mb_per_sec}\t{iops}\t{duration_ms}\t{block_bytes}\t{cache_hits}\t{cache_misses}\t{remote_fetches}\t{run_firecracker_ms}\t{request_to_first_user_output_ms}".format(**row)
        )
    append_results(lines)
    print(f"wrote {RESULTS_FILE}", flush=True)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import sys
import time
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
RESULTS_FILE = Path(os.environ.get(
    "FC_VHOST_RESULTS_FILE",
    "docs/benchmarks/firecracker-vhost-user-blk-results.md",
))
BACKENDS = [v.strip() for v in os.environ.get(
    "FC_VHOST_BACKENDS",
    "virtio-blk,vhost-user-blk",
).split(",") if v.strip()]
MODES = [v.strip() for v in os.environ.get(
    "FC_VHOST_MODES",
    os.environ.get("DISK_BENCH_MODES", "sequential,random"),
).split(",") if v.strip()]


def q(value):
    return shlex.quote(str(value))


def run_backend(backend):
    env = os.environ.copy()
    env["DISK_BENCH_BACKEND"] = backend
    env["DISK_BENCH_SKIP_DOCKER"] = "true"
    env["DISK_BENCH_MODES"] = ",".join(MODES)

    cmd = [sys.executable, str(ROOT / "scripts" / "measure-disk-docker-vs-firecracker.py")]
    start = time.time()
    proc = subprocess.run(
        cmd,
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    elapsed_ms = int((time.time() - start) * 1000)
    output = proc.stdout
    if proc.returncode != 0:
        raise RuntimeError(
            "backend %s failed after %dms:\n%s" % (backend, elapsed_ms, output[-6000:])
        )

    match = re.search(r"SUMMARY_JSON=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError(
            "backend %s did not print SUMMARY_JSON:\n%s" % (backend, output[-6000:])
        )
    return json.loads(match.group(1)), output, elapsed_ms


def fmt_num(value):
    try:
        as_float = float(value)
    except (TypeError, ValueError):
        return str(value)
    if as_float >= 100:
        return "%.0f" % as_float
    if as_float >= 10:
        return "%.1f" % as_float
    return "%.2f" % as_float


def row_for(backend, mode, result):
    fc = result[mode]["firecracker"]
    return {
        "backend": backend,
        "mode": mode,
        "mb_per_sec": fmt_num(fc.get("mb_per_sec")),
        "iops": fmt_num(fc.get("iops")),
        "duration_ms": str(fc.get("duration_ms")),
        "wall_ms": str(fc.get("wall_ms")),
        "block_bytes": str(fc.get("block_bytes")),
        "io_depth": str(fc.get("io_depth")),
        "direct": str(fc.get("direct")),
        "read_ahead_kb": str(fc.get("read_ahead_kb")),
    }


def write_markdown(rows, raw_logs):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        "# Firecracker vhost-user-blk Benchmark",
        "",
        "Generated: `%s`" % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "",
        "This compares Firecracker's built-in file-backed `virtio-blk` drive with a `vhost-user-blk` drive backed by `qemu-storage-daemon` over a Unix socket. It does not start the Orca stack.",
        "",
        "| Backend | Mode | Throughput | IOPS | Guest Duration | Wall Time | Block | IO Depth | Direct | Readahead |",
        "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | ---: |",
    ]
    for row in rows:
        lines.append(
            "| {backend} | {mode} | {mb_per_sec} MiB/s | {iops} | {duration_ms} ms | {wall_ms} ms | {block_bytes} | {io_depth} | {direct} | {read_ahead_kb} |".format(**row)
        )
    lines.extend(["", "## Raw Logs", ""])
    for backend, output in raw_logs:
        lines.extend([
            "<details>",
            "<summary>%s</summary>" % backend,
            "",
            "```text",
            output.strip(),
            "```",
            "",
            "</details>",
            "",
        ])
    RESULTS_FILE.write_text("\n".join(lines), encoding="utf-8")


def main():
    if sys.platform != "linux":
        raise SystemExit("Firecracker requires Linux/KVM; this host is %s" % sys.platform)
    if not Path("/dev/kvm").exists():
        raise SystemExit("Firecracker requires /dev/kvm; it is not present on this host")
    if "vhost-user-blk" in BACKENDS and not shutil_which("qemu-storage-daemon"):
        raise SystemExit("qemu-storage-daemon is required for vhost-user-blk")

    rows = []
    raw_logs = []
    for backend in BACKENDS:
        result, output, _elapsed_ms = run_backend(backend)
        raw_logs.append((backend, output))
        for mode in MODES:
            rows.append(row_for(backend, mode, result))
    write_markdown(rows, raw_logs)
    print("wrote %s" % RESULTS_FILE)
    print(RESULTS_FILE.read_text(encoding="utf-8").split("## Raw Logs", 1)[0].rstrip())


def shutil_which(name):
    for directory in os.environ.get("PATH", "").split(os.pathsep):
        candidate = Path(directory) / name
        if candidate.exists() and os.access(candidate, os.X_OK):
            return str(candidate)
    return None


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

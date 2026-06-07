#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import sys
import time
from pathlib import Path


IMAGE = os.environ.get("CPU_BENCH_IMAGE", "alpine:3.22")
ASSET_DIR = Path(os.environ.get("ASSET_DIR", "firecracker-assets"))
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/cpu-bench"))
ITERATIONS = int(os.environ.get("CPU_BENCH_ITERATIONS", "600000000"))
TIMEOUT_SECONDS = int(os.environ.get("CPU_BENCH_TIMEOUT_SECONDS", "120"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "256"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "1"))


def run(cmd, check=True):
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


def now_ms():
    return time.time_ns() // 1_000_000


def extract_result(output):
    match = re.search(r"CPU_BENCH_RESULT=(\{.*?\})", output)
    if not match:
        raise RuntimeError("CPU_BENCH_RESULT marker not found in output:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def build_binary(bin_path):
    bin_path.parent.mkdir(parents=True, exist_ok=True)
    print("building static linux cpu benchmark at %s" % bin_path, flush=True)
    run(
        "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/cpu-bench"
        % q(bin_path)
    )


def measure_docker(bin_path):
    print("running single-thread cpu benchmark in Docker image %s" % IMAGE, flush=True)
    start = now_ms()
    output = run(
        "docker run --rm --entrypoint /cpu-bench -v %s:/cpu-bench:ro %s "
        "-kind docker -iterations %d"
        % (q(bin_path), q(IMAGE), ITERATIONS)
    )
    result = extract_result(output)
    result["wall_ms"] = now_ms() - start
    result["raw_output"] = output.strip()
    return result


def build_initramfs(bin_path, initramfs_path):
    initramfs_path.parent.mkdir(parents=True, exist_ok=True)
    root = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "initramfs.XXXXXX"))).strip())
    try:
        run("install -m 0755 %s %s" % (q(bin_path), q(root / "init")))
        run(
            "cd %s && find . -print0 | cpio --null -ov --format=newc 2>/dev/null | gzip -9 > %s"
            % (q(root), q(initramfs_path.resolve()))
        )
    finally:
        run("rm -rf %s" % q(root), check=False)


def measure_firecracker(bin_path):
    firecracker = ASSET_DIR / "firecracker"
    kernel = ASSET_DIR / "vmlinux"
    if not firecracker.exists() or not kernel.exists():
        raise RuntimeError("missing Firecracker assets under %s" % ASSET_DIR)
    if not Path("/dev/kvm").exists():
        raise RuntimeError("/dev/kvm is missing")

    run_dir = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "firecracker.XXXXXX"))).strip())
    initramfs = run_dir / "cpu-bench-initramfs.cpio.gz"
    serial_log = run_dir / "serial.log"
    firecracker_log = run_dir / "firecracker.log"
    socket_path = run_dir / "firecracker.sock"
    config_path = run_dir / "firecracker.json"
    build_initramfs(bin_path, initramfs)

    boot_args = (
        "console=ttyS0 reboot=k panic=1 pci=off init=/init "
        "-kind firecracker -iterations %d"
    ) % ITERATIONS
    config = {
        "boot-source": {
            "kernel_image_path": str(kernel.resolve()),
            "initrd_path": str(initramfs.resolve()),
            "boot_args": boot_args,
        },
        "drives": [],
        "machine-config": {
            "vcpu_count": VCPU_COUNT,
            "mem_size_mib": MEM_SIZE_MIB,
            "track_dirty_pages": False,
        },
        "logger": {
            "log_path": str(firecracker_log),
            "level": "Info",
            "show_level": True,
            "show_log_origin": True,
        },
    }
    config_path.write_text(json.dumps(config, indent=2))

    print("running single-thread cpu benchmark in Firecracker", flush=True)
    start = now_ms()
    serial = serial_log.open("wb")
    fc_err = firecracker_log.open("ab")
    proc = subprocess.Popen(
        [str(firecracker.resolve()), "--api-sock", str(socket_path), "--config-file", str(config_path)],
        stdout=serial,
        stderr=fc_err,
    )
    serial.close()
    fc_err.close()
    try:
        deadline = time.time() + TIMEOUT_SECONDS
        output = ""
        while time.time() < deadline:
            if serial_log.exists():
                output = serial_log.read_text(errors="replace")
            if "CPU_BENCH_RESULT=" in output:
                result = extract_result(output)
                result["wall_ms"] = now_ms() - start
                result["work_dir"] = str(run_dir)
                result["serial_log"] = str(serial_log)
                return result
            if proc.poll() is not None and "CPU_BENCH_RESULT=" not in output:
                raise RuntimeError("Firecracker exited before result:\n%s" % output[-4000:])
            time.sleep(0.05)
        raise TimeoutError("timed out waiting for CPU_BENCH_RESULT; serial log: %s" % serial_log)
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)


def print_result(result):
    print(
        "[%(kind)s] duration_ms=%(duration_ms)s wall_ms=%(wall_ms)s ops_per_sec=%(ops_per_sec)s "
        "num_cpu=%(num_cpu)s gomaxprocs=%(gomaxprocs)s checksum=%(checksum)s"
        % result
    )
    if result.get("serial_log"):
        print("  serial_log=%s" % result["serial_log"])


def main():
    WORK_PARENT.mkdir(parents=True, exist_ok=True)
    bin_path = WORK_PARENT / "cpu-bench-linux-amd64"
    build_binary(bin_path)
    docker = measure_docker(bin_path)
    firecracker = measure_firecracker(bin_path)
    print()
    print_result(docker)
    print_result(firecracker)
    ratio = float(firecracker["duration_ms"]) / float(docker["duration_ms"])
    print("firecracker_vs_docker_duration_ratio=%.2fx" % ratio)
    print("SUMMARY_JSON=%s" % json.dumps({"docker": docker, "firecracker": firecracker, "ratio": ratio}, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

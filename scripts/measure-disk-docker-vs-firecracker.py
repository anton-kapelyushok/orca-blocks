#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import sys
import time
from pathlib import Path


IMAGE = os.environ.get("DISK_BENCH_IMAGE", "alpine:3.22")
ASSET_DIR = Path(os.environ.get("ASSET_DIR", "firecracker-assets"))
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/disk-bench"))
DATA_DIR = Path(os.environ.get("DISK_BENCH_DATA_DIR", str(WORK_PARENT)))
SIZE_MB = int(os.environ.get("DISK_BENCH_SIZE_MB", "256"))
RANDOM_OPS = int(os.environ.get("DISK_BENCH_RANDOM_OPS", "65536"))
SEQ_BLOCK_BYTES = int(os.environ.get("DISK_BENCH_SEQ_BLOCK_BYTES", str(128 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("DISK_BENCH_RANDOM_BLOCK_BYTES", "4096"))
TIMEOUT_SECONDS = int(os.environ.get("DISK_BENCH_TIMEOUT_SECONDS", "120"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "256"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "1"))
DIRECT = os.environ.get("DISK_BENCH_DIRECT", "false").lower() in {"1", "true", "yes", "on"}
READ_AHEAD_KB = os.environ.get("DISK_BENCH_READ_AHEAD_KB")
MODES = [m.strip() for m in os.environ.get("DISK_BENCH_MODES", "sequential,random").split(",") if m.strip()]
SKIP_DOCKER = os.environ.get("DISK_BENCH_SKIP_DOCKER", "false").lower() in {"1", "true", "yes", "on"}
IO_ENGINE = os.environ.get("DISK_BENCH_IO_ENGINE", "").strip()
BACKEND = os.environ.get("DISK_BENCH_BACKEND", "virtio-blk").strip()
PREBUILT_BIN = os.environ.get("DISK_BENCH_BIN", "").strip()
IO_DEPTH = int(os.environ.get("DISK_BENCH_IO_DEPTH", "1"))


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
    match = re.search(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError("DISK_BENCH_RESULT marker not found in output:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def build_binary(bin_path):
    bin_path.parent.mkdir(parents=True, exist_ok=True)
    print("building static linux disk benchmark at %s" % bin_path, flush=True)
    run(
        "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/disk-bench"
        % q(bin_path)
    )


def prepare_data_file(path):
    size_bytes = SIZE_MB * 1024 * 1024
    if path.exists() and path.stat().st_size == size_bytes:
        print("reusing benchmark data file %s" % path, flush=True)
        return
    print("creating %d MiB benchmark data file at %s" % (SIZE_MB, path), flush=True)
    path.unlink(missing_ok=True)
    run(
        "dd if=/dev/urandom of=%s bs=1M count=%d status=none"
        % (q(path), SIZE_MB)
    )


def drop_caches():
    run("sync", check=False)
    run("sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'", check=False)


def measure_docker(bin_path, data_path, mode):
    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    args = (
        "-kind docker-%s -mode %s -path /bench.dat -size-bytes %d "
        "-block-bytes %d -random-ops %d -io-depth %d"
        % (mode, mode, SIZE_MB * 1024 * 1024, block_bytes, RANDOM_OPS, IO_DEPTH)
    )
    if DIRECT:
        args += " -direct"
    if READ_AHEAD_KB is not None:
        args += " -read-ahead-kb %d" % int(READ_AHEAD_KB)
    print("running %s disk benchmark in Docker image %s" % (mode, IMAGE), flush=True)
    drop_caches()
    start = now_ms()
    output = run(
        "docker run --rm --entrypoint /disk-bench "
        "-v %s:/disk-bench:ro -v %s:/bench.dat:ro %s %s"
        % (q(bin_path), q(data_path), q(IMAGE), args)
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


def measure_firecracker(bin_path, data_path, mode):
    firecracker = ASSET_DIR / "firecracker"
    kernel = ASSET_DIR / "vmlinux"
    if not firecracker.exists() or not kernel.exists():
        raise RuntimeError("missing Firecracker assets under %s" % ASSET_DIR)
    if not Path("/dev/kvm").exists():
        raise RuntimeError("/dev/kvm is missing")

    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    run_dir = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "firecracker.XXXXXX"))).strip())
    initramfs = run_dir / "disk-bench-initramfs.cpio.gz"
    serial_log = run_dir / "serial.log"
    firecracker_log = run_dir / "firecracker.log"
    socket_path = run_dir / "firecracker.sock"
    vhost_socket_path = run_dir / "vhost-user-blk.sock"
    config_path = run_dir / "firecracker.json"
    qemu_log_path = run_dir / "qemu-storage-daemon.log"
    build_initramfs(bin_path, initramfs)

    boot_args = (
        "console=ttyS0 reboot=k panic=1 pci=off init=/init "
        "-kind firecracker-%s -mode %s -path /dev/vda -size-bytes %d "
        "-block-bytes %d -random-ops %d -io-depth %d"
        % (mode, mode, SIZE_MB * 1024 * 1024, block_bytes, RANDOM_OPS, IO_DEPTH)
    )
    if DIRECT:
        boot_args += " -direct"
    if READ_AHEAD_KB is not None:
        boot_args += " -read-ahead-kb %d" % int(READ_AHEAD_KB)
    qemu_process = None
    qemu_log = None
    if BACKEND == "vhost-user-blk":
        qemu_log = qemu_log_path.open("ab")
        qemu_process = subprocess.Popen(
            [
                "qemu-storage-daemon",
                "--blockdev",
                "driver=file,node-name=bench-file,filename=%s" % str(data_path.resolve()),
                "--blockdev",
                "driver=raw,node-name=bench-raw,file=bench-file",
                "--export",
                "type=vhost-user-blk,id=bench-export,node-name=bench-raw,addr.type=unix,addr.path=%s,writable=off,num-queues=1,logical-block-size=512"
                % str(vhost_socket_path),
            ],
            stdout=qemu_log,
            stderr=subprocess.STDOUT,
        )
        deadline = time.time() + 10
        while time.time() < deadline and not vhost_socket_path.exists():
            if qemu_process.poll() is not None:
                raise RuntimeError(
                    "qemu-storage-daemon exited before socket appeared:\n%s"
                    % qemu_log_path.read_text(errors="replace")
                )
            time.sleep(0.05)
        if not vhost_socket_path.exists():
            raise TimeoutError("timed out waiting for vhost-user-blk socket %s" % vhost_socket_path)
        drive = {
            "drive_id": "bench",
            "is_root_device": False,
            "socket": str(vhost_socket_path),
        }
    elif BACKEND == "virtio-blk":
        drive = {
            "drive_id": "bench",
            "path_on_host": str(data_path.resolve()),
            "is_root_device": False,
            "is_read_only": True,
        }
        if IO_ENGINE:
            drive["io_engine"] = IO_ENGINE
    else:
        raise RuntimeError("unsupported DISK_BENCH_BACKEND=%r" % BACKEND)
    config = {
        "boot-source": {
            "kernel_image_path": str(kernel.resolve()),
            "initrd_path": str(initramfs.resolve()),
            "boot_args": boot_args,
        },
        "drives": [drive],
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

    print("running %s disk benchmark in Firecracker" % mode, flush=True)
    drop_caches()
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
            if re.search(r"DISK_BENCH_RESULT=\{[^\r\n]*\}", output):
                result = extract_result(output)
                result["wall_ms"] = now_ms() - start
                result["work_dir"] = str(run_dir)
                result["serial_log"] = str(serial_log)
                if qemu_process is not None:
                    result["qemu_log"] = str(qemu_log_path)
                result["backend"] = BACKEND
                return result
            if proc.poll() is not None and "DISK_BENCH_RESULT=" not in output:
                raise RuntimeError("Firecracker exited before result:\n%s" % output[-4000:])
            time.sleep(0.05)
        raise TimeoutError("timed out waiting for DISK_BENCH_RESULT; serial log: %s" % serial_log)
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        if qemu_process is not None and qemu_process.poll() is None:
            qemu_process.terminate()
            try:
                qemu_process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                qemu_process.kill()
                qemu_process.wait(timeout=5)
        if qemu_log is not None:
            qemu_log.close()


def print_result(result):
    print(
        "[%(kind)s] mode=%(mode)s duration_ms=%(duration_ms)s wall_ms=%(wall_ms)s "
        "mb_per_sec=%(mb_per_sec)s iops=%(iops)s block_bytes=%(block_bytes)s "
        "direct=%(direct)s read_ahead_kb=%(read_ahead_kb)s io_depth=%(io_depth)s "
        "bytes_read=%(bytes_read)s checksum=%(checksum)s"
        % result
    )
    if result.get("serial_log"):
        print("  serial_log=%s" % result["serial_log"])
    if result.get("qemu_log"):
        print("  qemu_log=%s" % result["qemu_log"])


def main():
    WORK_PARENT.mkdir(parents=True, exist_ok=True)
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    bin_path = Path(PREBUILT_BIN).resolve() if PREBUILT_BIN else WORK_PARENT / "disk-bench-linux-amd64"
    data_path = DATA_DIR / ("bench-%dmib.dat" % SIZE_MB)
    if PREBUILT_BIN:
        print("using prebuilt disk benchmark at %s" % bin_path, flush=True)
    else:
        build_binary(bin_path)
    prepare_data_file(data_path)
    print("direct_io=%s" % DIRECT, flush=True)
    print("read_ahead_kb=%s" % (READ_AHEAD_KB if READ_AHEAD_KB is not None else "default"), flush=True)
    print("io_engine=%s" % (IO_ENGINE if IO_ENGINE else "default"), flush=True)
    print("backend=%s" % BACKEND, flush=True)
    print("io_depth=%s" % IO_DEPTH, flush=True)
    print("modes=%s" % ",".join(MODES), flush=True)
    print("skip_docker=%s" % SKIP_DOCKER, flush=True)

    results = {}
    for mode in MODES:
        docker = None
        if not SKIP_DOCKER:
            docker = measure_docker(bin_path, data_path, mode)
        firecracker = measure_firecracker(bin_path, data_path, mode)
        results[mode] = {"firecracker": firecracker}
        if docker is not None:
            results[mode]["docker"] = docker
        print()
        if docker is not None:
            print_result(docker)
        print_result(firecracker)
        if docker is not None:
            ratio = float(firecracker["duration_ms"]) / float(docker["duration_ms"])
            print("firecracker_vs_docker_%s_duration_ratio=%.2fx" % (mode, ratio))
    print("SUMMARY_JSON=%s" % json.dumps(results, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

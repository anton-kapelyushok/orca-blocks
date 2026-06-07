#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import sys
import time
from pathlib import Path


WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/vmm-bench"))
DATA_PATH = Path(os.environ.get("DISK_BENCH_DATA_PATH", ".tmp/disk-bench/bench-128mib.dat"))
BIN_PATH = Path(os.environ.get("DISK_BENCH_BIN", "bin/disk-bench"))
KERNEL_PATH = Path(os.environ.get("QEMU_KERNEL", "/boot/vmlinuz-" + os.uname().release))
MACHINE = os.environ.get("QEMU_MACHINE", "microvm")
SIZE_BYTES = int(os.environ.get("DISK_BENCH_SIZE_BYTES", str(128 * 1024 * 1024)))
RANDOM_OPS = int(os.environ.get("DISK_BENCH_RANDOM_OPS", "32768"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "256"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "1"))
TIMEOUT_SECONDS = int(os.environ.get("QEMU_TIMEOUT_SECONDS", "90"))
MODES = [m.strip() for m in os.environ.get("DISK_BENCH_MODES", "sequential,random").split(",") if m.strip()]


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


def build_initramfs(initramfs_path):
    initramfs_path.parent.mkdir(parents=True, exist_ok=True)
    root = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "qemu-initramfs.XXXXXX"))).strip())
    try:
        run("install -m 0755 %s %s" % (q(BIN_PATH), q(root / "init")))
        run(
            "cd %s && find . -print0 | cpio --null -ov --format=newc 2>/dev/null | gzip -9 > %s"
            % (q(root), q(initramfs_path.resolve()))
        )
    finally:
        run("rm -rf %s" % q(root), check=False)


def extract_result(output):
    match = re.search(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError("DISK_BENCH_RESULT marker not found:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def qemu_args(mode, initramfs_path, log_path):
    block_bytes = 1024 * 1024 if mode == "sequential" else 4096
    bench_args = (
        "-kind qemu-%s-%s -mode %s -path /dev/vda -size-bytes %d "
        "-block-bytes %d -random-ops %d -io-depth 1"
        % (MACHINE, mode, mode, SIZE_BYTES, block_bytes, RANDOM_OPS)
    )
    common = [
        "qemu-system-x86_64",
        "-enable-kvm",
        "-cpu",
        "host",
        "-smp",
        str(VCPU_COUNT),
        "-m",
        "%dM" % MEM_SIZE_MIB,
        "-nodefaults",
        "-no-user-config",
        "-nographic",
        "-no-reboot",
        "-serial",
        "stdio",
        "-kernel",
        str(KERNEL_PATH),
        "-initrd",
        str(initramfs_path),
        "-append",
        "console=ttyS0 quiet loglevel=0 reboot=t panic=-1 init=/init %s" % bench_args,
        "-drive",
        "file=%s,format=raw,if=none,id=bench,readonly=on" % DATA_PATH,
    ]
    if MACHINE == "microvm":
        return common[:2] + [
            "-M",
            "microvm,acpi=off,pcie=off,isa-serial=on,x-option-roms=off",
        ] + common[2:] + ["-device", "virtio-blk-device,drive=bench"], log_path
    if MACHINE == "q35":
        return common[:2] + ["-M", "q35,accel=kvm"] + common[2:] + ["-device", "virtio-blk-pci,drive=bench"], log_path
    raise RuntimeError("unsupported QEMU_MACHINE=%r" % MACHINE)


def measure(mode, initramfs_path):
    log_path = WORK_PARENT / ("qemu-%s-%s.log" % (MACHINE, mode))
    args, _ = qemu_args(mode, initramfs_path, log_path)
    run("sync", check=False)
    run("echo 3 > /proc/sys/vm/drop_caches", check=False)
    start = now_ms()
    proc = subprocess.run(
        ["timeout", str(TIMEOUT_SECONDS)] + args,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    wall_ms = now_ms() - start
    log_path.write_text(proc.stdout)
    result = extract_result(proc.stdout)
    result["wall_ms"] = wall_ms
    result["kernel"] = str(KERNEL_PATH)
    result["machine"] = MACHINE
    result["log_path"] = str(log_path)
    return result


def main():
    WORK_PARENT.mkdir(parents=True, exist_ok=True)
    initramfs_path = WORK_PARENT / "qemu-disk-bench-initramfs.cpio.gz"
    build_initramfs(initramfs_path)
    print("qemu_machine=%s" % MACHINE)
    print("qemu_kernel=%s" % KERNEL_PATH)
    print("data_path=%s" % DATA_PATH)
    results = {}
    for mode in MODES:
        result = measure(mode, initramfs_path)
        results[mode] = result
        print(
            "[%(kind)s] duration_ms=%(duration_ms)s wall_ms=%(wall_ms)s "
            "mb_per_sec=%(mb_per_sec)s iops=%(iops)s log=%(log_path)s"
            % result
        )
    print("SUMMARY_JSON=%s" % json.dumps(results, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

#!/usr/bin/env python3
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import time
from pathlib import Path


ASSET_DIR = Path(os.environ.get("ASSET_DIR", "firecracker-assets"))
WORK_DIR = Path(os.environ.get("FC_FILE_NBD_WORK_DIR", ".tmp/firecracker-file-vs-nbd"))
RESULTS_FILE = Path(os.environ.get("FC_FILE_NBD_RESULTS_FILE", "docs/benchmarks/firecracker-file-vs-nbd-results.txt"))
DISK_BENCH_BIN = Path(os.environ.get("FC_FILE_NBD_DISK_BENCH_BIN", str(WORK_DIR / "disk-bench")))
NBD_SERVER_BIN = Path(os.environ.get("FC_FILE_NBD_SERVER_BIN", str(WORK_DIR / "local-nbd-file-server")))
DATA_FILE = Path(os.environ.get("FC_FILE_NBD_DATA_FILE", str(WORK_DIR / "bench.dat")))
SIZE_MB = int(os.environ.get("FC_FILE_NBD_SIZE_MB", "256"))
RANDOM_OPS = int(os.environ.get("FC_FILE_NBD_RANDOM_OPS", "4096"))
SEQ_BLOCK_BYTES = int(os.environ.get("FC_FILE_NBD_SEQ_BLOCK_BYTES", str(1024 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("FC_FILE_NBD_RANDOM_BLOCK_BYTES", "4096"))
IO_DEPTHS = [int(v.strip()) for v in os.environ.get("FC_FILE_NBD_IO_DEPTHS", "1").split(",") if v.strip()]
MODES = [m.strip() for m in os.environ.get("FC_FILE_NBD_MODES", "sequential,random").split(",") if m.strip()]
TARGETS = [t.strip() for t in os.environ.get("FC_FILE_NBD_TARGETS", "file,nbd-range,nbd-chunk").split(",") if t.strip()]
DIRECT = os.environ.get("FC_FILE_NBD_DIRECT", "false").lower() in {"1", "true", "yes", "on"}
DROP_CACHES = os.environ.get("FC_FILE_NBD_DROP_CACHES", "true").lower() in {"1", "true", "yes", "on"}
READ_AHEAD_KB = os.environ.get("FC_FILE_NBD_READ_AHEAD_KB", "").strip()
TIMEOUT_SECONDS = int(os.environ.get("FC_FILE_NBD_TIMEOUT_SECONDS", "90"))
VCPU_COUNT = int(os.environ.get("FC_FILE_NBD_VCPU_COUNT", "1"))
MEM_SIZE_MIB = int(os.environ.get("FC_FILE_NBD_MEM_SIZE_MIB", "256"))
NBD_ADDR = os.environ.get("FC_FILE_NBD_ADDR", "127.0.0.1:10910")
NBD_EXPORT = os.environ.get("FC_FILE_NBD_EXPORT", "bench")


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


def now_ms():
    return time.time_ns() // 1_000_000


def extract_result(output):
    match = re.search(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError("DISK_BENCH_RESULT marker not found:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def maybe_build_binaries():
    WORK_DIR.mkdir(parents=True, exist_ok=True)
    if not DISK_BENCH_BIN.exists():
        run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/disk-bench" % q(DISK_BENCH_BIN))
    if not NBD_SERVER_BIN.exists():
        run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/local-nbd-file-server" % q(NBD_SERVER_BIN))


def prepare_data_file():
    size_bytes = SIZE_MB * 1024 * 1024
    DATA_FILE.parent.mkdir(parents=True, exist_ok=True)
    if DATA_FILE.exists() and DATA_FILE.stat().st_size == size_bytes:
        print(f"reusing data file {DATA_FILE}", flush=True)
        return
    DATA_FILE.unlink(missing_ok=True)
    run("dd if=/dev/urandom of=%s bs=1M count=%d status=none" % (q(DATA_FILE), SIZE_MB))
    run("sync", check=False)


def build_initramfs(initramfs_path):
    root = Path(run("mktemp -d %s" % q(str(WORK_DIR / "initramfs.XXXXXX"))).strip())
    try:
        run("install -m 0755 %s %s" % (q(DISK_BENCH_BIN), q(root / "init")))
        run(
            "cd %s && find . -print0 | cpio --null -ov --format=newc 2>/dev/null | gzip -9 > %s"
            % (q(root), q(initramfs_path.resolve()))
        )
    finally:
        run("rm -rf %s" % q(root), check=False)


def drop_caches():
    if not DROP_CACHES:
        return
    run("sync", check=False)
    run("sh -c 'echo 3 > /proc/sys/vm/drop_caches'", check=False)


def find_free_nbd():
    preferred = os.environ.get("FC_FILE_NBD_DEVICE", "").strip()
    if preferred:
        return preferred
    for i in range(0, 32):
        dev = Path(f"/dev/nbd{i}")
        pid = Path(f"/sys/block/nbd{i}/pid")
        if dev.exists() and not pid.exists():
            return str(dev)
    raise RuntimeError("no free /dev/nbdX device found")


def wait_for_server(proc):
    host, port = NBD_ADDR.rsplit(":", 1)
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("NBD server exited early")
        out = run("timeout 1 bash -lc '</dev/tcp/%s/%s' >/dev/null 2>&1; echo $?" % (q(host), q(port)), check=False).strip()
        if out.endswith("0"):
            return
        time.sleep(0.1)
    raise TimeoutError(f"timed out waiting for NBD server at {NBD_ADDR}")


def attach_nbd(device):
    host, port = NBD_ADDR.rsplit(":", 1)
    run("nbd-client %s %s %s -N %s" % (q(host), q(port), q(device), q(NBD_EXPORT)))


def detach_nbd(device):
    if device:
        run("nbd-client -d %s" % q(device), check=False)


def start_nbd_server():
    return start_nbd_server_mode("range")


def start_nbd_server_mode(mode):
    proc = subprocess.Popen(
        [
            str(NBD_SERVER_BIN),
            "-addr", NBD_ADDR,
            "-file", str(DATA_FILE),
            "-export", NBD_EXPORT,
            "-mode", mode,
            "-chunk-size", str(4 * 1024 * 1024),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        preexec_fn=os.setsid,
    )
    wait_for_server(proc)
    return proc


def stop_process(proc):
    if proc and proc.poll() is None:
        os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
            proc.wait(timeout=5)


def measure_firecracker(target, drive_path, mode, io_depth):
    firecracker = ASSET_DIR / "firecracker"
    kernel = ASSET_DIR / "vmlinux"
    if not firecracker.exists() or not kernel.exists():
        raise RuntimeError(f"missing Firecracker assets under {ASSET_DIR}")
    if not Path("/dev/kvm").exists():
        raise RuntimeError("/dev/kvm is missing")

    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    run_dir = Path(run("mktemp -d %s" % q(str(WORK_DIR / "fc.XXXXXX"))).strip())
    initramfs = run_dir / "initramfs.cpio.gz"
    serial_log = run_dir / "serial.log"
    firecracker_log = run_dir / "firecracker.log"
    socket_path = run_dir / "firecracker.sock"
    config_path = run_dir / "firecracker.json"
    build_initramfs(initramfs)

    boot_args = (
        "console=ttyS0 reboot=k panic=1 pci=off init=/init "
        f"-kind firecracker-{target}-{mode}-qd{io_depth} "
        f"-mode {mode} -path /dev/vda -size-bytes {SIZE_MB * 1024 * 1024} "
        f"-block-bytes {block_bytes} -random-ops {RANDOM_OPS} -io-depth {io_depth}"
    )
    if READ_AHEAD_KB:
        boot_args += f" -read-ahead-kb {READ_AHEAD_KB}"
    if DIRECT:
        boot_args += " -direct"

    config = {
        "boot-source": {
            "kernel_image_path": str(kernel.resolve()),
            "initrd_path": str(initramfs.resolve()),
            "boot_args": boot_args,
        },
        "drives": [{
            "drive_id": "bench",
            "path_on_host": str(Path(drive_path).resolve()) if not str(drive_path).startswith("/dev/") else str(drive_path),
            "is_root_device": False,
            "is_read_only": True,
        }],
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
    config_path.write_text(json.dumps(config, indent=2), encoding="utf-8")

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
                result["target"] = target
                result["wall_ms"] = now_ms() - start
                result["work_dir"] = str(run_dir)
                return result
            if proc.poll() is not None:
                raise RuntimeError(f"Firecracker exited before result:\n{output[-4000:]}")
            time.sleep(0.05)
        raise TimeoutError(f"timed out waiting for result; serial_log={serial_log}")
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)


def append_results(rows, nbd_device):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    lines = [
        "",
        f"firecracker-file-vs-nbd run {started}",
        f"data_file={DATA_FILE}",
        f"nbd_device={nbd_device}",
        f"size_mb={SIZE_MB} random_ops={RANDOM_OPS} io_depths={','.join(map(str, IO_DEPTHS))} direct={str(DIRECT).lower()} drop_caches={str(DROP_CACHES).lower()} read_ahead_kb={READ_AHEAD_KB or 'default'}",
        "",
        "target\tmode\tio_depth\tmb_per_sec\tiops\tduration_ms\twall_ms\tblock_bytes\tops\tbytes_read\tchecksum",
    ]
    for row in rows:
        lines.append(
            "{target}\t{mode}\t{io_depth}\t{mb_per_sec}\t{iops}\t{duration_ms}\t{wall_ms}\t{block_bytes}\t{ops}\t{bytes_read}\t{checksum}".format(**row)
        )
    with RESULTS_FILE.open("a", encoding="utf-8") as f:
        for line in lines:
            f.write(line.rstrip() + "\n")


def main():
    maybe_build_binaries()
    prepare_data_file()

    nbd_device = find_free_nbd()
    detach_nbd(nbd_device)
    rows = []
    for mode in MODES:
        for io_depth in IO_DEPTHS:
            for target in TARGETS:
                nbd_server = None
                drive_path = DATA_FILE
                try:
                    if target in {"nbd", "nbd-range", "nbd-chunk"}:
                        nbd_mode = "chunk" if target == "nbd-chunk" else "range"
                        detach_nbd(nbd_device)
                        nbd_server = start_nbd_server_mode(nbd_mode)
                        attach_nbd(nbd_device)
                        drive_path = nbd_device
                    print(f"running Firecracker target={target} mode={mode} io_depth={io_depth}", flush=True)
                    row = measure_firecracker(target, drive_path, mode, io_depth)
                    rows.append(row)
                    print(json.dumps(row, sort_keys=True), flush=True)
                finally:
                    if nbd_server is not None:
                        detach_nbd(nbd_device)
                        stop_process(nbd_server)
    append_results(rows, nbd_device)
    print(f"wrote {RESULTS_FILE}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print(f"error: {err}", file=sys.stderr)
        sys.exit(1)

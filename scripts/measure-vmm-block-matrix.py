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


WORK_DIR = Path(os.environ.get("VMM_MATRIX_WORK_DIR", ".tmp/vmm-block-matrix"))
RESULTS_FILE = Path(os.environ.get("VMM_MATRIX_RESULTS_FILE", "docs/benchmarks/vmm-block-matrix-results.txt"))
DATA_FILE = Path(os.environ.get("VMM_MATRIX_DATA_FILE", ".tmp/firecracker-file-vs-nbd/bench.dat"))
DISK_BENCH_BIN = Path(os.environ.get("VMM_MATRIX_DISK_BENCH_BIN", ".tmp/firecracker-file-vs-nbd/disk-bench"))
NBD_SERVER_BIN = Path(os.environ.get("VMM_MATRIX_NBD_SERVER_BIN", ".tmp/firecracker-file-vs-nbd/local-nbd-file-server"))
FIRECRACKER_BIN = Path(os.environ.get("FIRECRACKER_BIN", "firecracker-assets/firecracker"))
FIRECRACKER_KERNEL = Path(os.environ.get("FIRECRACKER_KERNEL", "firecracker-assets/vmlinux"))
QEMU_KERNEL = Path(os.environ.get("QEMU_KERNEL", "/boot/vmlinuz-" + os.uname().release))
CLOUD_HYPERVISOR_BIN = Path(os.environ.get("CLOUD_HYPERVISOR_BIN", "/root/orca-bench/bin/cloud-hypervisor"))
DOCKER_IMAGE = os.environ.get("VMM_MATRIX_DOCKER_IMAGE", "alpine:3.22")
RUNTIMES = [v.strip() for v in os.environ.get("VMM_MATRIX_RUNTIMES", "docker,firecracker,cloud-hypervisor,qemu-q35").split(",") if v.strip()]
TARGETS = [v.strip() for v in os.environ.get("VMM_MATRIX_TARGETS", "file,nbd").split(",") if v.strip()]
MODES = [v.strip() for v in os.environ.get("VMM_MATRIX_MODES", "sequential,random").split(",") if v.strip()]
SIZE_BYTES = int(os.environ.get("VMM_MATRIX_SIZE_BYTES", str(256 * 1024 * 1024)))
RANDOM_OPS = int(os.environ.get("VMM_MATRIX_RANDOM_OPS", "4096"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "1"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "256"))
TIMEOUT_SECONDS = int(os.environ.get("VMM_MATRIX_TIMEOUT_SECONDS", "90"))
NBD_ADDR = os.environ.get("VMM_MATRIX_NBD_ADDR", "127.0.0.1:10940")
NBD_EXPORT = os.environ.get("VMM_MATRIX_NBD_EXPORT", "bench")


def run(cmd, check=True):
    print("$ %s" % cmd, flush=True)
    return subprocess.run(cmd, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=check).stdout


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def require(path, label):
    if not path.exists():
        raise RuntimeError("missing %s: %s" % (label, path))


def block_bytes(mode):
    return 1024 * 1024 if mode == "sequential" else 4096


def extract_result(output):
    match = re.search(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError("DISK_BENCH_RESULT marker not found:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def wait_result_from_file(proc, log_path, start_ms):
    deadline = time.time() + TIMEOUT_SECONDS
    output = ""
    while time.time() < deadline:
        if log_path.exists():
            output = log_path.read_text(errors="replace")
        if re.search(r"DISK_BENCH_RESULT=\{[^\r\n]*\}", output):
            result = extract_result(output)
            result["wall_ms"] = now_ms() - start_ms
            return result
        if proc.poll() is not None:
            raise RuntimeError("process exited before result:\n%s" % output[-4000:])
        time.sleep(0.05)
    raise TimeoutError("timed out waiting for DISK_BENCH_RESULT in %s" % log_path)


def stop_process(proc):
    if not proc or proc.poll() is not None:
        return
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
    except Exception:
        proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except Exception:
            proc.kill()
        proc.wait(timeout=5)


def build_initramfs():
    initramfs = WORK_DIR / "disk-bench-initramfs.cpio.gz"
    root = Path(run("mktemp -d %s" % q(str(WORK_DIR / "initramfs.XXXXXX"))).strip())
    try:
        run("install -m 0755 %s %s" % (q(DISK_BENCH_BIN), q(root / "init")))
        run("cd %s && find . -print0 | cpio --null -ov --format=newc 2>/dev/null | gzip -9 > %s" % (q(root), q(initramfs.resolve())))
    finally:
        run("rm -rf %s" % q(root), check=False)
    return initramfs


def drop_caches():
    run("sync", check=False)
    run("sh -c 'echo 3 > /proc/sys/vm/drop_caches'", check=False)


def find_free_nbd():
    preferred = os.environ.get("VMM_MATRIX_NBD_DEVICE", "").strip()
    if preferred:
        return preferred
    for i in range(0, 32):
        dev = Path("/dev/nbd%d" % i)
        pid = Path("/sys/block/nbd%d/pid" % i)
        if dev.exists() and not pid.exists():
            return str(dev)
    raise RuntimeError("no free /dev/nbdX device found")


def wait_for_nbd_server(proc):
    host, port = NBD_ADDR.rsplit(":", 1)
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("NBD server exited early")
        out = run("timeout 1 bash -lc '</dev/tcp/%s/%s' >/dev/null 2>&1; echo $?" % (q(host), q(port)), check=False).strip()
        if out.endswith("0"):
            return
        time.sleep(0.1)
    raise TimeoutError("timed out waiting for NBD server")


def detach_nbd(device):
    run("nbd-client -d %s" % q(device), check=False)


def attach_nbd(device):
    host, port = NBD_ADDR.rsplit(":", 1)
    run("nbd-client %s %s %s -N %s" % (q(host), q(port), q(device), q(NBD_EXPORT)))


def start_nbd(device):
    detach_nbd(device)
    proc = subprocess.Popen(
        [
            str(NBD_SERVER_BIN),
            "-addr", NBD_ADDR,
            "-file", str(DATA_FILE),
            "-export", NBD_EXPORT,
            "-mode", "range",
            "-chunk-size", str(4 * 1024 * 1024),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        preexec_fn=os.setsid,
    )
    wait_for_nbd_server(proc)
    attach_nbd(device)
    return proc


def bench_args(runtime, target, mode, path):
    args = [
        "-kind", "%s-%s-%s" % (runtime, target, mode),
        "-mode", mode,
        "-path", path,
        "-size-bytes", str(SIZE_BYTES),
        "-block-bytes", str(block_bytes(mode)),
        "-random-ops", str(RANDOM_OPS),
        "-io-depth", "1",
    ]
    if runtime != "docker":
        args += ["-hang-after-result"]
    return args


def measure_docker(target, mode, path):
    log_path = WORK_DIR / ("docker-%s-%s.log" % (target, mode))
    args = ["docker", "run", "--rm", "--entrypoint", "/disk-bench", "-v", "%s:/disk-bench:ro" % DISK_BENCH_BIN.resolve()]
    if target == "file":
        args += ["-v", "%s:/bench.dat:ro" % DATA_FILE.resolve()]
    else:
        args += ["--device", "%s:/bench.dat" % path]
    args += [DOCKER_IMAGE] + bench_args("docker", target, mode, "/bench.dat")
    drop_caches()
    start = now_ms()
    proc = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    log_path.write_text(proc.stdout)
    result = extract_result(proc.stdout)
    result["wall_ms"] = now_ms() - start
    return result


def measure_firecracker(target, mode, path, initramfs):
    require(FIRECRACKER_BIN, "Firecracker binary")
    require(FIRECRACKER_KERNEL, "Firecracker kernel")
    run_dir = Path(run("mktemp -d %s" % q(str(WORK_DIR / "firecracker.XXXXXX"))).strip())
    serial_log = run_dir / "serial.log"
    fc_log = run_dir / "firecracker.log"
    socket_path = run_dir / "firecracker.sock"
    config_path = run_dir / "firecracker.json"
    config = {
        "boot-source": {
            "kernel_image_path": str(FIRECRACKER_KERNEL.resolve()),
            "initrd_path": str(initramfs.resolve()),
            "boot_args": "console=ttyS0 reboot=k panic=1 pci=off init=/init " + " ".join(bench_args("firecracker", target, mode, "/dev/vda")),
        },
        "drives": [{"drive_id": "bench", "path_on_host": str(Path(path).resolve()) if not str(path).startswith("/dev/") else str(path), "is_root_device": False, "is_read_only": True}],
        "machine-config": {"vcpu_count": VCPU_COUNT, "mem_size_mib": MEM_SIZE_MIB, "track_dirty_pages": False},
        "logger": {"log_path": str(fc_log), "level": "Info", "show_level": True, "show_log_origin": True},
    }
    config_path.write_text(json.dumps(config, indent=2))
    drop_caches()
    start = now_ms()
    with serial_log.open("wb") as serial, fc_log.open("ab") as err:
        proc = subprocess.Popen([str(FIRECRACKER_BIN.resolve()), "--api-sock", str(socket_path), "--config-file", str(config_path)], stdout=serial, stderr=err, preexec_fn=os.setsid)
    try:
        return wait_result_from_file(proc, serial_log, start)
    finally:
        stop_process(proc)


def measure_qemu_q35(target, mode, path, initramfs):
    log_path = WORK_DIR / ("qemu-q35-%s-%s.log" % (target, mode))
    args = [
        "qemu-system-x86_64", "-enable-kvm", "-M", "q35,accel=kvm", "-cpu", "host",
        "-smp", str(VCPU_COUNT), "-m", "%dM" % MEM_SIZE_MIB,
        "-nodefaults", "-no-user-config", "-nographic", "-no-reboot", "-serial", "stdio",
        "-kernel", str(QEMU_KERNEL), "-initrd", str(initramfs),
        "-append", "console=ttyS0 quiet loglevel=0 reboot=t panic=-1 init=/init " + " ".join(bench_args("qemu-q35", target, mode, "/dev/vda")),
        "-drive", "file=%s,format=raw,if=none,id=bench,readonly=on" % path,
        "-device", "virtio-blk-pci,drive=bench",
    ]
    drop_caches()
    start = now_ms()
    with log_path.open("wb") as log:
        proc = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, preexec_fn=os.setsid)
    try:
        return wait_result_from_file(proc, log_path, start)
    finally:
        stop_process(proc)


def measure_cloud(target, mode, path, initramfs):
    require(CLOUD_HYPERVISOR_BIN, "Cloud Hypervisor binary")
    log_path = WORK_DIR / ("cloud-hypervisor-%s-%s.log" % (target, mode))
    args = [
        str(CLOUD_HYPERVISOR_BIN),
        "--kernel", str(FIRECRACKER_KERNEL),
        "--initramfs", str(initramfs),
        "--cmdline", "console=hvc0 reboot=k panic=1 init=/init " + " ".join(bench_args("cloud-hypervisor", target, mode, "/dev/vda")),
        "--disk", "path=%s,readonly=on,image_type=raw" % path,
        "--cpus", "boot=%d" % VCPU_COUNT,
        "--memory", "size=%dM" % MEM_SIZE_MIB,
        "--console", "tty",
        "--serial", "off",
    ]
    drop_caches()
    start = now_ms()
    with log_path.open("wb") as log:
        proc = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, preexec_fn=os.setsid)
    try:
        return wait_result_from_file(proc, log_path, start)
    finally:
        stop_process(proc)


def measure(runtime, target, mode, path, initramfs):
    print("measuring runtime=%s target=%s mode=%s" % (runtime, target, mode), flush=True)
    if runtime == "docker":
        return measure_docker(target, mode, path)
    if runtime == "firecracker":
        return measure_firecracker(target, mode, path, initramfs)
    if runtime == "qemu-q35":
        return measure_qemu_q35(target, mode, path, initramfs)
    if runtime == "cloud-hypervisor":
        return measure_cloud(target, mode, path, initramfs)
    raise RuntimeError("unsupported runtime %s" % runtime)


def write_results(rows):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        "VMM Block Matrix Results",
        "date=%s" % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host=%s" % run("hostname", check=False).strip(),
        "data_file=%s" % DATA_FILE,
        "size_bytes=%d" % SIZE_BYTES,
        "random_ops=%d" % RANDOM_OPS,
        "seq_block_bytes=1048576",
        "random_block_bytes=4096",
        "vcpus=%d memory_mib=%d" % (VCPU_COUNT, MEM_SIZE_MIB),
        "",
        "runtime\ttarget\tmode\tmb_per_sec\tiops\tduration_ms\twall_ms\tblock_bytes\tops\tbytes_read",
    ]
    for row in rows:
        lines.append("%(runtime)s\t%(target)s\t%(mode)s\t%(mb_per_sec)s\t%(iops)s\t%(duration_ms)s\t%(wall_ms)s\t%(block_bytes)s\t%(ops)s\t%(bytes_read)s" % row)
    RESULTS_FILE.write_text("\n".join(lines) + "\n")
    print("wrote %s" % RESULTS_FILE, flush=True)


def main():
    WORK_DIR.mkdir(parents=True, exist_ok=True)
    require(DATA_FILE, "data file")
    require(DISK_BENCH_BIN, "disk-bench binary")
    require(NBD_SERVER_BIN, "local NBD file server")
    initramfs = build_initramfs()
    nbd_device = find_free_nbd()
    rows = []
    for runtime in RUNTIMES:
        for target in TARGETS:
            for mode in MODES:
                nbd_proc = None
                path = str(DATA_FILE)
                try:
                    if target == "nbd":
                        nbd_proc = start_nbd(nbd_device)
                        path = nbd_device
                    row = measure(runtime, target, mode, path, initramfs)
                    row["runtime"] = runtime
                    row["target"] = target
                    rows.append(row)
                    print(json.dumps(row, sort_keys=True), flush=True)
                finally:
                    if nbd_proc is not None:
                        detach_nbd(nbd_device)
                        stop_process(nbd_proc)
    write_results(rows)


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

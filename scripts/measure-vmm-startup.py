#!/usr/bin/env python3
import base64
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import time
from pathlib import Path


IMAGE = os.environ.get("STARTUP_IMAGE", "alpine:3.22")
COMMAND = os.environ.get("STARTUP_COMMAND", "echo ORCA_STARTUP_MARKER")
MARKER = os.environ.get("STARTUP_MARKER", "ORCA_STARTUP_MARKER")
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/vmm-startup"))
ASSET_DIR = Path(os.environ.get("ASSET_DIR", "firecracker-assets"))
ROOTFS_SIZE_MB = int(os.environ.get("ROOTFS_SIZE_MB", "512"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "256"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "1"))
TIMEOUT_SECONDS = int(os.environ.get("STARTUP_TIMEOUT_SECONDS", "30"))
RUNTIMES = [r.strip() for r in os.environ.get("STARTUP_RUNTIMES", "docker,firecracker,qemu-microvm,qemu-q35,cloud-hypervisor").split(",") if r.strip()]
ORCA_INIT_BIN = Path(os.environ.get("ORCA_INIT_BIN", "bin/orca-init"))
FIRECRACKER_BIN = Path(os.environ.get("FIRECRACKER_BIN", str(ASSET_DIR / "firecracker")))
FIRECRACKER_KERNEL = Path(os.environ.get("FIRECRACKER_KERNEL", str(ASSET_DIR / "vmlinux")))
QEMU_KERNEL = Path(os.environ.get("QEMU_KERNEL", "/boot/vmlinuz-" + os.uname().release))
CLOUD_HYPERVISOR_BIN = Path(os.environ.get("CLOUD_HYPERVISOR_BIN", "bin/cloud-hypervisor"))
FORCE_ROOTFS = os.environ.get("FORCE_ROOTFS", "false").lower() in {"1", "true", "yes", "on"}


def now_ms():
    return time.time_ns() // 1_000_000


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


def b64(value):
    return base64.b64encode(value.encode()).decode()


def safe_name(value):
    return re.sub(r"[^A-Za-z0-9._-]+", "-", value.replace("/", "-").replace(":", "-")).strip("-")


def require(path, label):
    if not path.exists():
        raise RuntimeError("missing %s: %s" % (label, path))


def image_inspect():
    run("docker pull %s" % q(IMAGE))
    raw = run("docker image inspect %s" % q(IMAGE))
    image = json.loads(raw)[0]
    cfg = image.get("Config") or {}
    return {
        "id": image.get("Id", ""),
        "env": cfg.get("Env") or [],
        "user": cfg.get("User") or "",
        "workdir": cfg.get("WorkingDir") or "",
    }, raw


def rootfs_meta_path(cache_dir):
    return cache_dir / "meta.json"


def ensure_rootfs(image_cfg, inspect_raw):
    cache_dir = WORK_PARENT / "rootfs-cache" / safe_name(IMAGE)
    cache_dir.mkdir(parents=True, exist_ok=True)
    rootfs = cache_dir / "rootfs.ext4"
    meta_path = rootfs_meta_path(cache_dir)
    desired = {
        "image": IMAGE,
        "image_id": image_cfg["id"],
        "rootfs_size_mb": ROOTFS_SIZE_MB,
        "orca_init_size": ORCA_INIT_BIN.stat().st_size,
        "orca_init_mtime_ns": ORCA_INIT_BIN.stat().st_mtime_ns,
    }
    if not FORCE_ROOTFS and rootfs.exists() and meta_path.exists():
        try:
            if json.loads(meta_path.read_text()) == desired:
                print("reusing rootfs %s" % rootfs, flush=True)
                return rootfs, cache_dir
        except Exception:
            pass

    print("building rootfs for %s at %s" % (IMAGE, rootfs), flush=True)
    tmp = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "rootfs-build.XXXXXX"))).strip())
    mount_dir = tmp / "mnt"
    tar_path = tmp / "rootfs.tar"
    cid = ""
    try:
        cid = run("docker create --entrypoint /bin/sh %s -c true" % q(IMAGE)).strip()
        run("docker export %s > %s" % (q(cid), q(tar_path)))
        run("docker rm -f %s >/dev/null" % q(cid), check=False)
        cid = ""

        rootfs.unlink(missing_ok=True)
        run("truncate -s %dM %s" % (ROOTFS_SIZE_MB, q(rootfs)))
        run("mkfs.ext4 -F %s >/dev/null" % q(rootfs))
        mount_dir.mkdir(parents=True, exist_ok=True)
        run("mount -o loop %s %s" % (q(rootfs), q(mount_dir)))
        try:
            run("tar --numeric-owner -xf %s -C %s" % (q(tar_path), q(mount_dir)))
            run("mkdir -p %s" % q(mount_dir / "etc"))
            (tmp / "image-inspect.json").write_text(inspect_raw)
            run("install -m 0644 %s %s" % (q(tmp / "image-inspect.json"), q(mount_dir / "etc/orca-image-inspect.json")))
            run("printf %s > %s" % (q(IMAGE + "\n"), q(mount_dir / "etc/orca-image-ref")))
            run("mkdir -p %s" % q(mount_dir / "dev"))
            run("mkdir -p %s" % q(mount_dir / "proc"))
            run("mkdir -p %s" % q(mount_dir / "sys"))
            run("mkdir -p %s" % q(mount_dir / "run"))
            run("mkdir -p %s" % q(mount_dir / "tmp"))
            run("chmod 1777 %s" % q(mount_dir / "tmp"))
            run("install -m 0755 %s %s" % (q(ORCA_INIT_BIN), q(mount_dir / "init")))
            run("sync")
        finally:
            run("umount %s" % q(mount_dir), check=False)
        meta_path.write_text(json.dumps(desired, indent=2, sort_keys=True))
        return rootfs, cache_dir
    finally:
        if cid:
            run("docker rm -f %s >/dev/null" % q(cid), check=False)
        if mount_dir.exists():
            run("umount %s" % q(mount_dir), check=False)
        run("rm -rf %s" % q(tmp), check=False)


def copy_rootfs(src, run_dir):
    dst = run_dir / "rootfs.ext4"
    try:
        run("cp --sparse=always --reflink=auto %s %s" % (q(src), q(dst)))
    except subprocess.CalledProcessError:
        shutil.copyfile(src, dst)
    return dst


def boot_args(console, image_cfg, extra=""):
    env_arg = b64("\n".join(image_cfg["env"]))
    user_arg = b64(image_cfg["user"])
    workdir = ""
    if image_cfg["workdir"]:
        workdir = " orca.workdir_b64=" + b64(image_cfg["workdir"])
    return (
        "root=/dev/vda rw rootwait console=%s quiet loglevel=0 reboot=t panic=-1 init=/init "
        "orca.command_b64=%s orca.env_b64=%s orca.user_b64=%s%s%s"
        % (console, b64(COMMAND), env_arg, user_arg, workdir, extra)
    )


def wait_for_marker(process, log_path, start_ms, timeout_seconds):
    deadline = time.time() + timeout_seconds
    first_output_ms = None
    while time.time() < deadline:
        logs = log_path.read_text(errors="replace") if log_path.exists() else ""
        if first_output_ms is None and ("orca-stdout:" in logs or MARKER in logs):
            first_output_ms = now_ms() - start_ms
        if MARKER in logs:
            return logs, now_ms() - start_ms, first_output_ms
        if process.poll() is not None:
            logs = log_path.read_text(errors="replace") if log_path.exists() else ""
            if MARKER in logs:
                return logs, now_ms() - start_ms, first_output_ms
            raise RuntimeError("process exited before marker:\n%s" % logs[-4000:])
        time.sleep(0.01)
    raise TimeoutError("timed out waiting for %s; log=%s" % (MARKER, log_path))


def stop_process(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)


def measure_docker(out_dir):
    log_path = out_dir / "docker.log"
    start = now_ms()
    with log_path.open("wb") as log:
        process = subprocess.Popen(
            ["docker", "run", "--rm", "--entrypoint", "/bin/sh", IMAGE, "-lc", COMMAND],
            stdout=log,
            stderr=subprocess.STDOUT,
        )
    try:
        logs, elapsed, first_output = wait_for_marker(process, log_path, start, TIMEOUT_SECONDS)
        process.wait(timeout=5)
        return result("docker", elapsed, first_output, log_path, logs)
    finally:
        stop_process(process)


def measure_firecracker(rootfs_src, image_cfg, out_dir):
    require(FIRECRACKER_BIN, "Firecracker binary")
    require(FIRECRACKER_KERNEL, "Firecracker kernel")
    run_dir = out_dir / "firecracker"
    run_dir.mkdir(parents=True, exist_ok=True)
    rootfs = copy_rootfs(rootfs_src, run_dir)
    serial_log = run_dir / "serial.log"
    fc_log = run_dir / "firecracker.log"
    config_path = run_dir / "firecracker.json"
    socket_path = run_dir / "firecracker.sock"
    config = {
        "boot-source": {
            "kernel_image_path": str(FIRECRACKER_KERNEL.resolve()),
            "boot_args": boot_args("ttyS0", image_cfg, " pci=off"),
        },
        "drives": [{"drive_id": "rootfs", "path_on_host": str(rootfs.resolve()), "is_root_device": True, "is_read_only": False}],
        "machine-config": {"vcpu_count": VCPU_COUNT, "mem_size_mib": MEM_SIZE_MIB, "track_dirty_pages": False},
        "logger": {"log_path": str(fc_log), "level": "Info", "show_level": True, "show_log_origin": True},
    }
    config_path.write_text(json.dumps(config, indent=2))
    start = now_ms()
    with serial_log.open("wb") as serial, fc_log.open("ab") as err:
        process = subprocess.Popen([str(FIRECRACKER_BIN.resolve()), "--api-sock", str(socket_path), "--config-file", str(config_path)], stdout=serial, stderr=err)
    try:
        logs, elapsed, first_output = wait_for_marker(process, serial_log, start, TIMEOUT_SECONDS)
        return result("firecracker", elapsed, first_output, serial_log, logs)
    finally:
        stop_process(process)


def measure_qemu(machine, rootfs_src, image_cfg, out_dir):
    require(QEMU_KERNEL, "QEMU kernel")
    run_dir = out_dir / machine
    run_dir.mkdir(parents=True, exist_ok=True)
    rootfs = copy_rootfs(rootfs_src, run_dir)
    log_path = run_dir / "serial.log"
    if machine == "qemu-microvm":
        machine_args = ["-M", "microvm,acpi=off,pcie=off,isa-serial=on,x-option-roms=off", "-device", "virtio-blk-device,drive=rootfs"]
    elif machine == "qemu-q35":
        machine_args = ["-M", "q35,accel=kvm", "-device", "virtio-blk-pci,drive=rootfs"]
    else:
        raise RuntimeError("unsupported qemu machine %s" % machine)
    args = [
        "qemu-system-x86_64",
        "-enable-kvm",
    ] + machine_args[:2] + [
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
        str(QEMU_KERNEL),
        "-append",
        boot_args("ttyS0", image_cfg),
        "-drive",
        "file=%s,format=raw,if=none,id=rootfs" % rootfs,
    ] + machine_args[2:]
    start = now_ms()
    with log_path.open("wb") as log:
        process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT)
    try:
        logs, elapsed, first_output = wait_for_marker(process, log_path, start, TIMEOUT_SECONDS)
        return result(machine, elapsed, first_output, log_path, logs)
    finally:
        stop_process(process)


def measure_cloud(rootfs_src, image_cfg, out_dir):
    require(CLOUD_HYPERVISOR_BIN, "Cloud Hypervisor binary")
    require(FIRECRACKER_KERNEL, "Cloud Hypervisor kernel")
    run_dir = out_dir / "cloud-hypervisor"
    run_dir.mkdir(parents=True, exist_ok=True)
    rootfs = copy_rootfs(rootfs_src, run_dir)
    log_path = run_dir / "serial.log"
    args = [
        str(CLOUD_HYPERVISOR_BIN),
        "--kernel",
        str(FIRECRACKER_KERNEL),
        "--cmdline",
        boot_args("hvc0", image_cfg),
        "--disk",
        "path=%s,readonly=off" % rootfs,
        "--cpus",
        "boot=%d" % VCPU_COUNT,
        "--memory",
        "size=%dM" % MEM_SIZE_MIB,
        "--console",
        "tty",
        "--serial",
        "off",
    ]
    start = now_ms()
    with log_path.open("wb") as log:
        process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT)
    try:
        logs, elapsed, first_output = wait_for_marker(process, log_path, start, TIMEOUT_SECONDS)
        return result("cloud-hypervisor", elapsed, first_output, log_path, logs)
    finally:
        stop_process(process)


def result(kind, elapsed_ms, first_output_ms, log_path, logs):
    return {
        "kind": kind,
        "elapsed_to_marker_ms": elapsed_ms,
        "first_output_ms": first_output_ms,
        "log_path": str(log_path),
        "marker": MARKER,
        "last_orca_lines": [line for line in logs.splitlines() if "orca-" in line][-12:],
    }


def print_result(item):
    if item.get("error"):
        print("[%-16s] error=%s log=%s" % (item["kind"], item["error"], item.get("log_path", "")))
        return
    print(
        "[%-16s] marker_ms=%5s first_output_ms=%5s log=%s"
        % (item["kind"], item["elapsed_to_marker_ms"], item.get("first_output_ms"), item["log_path"])
    )


def main():
    WORK_PARENT.mkdir(parents=True, exist_ok=True)
    require(ORCA_INIT_BIN, "orca-init binary")
    image_cfg, inspect_raw = image_inspect()
    rootfs, _ = ensure_rootfs(image_cfg, inspect_raw)
    run_id = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    out_dir = WORK_PARENT / ("startup-%s" % run_id)
    out_dir.mkdir(parents=True, exist_ok=True)
    print("image=%s" % IMAGE)
    print("command=%s" % COMMAND)
    print("marker=%s" % MARKER)
    print("rootfs=%s" % rootfs)
    print("out_dir=%s" % out_dir)
    results = []
    for runtime in RUNTIMES:
        try:
            if runtime == "docker":
                item = measure_docker(out_dir)
            elif runtime == "firecracker":
                item = measure_firecracker(rootfs, image_cfg, out_dir)
            elif runtime in {"qemu-microvm", "qemu-q35"}:
                item = measure_qemu(runtime, rootfs, image_cfg, out_dir)
            elif runtime == "cloud-hypervisor":
                item = measure_cloud(rootfs, image_cfg, out_dir)
            else:
                raise RuntimeError("unknown runtime %s" % runtime)
        except Exception as err:
            item = {"kind": runtime, "error": str(err)}
        results.append(item)
        print_result(item)
    summary = {"image": IMAGE, "command": COMMAND, "marker": MARKER, "rootfs": str(rootfs), "out_dir": str(out_dir), "results": results}
    (out_dir / "results.json").write_text(json.dumps(summary, indent=2, sort_keys=True))
    print("SUMMARY_JSON=%s" % json.dumps(summary, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

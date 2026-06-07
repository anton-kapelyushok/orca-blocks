#!/usr/bin/env python3
import importlib.util
import json
import os
import pty
import subprocess
import sys
import time
from pathlib import Path


HELPERS_PATH = Path(__file__).resolve().parent / "measure-jetbrains-workspace-timings.py"
spec = importlib.util.spec_from_file_location("jetbrains_timings", HELPERS_PATH)
helpers = importlib.util.module_from_spec(spec)
sys.modules["jetbrains_timings"] = helpers
spec.loader.exec_module(helpers)


WORKSPACE_MARKERS = [
    "Dock HTTP Api listening",
    "Workspace Server listening",
    "Version: 261.643",
    "Smart Mode: enabled",
    "Published to JetBrains Relay: true",
    helpers.JOIN_MARKER,
]


def now_ms():
    return time.time_ns() // 1_000_000


def read_nonblocking(fd):
    import errno
    import fcntl

    flags = fcntl.fcntl(fd, fcntl.F_GETFL)
    fcntl.fcntl(fd, fcntl.F_SETFL, flags | os.O_NONBLOCK)
    try:
        raw = os.read(fd, 65536)
        return raw.decode(errors="replace")
    except OSError as err:
        if err.errno in (errno.EAGAIN, errno.EWOULDBLOCK, errno.EIO):
            return ""
        raise


def wait_for(log_path, marker, timeout_seconds, output_supplier):
    deadline = time.time() + timeout_seconds
    while time.time() < deadline:
        output_supplier()
        logs = log_path.read_text(errors="replace") if log_path.exists() else ""
        if marker in logs:
            return logs
        time.sleep(0.05)
    raise TimeoutError("timed out waiting for %r" % marker)


def wait_for_workspace(log_path, timeout_seconds, output_supplier, start_ms):
    first_seen = {}
    deadline = time.time() + timeout_seconds
    while time.time() < deadline:
        output_supplier()
        logs = log_path.read_text(errors="replace") if log_path.exists() else ""
        helpers.remember_markers(first_seen, logs, start_ms, WORKSPACE_MARKERS)
        if helpers.JOIN_MARKER in logs:
            return logs, first_seen
        time.sleep(0.05)
    raise TimeoutError("timed out waiting for %r" % helpers.JOIN_MARKER)


def build_firecracker_config(rootfs, run_dir, image_cfg, net):
    firecracker = helpers.ASSET_DIR / "firecracker"
    kernel = helpers.ASSET_DIR / "vmlinux"
    socket_path = run_dir / "firecracker.sock"
    firecracker_log = run_dir / "firecracker.log"
    config_path = run_dir / "firecracker.json"
    workdir_arg = ""
    if image_cfg["workdir"]:
        workdir_arg = " orca.workdir_b64=%s" % helpers.b64(image_cfg["workdir"])
    boot_args = (
        "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init "
        "orca.tty=1 orca.env_b64=%s orca.user_b64=%s%s "
        "orca.net_ip=%s orca.net_gateway=%s orca.net_dns=%s"
    ) % (
        helpers.b64("\n".join(image_cfg["env"])),
        helpers.b64(image_cfg["user"]),
        workdir_arg,
        net["guest_cidr"],
        net["host_ip"],
        helpers.GUEST_DNS,
    )
    config = {
        "boot-source": {
            "kernel_image_path": str(kernel.resolve()),
            "boot_args": boot_args,
        },
        "drives": [
            {
                "drive_id": "rootfs",
                "path_on_host": str(rootfs.resolve()),
                "is_root_device": True,
                "is_read_only": False,
            }
        ],
        "network-interfaces": [
            {
                "iface_id": "eth0",
                "guest_mac": net["guest_mac"],
                "host_dev_name": net["tap"],
            }
        ],
        "machine-config": {
            "vcpu_count": helpers.VCPU_COUNT,
            "mem_size_mib": helpers.MEM_SIZE_MIB,
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
    return firecracker, socket_path, config_path


def main():
    timeout_seconds = int(sys.argv[1]) if len(sys.argv) > 1 else 180
    command = " ".join(sys.argv[2:]) if len(sys.argv) > 2 else helpers.ORCA_COMMAND
    cached_rootfs, image_cfg, init_bin = helpers.ensure_local_rootfs()
    run_dir = Path(
        helpers.run("mktemp -d %s" % helpers.q(str(helpers.WORK_PARENT / "hotpool.XXXXXX"))).strip()
    )
    rootfs = run_dir / "rootfs.ext4"
    serial_log = run_dir / "serial.log"
    print("copying cached rootfs for hotpool run", flush=True)
    helpers.run("cp --sparse=always --reflink=auto %s %s" % (helpers.q(cached_rootfs), helpers.q(rootfs)))
    helpers.sideload_orca_init(rootfs, init_bin)
    net = helpers.setup_tap()
    process = None
    master_fd = None
    output_buf = []

    def collect_output():
        if master_fd is None:
            return ""
        chunk = read_nonblocking(master_fd)
        if chunk:
            output_buf.append(chunk)
            with serial_log.open("a", encoding="utf-8", errors="replace") as f:
                f.write(chunk)
        return chunk

    try:
        firecracker, socket_path, config_path = build_firecracker_config(rootfs, run_dir, image_cfg, net)
        master_fd, slave_fd = pty.openpty()
        start = now_ms()
        process = subprocess.Popen(
            [str(firecracker.resolve()), "--api-sock", str(socket_path), "--config-file", str(config_path)],
            stdin=slave_fd,
            stdout=slave_fd,
            stderr=subprocess.STDOUT,
            close_fds=True,
        )
        os.close(slave_fd)
        wait_for(serial_log, "orca-init: tty ready", timeout_seconds, collect_output)
        ready_ms = now_ms() - start
        start_workspace = now_ms()
        os.write(master_fd, (command + "\n").encode())
        logs, first_seen = wait_for_workspace(serial_log, timeout_seconds, collect_output, start_workspace)
        elapsed_ms = now_ms() - start_workspace
        print("\n[firecracker-hotpool-local]")
        print("work_dir=%s" % run_dir)
        print("serial_log=%s" % serial_log)
        print("ready_ms=%d" % ready_ms)
        print("workspace_start_to_join_ms=%d" % elapsed_ms)
        print("tini_warning=%s" % ("Tini is not running as PID 1" in logs))
        for marker, seen_ms in first_seen.items():
            print("%-40s %7dms" % (marker, seen_ms))
        print(
            "SUMMARY_JSON=%s"
            % json.dumps(
                {
                    "kind": "firecracker-hotpool-local",
                    "ready_ms": ready_ms,
                    "workspace_start_to_join_ms": elapsed_ms,
                    "first_seen_ms": first_seen,
                    "command": command,
                    "tini_warning": "Tini is not running as PID 1" in logs,
                    "work_dir": str(run_dir),
                    "serial_log": str(serial_log),
                    "vcpu_count": helpers.VCPU_COUNT,
                    "mem_size_mib": helpers.MEM_SIZE_MIB,
                },
                sort_keys=True,
            )
        )
    finally:
        if master_fd is not None:
            try:
                os.write(master_fd, b"sync\nexit\n")
            except OSError:
                pass
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        if master_fd is not None:
            try:
                os.close(master_fd)
            except OSError:
                pass
        helpers.cleanup_tap(net)


if __name__ == "__main__":
    main()

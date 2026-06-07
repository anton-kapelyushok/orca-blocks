#!/usr/bin/env python3
import json
import os
import random
import shlex
import subprocess
import sys
import time
import urllib.request
from pathlib import Path


IMAGE = os.environ.get("IMAGE", "registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643")
BASE_IMAGE_ID = os.environ.get("BASE_IMAGE_ID", "base-96128f21-2503-45ef-a73d-5ed7b83ce279")
JOIN_MARKER = "Join this workspace using URL:"
ORCA_COMMAND = "bash -c '$SH_SCRIPT_FOLDER_ENV/start.sh \"$@\"' -- '-- --auth=accept-everyone --publish --enableSmartMode'"
ASSET_DIR = Path(os.environ.get("ASSET_DIR", "firecracker-assets"))
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/jetbrains-workspace-timings"))
ROOTFS_SIZE_MB = int(os.environ.get("ROOTFS_SIZE_MB", "10000"))
MEM_SIZE_MIB = int(os.environ.get("MEM_SIZE_MIB", "4096"))
VCPU_COUNT = int(os.environ.get("VCPU_COUNT", "2"))
GUEST_DNS = os.environ.get("GUEST_DNS", "1.1.1.1")
DOCKER_RUN_ARGS = os.environ.get("DOCKER_RUN_ARGS", "")


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


def http_json(method, url, body=None, timeout=120):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["content-type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def remember_markers(first_seen, logs, start_ms, markers):
    elapsed = now_ms() - start_ms
    for marker in markers:
        if marker not in first_seen and marker in logs:
            first_seen[marker] = elapsed


def measure_docker(timeout_seconds):
    name = "orca-air-workspace-measure-%d" % int(time.time() * 1000)
    run("docker rm -f %s >/dev/null 2>&1 || true" % name, check=False)
    start = now_ms()
    docker_args = (DOCKER_RUN_ARGS.strip() + " ") if DOCKER_RUN_ARGS.strip() else ""
    container_id = run("docker run -d --name %s %s%s" % (name, docker_args, IMAGE)).strip()
    first_seen = {}
    try:
        deadline = time.time() + timeout_seconds
        while time.time() < deadline:
            logs = run("docker logs %s 2>&1" % name, check=False)
            remember_markers(
                first_seen,
                logs,
                start,
                [
                    "Dock HTTP Api listening",
                    "Workspace Server listening",
                    "Version: 261.643",
                    "Smart Mode: enabled",
                    "Published to JetBrains Relay: true",
                    JOIN_MARKER,
                ],
            )
            if JOIN_MARKER in logs:
                return {
                    "kind": "docker",
                    "container": name,
                    "container_id": container_id,
                    "elapsed_ms": now_ms() - start,
                    "first_seen_ms": first_seen,
                    "tini_warning": "Tini is not running as PID 1" in logs,
                }
            time.sleep(1)
        return {
            "kind": "docker",
            "error": "timeout",
            "elapsed_ms": now_ms() - start,
            "first_seen_ms": first_seen,
        }
    finally:
        run("docker rm -f %s >/dev/null 2>&1 || true" % name, check=False)


def image_inspect():
    try:
        raw = run("docker image inspect %s" % q(IMAGE))
    except subprocess.CalledProcessError:
        print("pulling %s" % IMAGE, flush=True)
        run("docker pull %s" % q(IMAGE))
        raw = run("docker image inspect %s" % q(IMAGE))
    image = json.loads(raw)[0]
    cfg = image.get("Config") or {}
    return image, {
        "env": cfg.get("Env") or [],
        "user": cfg.get("User") or "",
        "workdir": cfg.get("WorkingDir") or "",
    }


def build_orca_init(init_bin):
    if not run("command -v go >/dev/null 2>&1", check=False):
        service = os.environ.get("ORCA_INIT_SOURCE_SERVICE", "node-1")
        run("docker compose cp %s:/orca-init %s" % (q(service), q(init_bin)))
        run("chmod 0755 %s" % q(init_bin))
        return
    build_time = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    run(
        "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath "
        "-ldflags %s -o %s ./cmd/orca-init"
        % (q("-s -w -X main.buildTimeUTC=%s" % build_time), q(init_bin))
    )


def ensure_local_rootfs():
    WORK_PARENT.mkdir(parents=True, exist_ok=True)
    cache_dir = WORK_PARENT / "local-firecracker-rootfs"
    cache_dir.mkdir(parents=True, exist_ok=True)
    rootfs = cache_dir / "rootfs.ext4"
    inspect_path = cache_dir / "image-inspect.json"
    meta_path = cache_dir / "rootfs-meta.json"
    init_bin = cache_dir / "orca-init"
    image, image_cfg = image_inspect()
    digest = image.get("Id", "")
    desired_meta = {
        "image": IMAGE,
        "image_digest": digest,
        "rootfs_size_mb": ROOTFS_SIZE_MB,
    }
    current_meta = {}
    if meta_path.exists():
        try:
            current_meta = json.loads(meta_path.read_text())
        except Exception:
            current_meta = {}
    if current_meta != desired_meta or not rootfs.exists():
        print("preparing cached local Firecracker rootfs at %s" % rootfs, flush=True)
        rootfs.unlink(missing_ok=True)
        inspect_path.write_text(json.dumps([image], indent=2))
        run("truncate -s %sM %s" % (ROOTFS_SIZE_MB, q(rootfs)))
        run("mkfs.ext4 -F %s >/dev/null" % q(rootfs))
        mount_dir = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "mnt.XXXXXX"))).strip())
        cid = ""
        mounted = False
        try:
            run("sudo mount -o loop %s %s" % (q(rootfs), q(mount_dir)))
            mounted = True
            cid = run("docker create --entrypoint /bin/sh %s -c true" % q(IMAGE)).strip()
            run("docker export %s | sudo tar --numeric-owner -xf - -C %s" % (q(cid), q(mount_dir)))
            run("sudo mkdir -p %s" % q(mount_dir / "dev"))
            run("sudo mkdir -p %s" % q(mount_dir / "proc"))
            run("sudo mkdir -p %s" % q(mount_dir / "sys"))
            run("sudo mkdir -p %s" % q(mount_dir / "run"))
            run("sudo mkdir -p %s" % q(mount_dir / "tmp"))
            run("sudo mkdir -p %s" % q(mount_dir / "etc"))
            run("sudo mkdir -p %s" % q(mount_dir / "orca"))
            run("sudo install -m 0644 %s %s" % (q(inspect_path), q(mount_dir / "etc/orca-image-inspect.json")))
            run("printf %s | sudo tee %s >/dev/null" % (q(IMAGE + "\n"), q(mount_dir / "etc/orca-image-ref")))
        finally:
            if cid:
                run("docker rm -f %s >/dev/null 2>&1 || true" % q(cid), check=False)
            if mounted:
                run("sudo umount %s" % q(mount_dir), check=False)
            run("rmdir %s >/dev/null 2>&1 || true" % q(mount_dir), check=False)
        meta_path.write_text(json.dumps(desired_meta, indent=2, sort_keys=True))
    else:
        print("reusing cached local Firecracker rootfs at %s" % rootfs, flush=True)

    build_orca_init(init_bin)
    return rootfs, image_cfg, init_bin


def sideload_orca_init(rootfs, init_bin):
    mount_dir = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "mnt.XXXXXX"))).strip())
    mounted = False
    try:
        run("sudo mount -o loop %s %s" % (q(rootfs), q(mount_dir)))
        mounted = True
        run("sudo rm -f %s" % q(mount_dir / "init"))
        run("sudo install -m 0755 %s %s" % (q(init_bin), q(mount_dir / "init")))
        run("sync")
    finally:
        if mounted:
            run("sudo umount %s" % q(mount_dir), check=False)
        run("rmdir %s >/dev/null 2>&1 || true" % q(mount_dir), check=False)


def b64(value):
    import base64

    return base64.b64encode(value.encode()).decode()


def setup_tap():
    third = random.randint(180, 249)
    tap = "tapjb%d" % random.randint(1000, 9999)
    host_ip = "172.31.%d.1" % third
    guest_ip = "172.31.%d.2" % third
    host_cidr = host_ip + "/30"
    guest_cidr = guest_ip + "/30"
    guest_mac = "06:00:ac:1f:%02x:%02x" % (third, random.randint(2, 254))
    run("sudo ip link del %s >/dev/null 2>&1 || true" % q(tap), check=False)
    run("sudo ip tuntap add dev %s mode tap" % q(tap))
    run("sudo ip addr add %s dev %s" % (q(host_cidr), q(tap)))
    run("sudo ip link set dev %s up" % q(tap))
    run("sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null")
    run("sudo iptables -t nat -C POSTROUTING -s %s -j MASQUERADE 2>/dev/null || sudo iptables -t nat -A POSTROUTING -s %s -j MASQUERADE" % (q(guest_cidr), q(guest_cidr)))
    run("sudo iptables -C FORWARD -i %s -j ACCEPT 2>/dev/null || sudo iptables -A FORWARD -i %s -j ACCEPT" % (q(tap), q(tap)))
    run("sudo iptables -C FORWARD -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || sudo iptables -A FORWARD -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" % (q(tap), q(tap)))
    return {
        "tap": tap,
        "host_ip": host_ip,
        "guest_cidr": guest_cidr,
        "guest_mac": guest_mac,
    }


def cleanup_tap(net):
    run("sudo iptables -t nat -D POSTROUTING -s %s -j MASQUERADE >/dev/null 2>&1 || true" % q(net["guest_cidr"]), check=False)
    run("sudo iptables -D FORWARD -i %s -j ACCEPT >/dev/null 2>&1 || true" % q(net["tap"]), check=False)
    run("sudo iptables -D FORWARD -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT >/dev/null 2>&1 || true" % q(net["tap"]), check=False)
    run("sudo ip link del %s >/dev/null 2>&1 || true" % q(net["tap"]), check=False)


def measure_firecracker_local(timeout_seconds):
    firecracker = ASSET_DIR / "firecracker"
    kernel = ASSET_DIR / "vmlinux"
    if not firecracker.exists() or not kernel.exists():
        return {
            "kind": "firecracker-local",
            "error": "missing firecracker assets under %s" % ASSET_DIR,
            "elapsed_ms": 0,
            "first_seen_ms": {},
        }
    if not Path("/dev/kvm").exists():
        return {
            "kind": "firecracker-local",
            "error": "/dev/kvm is missing",
            "elapsed_ms": 0,
            "first_seen_ms": {},
        }
    cached_rootfs, image_cfg, init_bin = ensure_local_rootfs()
    run_dir = Path(run("mktemp -d %s" % q(str(WORK_PARENT / "run.XXXXXX"))).strip())
    rootfs = run_dir / "rootfs.ext4"
    print("copying cached rootfs for local Firecracker run", flush=True)
    run("cp --sparse=always --reflink=auto %s %s" % (q(cached_rootfs), q(rootfs)))
    sideload_orca_init(rootfs, init_bin)
    serial_log = run_dir / "serial.log"
    firecracker_log = run_dir / "firecracker.log"
    config_path = run_dir / "firecracker.json"
    socket_path = run_dir / "firecracker.sock"
    net = setup_tap()
    first_seen = {}
    process = None
    try:
        workdir_arg = ""
        if image_cfg["workdir"]:
            workdir_arg = " orca.workdir_b64=%s" % b64(image_cfg["workdir"])
        boot_args = (
            "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init "
            "orca.tty=1 orca.command_b64=%s orca.env_b64=%s orca.user_b64=%s%s "
            "orca.net_ip=%s orca.net_gateway=%s orca.net_dns=%s"
        ) % (
            b64(ORCA_COMMAND),
            b64("\n".join(image_cfg["env"])),
            b64(image_cfg["user"]),
            workdir_arg,
            net["guest_cidr"],
            net["host_ip"],
            GUEST_DNS,
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
        serial = serial_log.open("wb")
        firecracker_err = firecracker_log.open("ab")
        start = now_ms()
        process = subprocess.Popen(
            [str(firecracker.resolve()), "--api-sock", str(socket_path), "--config-file", str(config_path)],
            stdout=serial,
            stderr=firecracker_err,
        )
        serial.close()
        firecracker_err.close()
        deadline = time.time() + timeout_seconds
        logs = ""
        while time.time() < deadline and process.poll() is None:
            if serial_log.exists():
                logs = serial_log.read_text(errors="replace")
            remember_markers(
                first_seen,
                logs,
                start,
                [
                    "orca-init: build_time_utc",
                    "orca-init: child reaper ready",
                    "orca-init: tty ready",
                    "Dock HTTP Api listening",
                    "Workspace Server listening",
                    "Version: 261.643",
                    "Smart Mode: enabled",
                    "Published to JetBrains Relay: true",
                    JOIN_MARKER,
                ],
            )
            if JOIN_MARKER in logs:
                return {
                    "kind": "firecracker-local",
                    "elapsed_ms": now_ms() - start,
                    "first_seen_ms": first_seen,
                    "work_dir": str(run_dir),
                    "serial_log": str(serial_log),
                    "tini_warning": "Tini is not running as PID 1" in logs,
                }
            time.sleep(1)
        if serial_log.exists():
            logs = serial_log.read_text(errors="replace")
        return {
            "kind": "firecracker-local",
            "error": "timeout" if process.poll() is None else "firecracker exited",
            "elapsed_ms": now_ms() - start,
            "first_seen_ms": first_seen,
            "work_dir": str(run_dir),
            "serial_log": str(serial_log),
            "tini_warning": "Tini is not running as PID 1" in logs,
        }
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        cleanup_tap(net)


def measure_orca(timeout_seconds):
    payload = {
        "base_image_id": BASE_IMAGE_ID,
        "force_node": "node-1",
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEM_SIZE_MIB,
        "tty": True,
        "command": ORCA_COMMAND,
    }
    start = now_ms()
    start_resp = http_json("POST", "http://localhost:18080/startEnv", payload, timeout=120)
    session_id = start_resp["session_id"]
    node_url = start_resp["node_url"]
    first_seen = {}
    try:
        deadline = time.time() + timeout_seconds
        while time.time() < deadline:
            tty = http_json("GET", "%s/sessions/%s/tty/output?offset=0" % (node_url, session_id), timeout=20)
            logs = tty.get("output", "")
            remember_markers(
                first_seen,
                logs,
                start,
                [
                    "orca-init: build_time_utc",
                    "orca-init: child reaper ready",
                    "orca-init: tty ready",
                    "Dock HTTP Api listening",
                    "Workspace Server listening",
                    "Version: 261.643",
                    "Smart Mode: enabled",
                    "Published to JetBrains Relay: true",
                    JOIN_MARKER,
                ],
            )
            if JOIN_MARKER in logs:
                return {
                    "kind": "orca",
                    "env_id": start_resp.get("env_id"),
                    "session_id": session_id,
                    "node_id": start_resp.get("node_id"),
                    "node_url": node_url,
                    "elapsed_ms": now_ms() - start,
                    "first_seen_ms": first_seen,
                    "firecracker_timings": json.loads(start_resp.get("firecracker_timings", "[]")),
                    "tini_warning": "Tini is not running as PID 1" in logs,
                }
            time.sleep(1)
        return {
            "kind": "orca",
            "error": "timeout",
            "elapsed_ms": now_ms() - start,
            "first_seen_ms": first_seen,
            "session_id": session_id,
        }
    finally:
        try:
            http_json("POST", "%s/sessions/%s/tty/stop" % (node_url, session_id), {}, timeout=90)
        except Exception:
            pass


def print_result(result):
    print("\n[%s]" % result["kind"])
    if result.get("error"):
        print("error=%s elapsed_ms=%s" % (result["error"], result["elapsed_ms"]))
    else:
        print("elapsed_to_join_ms=%s" % result["elapsed_ms"])
    print("tini_warning=%s" % result.get("tini_warning", False))
    if result.get("work_dir"):
        print("work_dir=%s" % result["work_dir"])
    if result.get("serial_log"):
        print("serial_log=%s" % result["serial_log"])
    for marker, elapsed in result.get("first_seen_ms", {}).items():
        print("%-40s %7dms" % (marker, elapsed))
    if result["kind"] == "orca":
        print("firecracker_steps:")
        for step in result.get("firecracker_timings", []):
            print("  %-28s %7sms %s" % (step.get("name"), step.get("duration_ms"), step.get("status")))


def usage():
    print(
        "usage: scripts/measure-jetbrains-workspace-timings.py [mode] [timeout_seconds]\n"
        "\n"
        "modes:\n"
        "  docker             measure docker run on the Linux VM host\n"
        "  firecracker-local  measure Firecracker with a local ext4 rootfs file, no NBD\n"
        "  firecracker        alias for firecracker-local\n"
        "  orca               measure Orca Firecracker session through NBD/storage\n"
        "  all                run docker, firecracker-local, then orca\n"
        "\n"
        "default: all 240\n"
        "\n"
        "examples:\n"
        "  scripts/measure-jetbrains-workspace-timings.py docker 120\n"
        "  scripts/measure-jetbrains-workspace-timings.py firecracker-local 180\n"
        "  scripts/measure-jetbrains-workspace-timings.py orca 260\n"
    )


def parse_args(argv):
    mode = "all"
    timeout_seconds = 240
    modes = {"docker", "firecracker-local", "firecracker", "orca", "all"}
    args = list(argv)
    if args and args[0] in {"-h", "--help", "help"}:
        usage()
        raise SystemExit(0)
    if args:
        if args[0] in modes:
            mode = args.pop(0)
        elif args[0].isdigit():
            timeout_seconds = int(args.pop(0))
        else:
            print("unknown mode %r\n" % args[0], file=sys.stderr)
            usage()
            raise SystemExit(2)
    if args:
        if args[0].isdigit():
            timeout_seconds = int(args.pop(0))
        else:
            print("invalid timeout %r\n" % args[0], file=sys.stderr)
            usage()
            raise SystemExit(2)
    if args:
        print("too many arguments: %s\n" % " ".join(args), file=sys.stderr)
        usage()
        raise SystemExit(2)
    if mode == "firecracker":
        mode = "firecracker-local"
    return mode, timeout_seconds


def main():
    mode, timeout_seconds = parse_args(sys.argv[1:])
    results = []
    if mode in {"docker", "all"}:
        print("measuring docker until %r" % JOIN_MARKER, flush=True)
        docker = measure_docker(timeout_seconds)
        results.append(docker)
        print_result(docker)
    if mode in {"firecracker-local", "all"}:
        print("\nmeasuring local Firecracker until %r" % JOIN_MARKER, flush=True)
        firecracker_local = measure_firecracker_local(timeout_seconds)
        results.append(firecracker_local)
        print_result(firecracker_local)
    if mode in {"orca", "all"}:
        print("\nmeasuring orca until %r" % JOIN_MARKER, flush=True)
        orca = measure_orca(timeout_seconds)
        results.append(orca)
        print_result(orca)
    print("\nSUMMARY_JSON=%s" % json.dumps(results, sort_keys=True))


if __name__ == "__main__":
    main()

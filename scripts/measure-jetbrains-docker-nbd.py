#!/usr/bin/env python3
import json
import os
import shlex
import signal
import subprocess
import sys
import time
import urllib.request
from pathlib import Path


IMAGE = os.environ.get("IMAGE", "registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643")
JOIN_MARKER = "Join this workspace using URL:"
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/jetbrains-docker-nbd-timings"))
NBD_SERVER_BIN = Path(os.environ.get("NBD_SERVER_BIN", ".tmp/firecracker-file-vs-nbd/local-nbd-file-server"))
NBD_SIZE_MB = int(os.environ.get("NBD_SIZE_MB", "16384"))
NBD_MODE = os.environ.get("NBD_MODE", "range")
NBD_PORT = int(os.environ.get("NBD_PORT", "12191"))
TIMEOUT_SECONDS = int(os.environ.get("TIMEOUT_SECONDS", "260"))
DOCKERD_WAIT_SECONDS = int(os.environ.get("DOCKERD_WAIT_SECONDS", "40"))


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def run(cmd, check=True, **kwargs):
    print(f"$ {cmd}", flush=True)
    result = subprocess.run(
        cmd,
        shell=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        **kwargs,
    )
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n", flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def ensure_nbd_server():
    if NBD_SERVER_BIN.exists():
        help_text = run("%s -h 2>&1 || true" % q(NBD_SERVER_BIN), check=False)
        if "-stats" in help_text:
            return
        print("rebuilding stale local-nbd-file-server without -stats support", flush=True)
    else:
        NBD_SERVER_BIN.parent.mkdir(parents=True, exist_ok=True)
    run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/local-nbd-file-server" % q(NBD_SERVER_BIN))


def find_free_nbd():
    explicit = os.environ.get("NBD_DEVICE")
    if explicit:
        return explicit
    for path in sorted(Path("/sys/block").glob("nbd*"), key=lambda p: int(p.name[3:])):
        pid_path = path / "pid"
        if not pid_path.exists():
            return "/dev/" + path.name
    raise RuntimeError("no free /dev/nbdX device found")


def wait_for_dockerd(sock):
    deadline = time.time() + DOCKERD_WAIT_SECONDS
    last = ""
    while time.time() < deadline:
        out = run("docker -H unix://%s info >/dev/null 2>&1 && echo ok || true" % q(sock), check=False).strip()
        if out == "ok":
            return
        last = out
        time.sleep(1)
    raise RuntimeError("temporary dockerd did not become ready: %s" % last)


def remember_markers(first_seen, logs, start_ms):
    elapsed = now_ms() - start_ms
    for marker in [
        "Dock HTTP Api listening",
        "Workspace Server listening",
        "Version: 261.643",
        "Smart Mode: enabled",
        "Published to JetBrains Relay: true",
        JOIN_MARKER,
    ]:
        if marker not in first_seen and marker in logs:
            first_seen[marker] = elapsed


def measure_container(sock):
    name = "orca-air-workspace-nbd-%d" % int(time.time() * 1000)
    start = now_ms()
    run("docker -H unix://%s rm -f %s >/dev/null 2>&1 || true" % (q(sock), q(name)), check=False)
    cid = run(
        "docker -H unix://%s run -d --network host --name %s %s"
        % (q(sock), q(name), q(IMAGE))
    ).strip()
    first_seen = {}
    try:
        deadline = time.time() + TIMEOUT_SECONDS
        while time.time() < deadline:
            logs = run("docker -H unix://%s logs %s 2>&1" % (q(sock), q(name)), check=False)
            remember_markers(first_seen, logs, start)
            if JOIN_MARKER in logs:
                return {
                    "kind": "docker-nbd",
                    "container": name,
                    "container_id": cid,
                    "elapsed_ms": now_ms() - start,
                    "first_seen_ms": first_seen,
                    "tini_warning": "Tini is not running as PID 1" in logs,
                }
            time.sleep(1)
        return {
            "kind": "docker-nbd",
            "error": "timeout",
            "elapsed_ms": now_ms() - start,
            "first_seen_ms": first_seen,
        }
    finally:
        run("docker -H unix://%s rm -f %s >/dev/null 2>&1 || true" % (q(sock), q(name)), check=False)


def print_result(result):
    print("\n[%s]" % result["kind"])
    if result.get("error"):
        print("error=%s elapsed_ms=%s" % (result["error"], result["elapsed_ms"]))
    else:
        print("elapsed_to_join_ms=%s" % result["elapsed_ms"])
        print("tini_warning=%s" % result.get("tini_warning"))
    for marker, elapsed in result.get("first_seen_ms", {}).items():
        print("%-40s %7dms" % (marker, elapsed))


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root; this script mounts NBD and starts a temporary dockerd")
    ensure_nbd_server()
    started = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    work = (WORK_PARENT / started).resolve()
    work.mkdir(parents=True, exist_ok=True)

    backing = work / "docker-data-root.img"
    mount_dir = work / "mnt"
    data_root = mount_dir / "data-root"
    containerd_root = mount_dir / "containerd-root"
    exec_root = work / "exec-root"
    containerd_state = work / "containerd-state"
    containerd_sock = work / "containerd.sock"
    sock = work / "docker.sock"
    pidfile = work / "dockerd.pid"
    server_log = open(work / "nbd-server.log", "w", encoding="utf-8")
    containerd_log = open(work / "containerd.log", "w", encoding="utf-8")
    dockerd_log = open(work / "dockerd.log", "w", encoding="utf-8")
    nbd_device = find_free_nbd()
    nbd_proc = None
    containerd_proc = None
    dockerd_proc = None
    mounted = False
    attached = False

    try:
        run("truncate -s %dM %s" % (NBD_SIZE_MB, q(backing)))
        nbd_proc = subprocess.Popen(
            [
                str(NBD_SERVER_BIN),
                "-addr", "127.0.0.1:%d" % NBD_PORT,
                "-file", str(backing),
                "-export", "docker-data-root",
                "-mode", NBD_MODE,
                "-stats", str(work / "nbd-stats.json"),
            ],
            stdout=server_log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        time.sleep(1)
        run("nbd-client 127.0.0.1 %d %s -N docker-data-root" % (NBD_PORT, q(nbd_device)))
        attached = True
        run("mkfs.ext4 -q -F %s" % q(nbd_device))
        mount_dir.mkdir(parents=True, exist_ok=True)
        run("mount %s %s" % (q(nbd_device), q(mount_dir)))
        mounted = True
        data_root.mkdir(parents=True, exist_ok=True)
        containerd_root.mkdir(parents=True, exist_ok=True)
        exec_root.mkdir(parents=True, exist_ok=True)
        containerd_state.mkdir(parents=True, exist_ok=True)

        containerd_cmd = [
            "containerd",
            "--address", str(containerd_sock),
            "--root", str(containerd_root),
            "--state", str(containerd_state),
            "--log-level", "warn",
        ]
        containerd_proc = subprocess.Popen(containerd_cmd, stdout=containerd_log, stderr=subprocess.STDOUT, text=True)
        for _ in range(DOCKERD_WAIT_SECONDS):
            if containerd_sock.exists():
                break
            time.sleep(1)
        if not containerd_sock.exists():
            raise RuntimeError("temporary containerd socket did not appear")

        dockerd_cmd = [
            "dockerd",
            "--host", "unix://%s" % sock,
            "--data-root", str(data_root),
            "--exec-root", str(exec_root),
            "--pidfile", str(pidfile),
            "--containerd", str(containerd_sock),
            "--containerd-namespace", "orca-docker-nbd-" + started,
            "--bridge", "none",
            "--iptables=false",
            "--ip-masq=false",
        ]
        dockerd_proc = subprocess.Popen(dockerd_cmd, stdout=dockerd_log, stderr=subprocess.STDOUT, text=True)
        wait_for_dockerd(sock)

        print("loading image into temporary NBD-backed dockerd", flush=True)
        run("docker image inspect %s >/dev/null" % q(IMAGE))
        run("docker save %s | docker -H unix://%s load" % (q(IMAGE), q(sock)))

        print("measuring docker on NBD-backed data-root until %r" % JOIN_MARKER, flush=True)
        result = measure_container(sock)
        result.update({
            "image": IMAGE,
            "nbd_device": nbd_device,
            "nbd_mode": NBD_MODE,
            "nbd_size_mb": NBD_SIZE_MB,
            "work_dir": str(work),
        })
        print_result(result)
        raw = json.dumps(result, sort_keys=True)
        (work / "result.json").write_text(raw + "\n", encoding="utf-8")
        print("\nSUMMARY_JSON=%s" % raw)
        print("work_dir=%s" % work)
    finally:
        if dockerd_proc and dockerd_proc.poll() is None:
            dockerd_proc.terminate()
            try:
                dockerd_proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                dockerd_proc.kill()
                dockerd_proc.wait(timeout=10)
        if containerd_proc and containerd_proc.poll() is None:
            containerd_proc.terminate()
            try:
                containerd_proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                containerd_proc.kill()
                containerd_proc.wait(timeout=10)
        if mounted:
            run("umount %s" % q(mount_dir), check=False)
        if attached:
            run("nbd-client -d %s" % q(nbd_device), check=False)
        if nbd_proc and nbd_proc.poll() is None:
            nbd_proc.send_signal(signal.SIGTERM)
            try:
                nbd_proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                nbd_proc.kill()
                nbd_proc.wait(timeout=5)
        server_log.close()
        containerd_log.close()
        dockerd_log.close()


if __name__ == "__main__":
    main()

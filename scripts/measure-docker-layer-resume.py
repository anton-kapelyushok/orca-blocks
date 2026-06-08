#!/usr/bin/env python3
import json
import os
import shlex
import subprocess
import time
from datetime import datetime, timezone
from pathlib import Path


IMAGE = os.environ.get("IMAGE", "registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643")
JOIN_MARKER = os.environ.get("MARKER", "Join this workspace using URL:")
START_COMMAND = os.environ.get("START_COMMAND", "")
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/docker-layer-resume"))
TIMEOUT_SECONDS = int(os.environ.get("TIMEOUT_SECONDS", "260"))
DOCKERD_WAIT_SECONDS = int(os.environ.get("DOCKERD_WAIT_SECONDS", "40"))
REGISTRY_PORT = int(os.environ.get("REGISTRY_PORT", "15000"))
REGISTRY = os.environ.get("REGISTRY", f"127.0.0.1:{REGISTRY_PORT}")
PROOF = os.environ.get("PROOF", f"orca-layer-proof-{int(time.time() * 1000)}")
PROOF_PATH = os.environ.get("PROOF_PATH", "/tmp/orca-proof")


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def phase_start(name):
    return {"name": name, "started_at": now_utc(), "_start_ms": now_ms()}


def phase_end(phase, **extra):
    phase["finished_at"] = now_utc()
    phase["duration_ms"] = now_ms() - phase.pop("_start_ms")
    phase.update(extra)
    print(
        "phase {name} started_at={started_at} finished_at={finished_at} duration_ms={duration_ms}".format(**phase),
        flush=True,
    )
    return phase


def run(cmd, check=True):
    print(f"$ {cmd}", flush=True)
    result = subprocess.run(
        cmd,
        shell=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n", flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def wait_for_dockerd(sock):
    deadline = time.time() + DOCKERD_WAIT_SECONDS
    while time.time() < deadline:
        out = run("docker -H unix://%s info >/dev/null 2>&1 && echo ok || true" % q(sock), check=False).strip()
        if out == "ok":
            return
        time.sleep(1)
    raise RuntimeError(f"dockerd at {sock} did not become ready")


def wait_for_registry():
    deadline = time.time() + DOCKERD_WAIT_SECONDS
    while time.time() < deadline:
        out = run("docker exec orca-layer-registry wget -qO- http://127.0.0.1:%d/v2/ >/dev/null 2>&1 && echo ok || true" % REGISTRY_PORT, check=False).strip()
        if out == "ok":
            return
        time.sleep(1)
    raise RuntimeError("registry did not become ready")


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


class NodeDaemon:
    def __init__(self, name, work):
        self.name = name
        self.work = work
        (work / name).mkdir(parents=True, exist_ok=True)
        self.data_root = work / name / "data-root"
        self.containerd_root = work / name / "containerd-root"
        self.containerd_state = work / name / "containerd-state"
        self.exec_root = work / name / "exec-root"
        self.containerd_sock = work / name / "containerd.sock"
        self.docker_sock = work / name / "docker.sock"
        self.pidfile = work / name / "dockerd.pid"
        self.containerd_log = open(work / name / "containerd.log", "w", encoding="utf-8")
        self.dockerd_log = open(work / name / "dockerd.log", "w", encoding="utf-8")
        self.containerd_proc = None
        self.dockerd_proc = None

    def start(self):
        for path in [self.data_root, self.containerd_root, self.containerd_state, self.exec_root]:
            path.mkdir(parents=True, exist_ok=True)
        self.containerd_proc = subprocess.Popen(
            [
                "containerd",
                "--address", str(self.containerd_sock),
                "--root", str(self.containerd_root),
                "--state", str(self.containerd_state),
                "--log-level", "warn",
            ],
            stdout=self.containerd_log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        for _ in range(DOCKERD_WAIT_SECONDS):
            if self.containerd_sock.exists():
                break
            time.sleep(1)
        if not self.containerd_sock.exists():
            raise RuntimeError(f"{self.name} containerd socket did not appear")
        self.dockerd_proc = subprocess.Popen(
            [
                "dockerd",
                "--host", "unix://%s" % self.docker_sock,
                "--data-root", str(self.data_root),
                "--exec-root", str(self.exec_root),
                "--pidfile", str(self.pidfile),
                "--containerd", str(self.containerd_sock),
                "--containerd-namespace", "orca-layer-" + self.name,
                "--bridge", "none",
                "--iptables=false",
                "--ip-masq=false",
                "--insecure-registry", REGISTRY,
            ],
            stdout=self.dockerd_log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        wait_for_dockerd(self.docker_sock)

    def docker(self, args, check=True):
        return run("docker -H unix://%s %s" % (q(self.docker_sock), args), check=check)

    def stop(self):
        if self.dockerd_proc and self.dockerd_proc.poll() is None:
            self.dockerd_proc.terminate()
            try:
                self.dockerd_proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.dockerd_proc.kill()
                self.dockerd_proc.wait(timeout=10)
        if self.containerd_proc and self.containerd_proc.poll() is None:
            self.containerd_proc.terminate()
            try:
                self.containerd_proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.containerd_proc.kill()
                self.containerd_proc.wait(timeout=10)
        self.containerd_log.close()
        self.dockerd_log.close()


def measure_workspace(node, label):
    name = f"orca-layer-{label}-{int(time.time() * 1000)}"
    phase = phase_start(f"{node.name}_{label}")
    start = now_ms()
    started_at = now_utc()
    command = ""
    if START_COMMAND:
        command = " /bin/sh -lc %s" % q(START_COMMAND)
    cid = node.docker("run -d --network host --name %s %s%s" % (q(name), q(IMAGE), command)).strip()
    first_seen = {}
    try:
        deadline = time.time() + TIMEOUT_SECONDS
        while time.time() < deadline:
            logs = node.docker("logs %s 2>&1" % q(name), check=False)
            remember_markers(first_seen, logs, start)
            if JOIN_MARKER in logs:
                return {
                    "label": label,
                    "kind": "workspace-start",
                    "node": node.name,
                    "container": name,
                    "container_id": cid,
                    "started_at": started_at,
                    "finished_at": now_utc(),
                    "elapsed_ms": now_ms() - start,
                    "first_seen_ms": first_seen,
                    "phase": phase_end(phase, marker=JOIN_MARKER),
                }
            time.sleep(1)
        return {
            "label": label,
            "kind": "workspace-start",
            "node": node.name,
            "error": "timeout",
            "started_at": started_at,
            "finished_at": now_utc(),
            "elapsed_ms": now_ms() - start,
            "first_seen_ms": first_seen,
            "phase": phase_end(phase, error="timeout"),
        }
    finally:
        node.docker("rm -f %s >/dev/null 2>&1 || true" % q(name), check=False)


def print_workspace_result(result):
    print("\n[%s:%s]" % (result["node"], result["label"]))
    if result.get("error"):
        print("error=%s elapsed_ms=%s" % (result["error"], result["elapsed_ms"]))
    else:
        print("elapsed_to_join_ms=%s" % result["elapsed_ms"])
    for marker, elapsed in result.get("first_seen_ms", {}).items():
        print("%-40s %7dms" % (marker, elapsed))


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root; this script starts temporary dockerd/containerd daemons")
    started = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    work = (WORK_PARENT / started).resolve()
    work.mkdir(parents=True, exist_ok=True)

    registry_name = "orca-layer-registry"
    env_image = f"{REGISTRY}/orca-env:{started.lower()}"
    node1 = NodeDaemon("node-1", work)
    node2 = NodeDaemon("node-2", work)
    rows = []
    phases = []

    try:
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(registry_name), check=False)
        run("docker run -d --name %s --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:%d registry:2" % (q(registry_name), REGISTRY_PORT))
        wait_for_registry()

        node1.start()
        node2.start()

        print("preloading base image into node-1 and node-2", flush=True)
        run("docker image inspect %s >/dev/null" % q(IMAGE))
        preload_phase = phase_start("node1_preload_base")
        run("docker save %s | docker -H unix://%s load" % (q(IMAGE), q(node1.docker_sock)))
        phases.append(phase_end(preload_phase))
        node1_preload_ms = phases[-1]["duration_ms"]
        preload_phase = phase_start("node2_preload_base")
        run("docker save %s | docker -H unix://%s load" % (q(IMAGE), q(node2.docker_sock)))
        phases.append(phase_end(preload_phase))
        node2_preload_ms = phases[-1]["duration_ms"]

        base1 = measure_workspace(node1, "base-start")
        phases.append(base1["phase"])
        print_workspace_result(base1)
        rows.append(base1)
        base2 = measure_workspace(node2, "base-start")
        phases.append(base2["phase"])
        print_workspace_result(base2)
        rows.append(base2)

        print("node-1 creating changed container and committing env image", flush=True)
        change_phase = phase_start("node1_commit_env_delta")
        cid = node1.docker(
            "create --entrypoint /bin/sh %s -lc %s"
            % (q(IMAGE), q(f"printf '%s\\n' {shlex.quote(PROOF)} > {shlex.quote(PROOF_PATH)} && cat {shlex.quote(PROOF_PATH)}"))
        ).strip()
        node1.docker("start -a %s" % q(cid))
        node1.docker("commit %s %s" % (q(cid), q(env_image)))
        node1.docker("rm -f %s >/dev/null" % q(cid), check=False)
        phases.append(phase_end(change_phase))
        commit_ms = phases[-1]["duration_ms"]

        print("node-1 pushing committed env image to registry", flush=True)
        push_phase = phase_start("node1_push_env_image")
        push_out = node1.docker("push %s" % q(env_image))
        phases.append(phase_end(push_phase))
        push_ms = phases[-1]["duration_ms"]

        print("node-2 running changed image from registry", flush=True)
        resume_phase = phase_start("node2_resume_changed_image")
        proof_out = node2.docker("run --rm --entrypoint /bin/cat %s %s" % (q(env_image), q(PROOF_PATH)))
        phases.append(phase_end(resume_phase))
        resume_ms = phases[-1]["duration_ms"]
        proof_ok = PROOF in proof_out

        summary = {
            "image": IMAGE,
            "start_command": START_COMMAND,
            "marker": JOIN_MARKER,
            "env_image": env_image,
            "proof": PROOF,
            "proof_path": PROOF_PATH,
            "proof_ok": proof_ok,
            "node1_preload_ms": node1_preload_ms,
            "node2_preload_ms": node2_preload_ms,
            "node1_base_start_ms": base1.get("elapsed_ms"),
            "node2_base_start_ms": base2.get("elapsed_ms"),
            "commit_ms": commit_ms,
            "push_ms": push_ms,
            "node2_resume_changed_ms": resume_ms,
            "work_dir": str(work),
            "push_tail": push_out.splitlines()[-12:],
            "phases": phases,
        }
        print("\n[layer-resume-summary]")
        for key in [
            "node1_preload_ms",
            "node2_preload_ms",
            "node1_base_start_ms",
            "node2_base_start_ms",
            "commit_ms",
            "push_ms",
            "node2_resume_changed_ms",
            "proof_ok",
            "env_image",
            "work_dir",
        ]:
            print(f"{key}={summary[key]}")
        print("\n[layer-resume-phases]")
        for phase in phases:
            print(
                "{name:28s} {duration_ms:7d}ms  {started_at} -> {finished_at}".format(
                    name=phase["name"],
                    duration_ms=phase["duration_ms"],
                    started_at=phase["started_at"],
                    finished_at=phase["finished_at"],
                )
            )
        raw = json.dumps({"summary": summary, "workspace_starts": rows}, sort_keys=True)
        (work / "result.json").write_text(raw + "\n", encoding="utf-8")
        print("\nSUMMARY_JSON=%s" % raw)
    finally:
        node1.stop()
        node2.stop()
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(registry_name), check=False)


if __name__ == "__main__":
    main()

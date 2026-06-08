#!/usr/bin/env python3
import json
import os
import shlex
import signal
import subprocess
import time
from datetime import datetime, timezone
from pathlib import Path


IMAGE = os.environ.get("IMAGE", "alpine:3.22")
START_COMMAND = os.environ.get("START_COMMAND", "echo ORCA_STARGZ_READY")
MARKER = os.environ.get("MARKER", "ORCA_STARGZ_READY")
PROOF = os.environ.get("PROOF", f"orca-stargz-proof-{int(time.time() * 1000)}")
PROOF_PATH = os.environ.get("PROOF_PATH", "/tmp/orca-proof")
WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/stargz-layer-resume"))
REGISTRY_PORT = int(os.environ.get("REGISTRY_PORT", "15001"))
REGISTRY = os.environ.get("REGISTRY", f"127.0.0.1:{REGISTRY_PORT}")
TIMEOUT_SECONDS = int(os.environ.get("TIMEOUT_SECONDS", "60"))
WAIT_SECONDS = int(os.environ.get("WAIT_SECONDS", "40"))
STARGZ_VERSION = os.environ.get("STARGZ_VERSION", "v0.18.2")
STARGZ_TOOLS_DIR = Path(os.environ.get("STARGZ_TOOLS_DIR", ".tmp/stargz-tools"))


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def run(cmd, check=True):
    print(f"$ {cmd}", flush=True)
    result = subprocess.run(cmd, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n", flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


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


def ensure_stargz_tools():
    STARGZ_TOOLS_DIR.mkdir(parents=True, exist_ok=True)
    ctr_remote = STARGZ_TOOLS_DIR / "ctr-remote"
    snapshotter = STARGZ_TOOLS_DIR / "containerd-stargz-grpc"
    if ctr_remote.exists() and snapshotter.exists():
        return ctr_remote.resolve(), snapshotter.resolve()
    archive = STARGZ_TOOLS_DIR / f"stargz-snapshotter-{STARGZ_VERSION}-linux-amd64.tar.gz"
    url = f"https://github.com/containerd/stargz-snapshotter/releases/download/{STARGZ_VERSION}/stargz-snapshotter-{STARGZ_VERSION}-linux-amd64.tar.gz"
    if not archive.exists():
        run("curl -L --fail -o %s %s" % (q(archive), q(url)))
    run("tar -xzf %s -C %s ctr-remote containerd-stargz-grpc" % (q(archive), q(STARGZ_TOOLS_DIR)))
    run("chmod +x %s %s" % (q(ctr_remote), q(snapshotter)))
    return ctr_remote.resolve(), snapshotter.resolve()


def wait_for_registry():
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        out = run("docker exec orca-stargz-registry wget -qO- http://127.0.0.1:%d/v2/ >/dev/null 2>&1 && echo ok || true" % REGISTRY_PORT, check=False).strip()
        if out == "ok":
            return
        time.sleep(1)
    raise RuntimeError("registry did not become ready")


class ContainerdNode:
    def __init__(self, name, work, ctr_remote, snapshotter_bin=None):
        self.name = name
        self.work = work / name
        self.ctr_remote = ctr_remote
        self.snapshotter_bin = snapshotter_bin
        self.root = self.work / "containerd-root"
        self.state = self.work / "containerd-state"
        self.sock = self.work / "containerd.sock"
        self.config = self.work / "containerd.toml"
        self.stargz_root = self.work / "stargz-root"
        self.stargz_sock = self.work / "stargz.sock"
        self.log = None
        self.stargz_log = None
        self.proc = None
        self.stargz_proc = None

    def start(self):
        self.work.mkdir(parents=True, exist_ok=True)
        self.root.mkdir(parents=True, exist_ok=True)
        self.state.mkdir(parents=True, exist_ok=True)
        self.log = open(self.work / "containerd.log", "w", encoding="utf-8")
        if self.snapshotter_bin:
            self.stargz_root.mkdir(parents=True, exist_ok=True)
            self.stargz_log = open(self.work / "stargz.log", "w", encoding="utf-8")
            self.stargz_proc = subprocess.Popen(
                [
                    str(self.snapshotter_bin),
                    "-address", str(self.stargz_sock),
                    "-root", str(self.stargz_root),
                    "-log-level", "warn",
                ],
                stdout=self.stargz_log,
                stderr=subprocess.STDOUT,
                text=True,
            )
            for _ in range(WAIT_SECONDS):
                if self.stargz_sock.exists():
                    break
                time.sleep(1)
            if not self.stargz_sock.exists():
                raise RuntimeError(f"{self.name} stargz socket did not appear")
            self.config.write_text(
                'version = 2\n'
                '[proxy_plugins]\n'
                '  [proxy_plugins.stargz]\n'
                '    type = "snapshot"\n'
                f'    address = "{self.stargz_sock}"\n',
                encoding="utf-8",
            )
            config_args = ["--config", str(self.config)]
        else:
            config_args = []
        self.proc = subprocess.Popen(
            ["containerd", "--address", str(self.sock), "--root", str(self.root), "--state", str(self.state), "--log-level", "warn", *config_args],
            stdout=self.log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        for _ in range(WAIT_SECONDS):
            if self.sock.exists():
                return
            time.sleep(1)
        raise RuntimeError(f"{self.name} containerd socket did not appear")

    def ctr(self, args, check=True):
        return run("%s --address %s --namespace default %s" % (q(self.ctr_remote), q(self.sock), args), check=check)

    def stop(self):
        if self.proc and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        if self.stargz_proc and self.stargz_proc.poll() is None:
            self.stargz_proc.send_signal(signal.SIGTERM)
            try:
                self.stargz_proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.stargz_proc.kill()
                self.stargz_proc.wait(timeout=10)
        if self.log:
            self.log.close()
        if self.stargz_log:
            self.stargz_log.close()


def docker_seed_registry(started):
    base_normal = f"{REGISTRY}/orca-base:{started.lower()}"
    env_normal = f"{REGISTRY}/orca-env-normal:{started.lower()}"
    run("docker pull %s" % q(IMAGE))
    run("docker tag %s %s" % (q(IMAGE), q(base_normal)))
    run("docker push %s" % q(base_normal))
    cid = run(
        "docker create --network host --entrypoint /bin/sh %s -lc %s"
        % (q(IMAGE), q(f"printf '%s\\n' {shlex.quote(PROOF)} > {shlex.quote(PROOF_PATH)} && cat {shlex.quote(PROOF_PATH)}"))
    ).strip()
    try:
        run("docker start -a %s" % q(cid))
        run("docker commit %s %s" % (q(cid), q(env_normal)))
        run("docker push %s" % q(env_normal))
    finally:
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(cid), check=False)
    return base_normal, env_normal


def optimize_and_push(optimizer, source, target):
    optimizer.ctr("images pull --plain-http %s" % q(source))
    optimizer.ctr("images optimize --oci --no-optimize %s %s" % (q(source), q(target)))
    optimizer.ctr("images push --plain-http %s" % q(target))


def run_marker(node, image_ref, label):
    phase = phase_start(label)
    cname = label.replace("_", "-") + "-" + str(int(time.time() * 1000))
    start = now_ms()
    out = node.ctr(
        "run --rm --net-host --snapshotter stargz %s %s /bin/sh -lc %s"
        % (q(image_ref), q(cname), q(START_COMMAND))
    )
    elapsed = now_ms() - start
    if MARKER not in out:
        raise RuntimeError(f"marker {MARKER!r} missing from output: {out}")
    return phase_end(phase, elapsed_ms=elapsed, output=out.strip())


def run_proof(node, image_ref):
    phase = phase_start("node2_resume_changed_stargz")
    cname = "stargz-proof-" + str(int(time.time() * 1000))
    start = now_ms()
    out = node.ctr(
        "run --rm --net-host --snapshotter stargz %s %s /bin/cat %s"
        % (q(image_ref), q(cname), q(PROOF_PATH))
    )
    elapsed = now_ms() - start
    return phase_end(phase, elapsed_ms=elapsed, proof_ok=PROOF in out, output=out.strip())


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root; this script starts containerd/stargz daemons")
    started = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    work = (WORK_PARENT / started).resolve()
    work.mkdir(parents=True, exist_ok=True)
    ctr_remote, snapshotter_bin = ensure_stargz_tools()
    registry_name = "orca-stargz-registry"
    optimizer = ContainerdNode("optimizer", work, ctr_remote)
    node2 = ContainerdNode("node-2", work, ctr_remote, snapshotter_bin=snapshotter_bin)
    phases = []

    try:
        run("docker rm -f %s >/dev/null 2>&1 || true" % registry_name, check=False)
        run("docker run -d --name %s --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:%d registry:2" % (q(registry_name), REGISTRY_PORT))
        wait_for_registry()
        optimizer.start()
        node2.start()

        seed_phase = phase_start("seed_normal_images")
        base_normal, env_normal = docker_seed_registry(started)
        phases.append(phase_end(seed_phase, base_normal=base_normal, env_normal=env_normal))

        base_esgz = f"{REGISTRY}/orca-base-esgz:{started.lower()}"
        env_esgz = f"{REGISTRY}/orca-env-esgz:{started.lower()}"
        opt_phase = phase_start("optimize_base_esgz")
        optimize_and_push(optimizer, base_normal, base_esgz)
        phases.append(phase_end(opt_phase, image=base_esgz))
        opt_phase = phase_start("optimize_env_esgz")
        optimize_and_push(optimizer, env_normal, env_esgz)
        phases.append(phase_end(opt_phase, image=env_esgz))

        pull_phase = phase_start("node2_rpull_base_esgz")
        node2.ctr("images rpull --plain-http --snapshotter stargz %s" % q(base_esgz))
        phases.append(phase_end(pull_phase, image=base_esgz))

        phases.append(run_marker(node2, base_esgz, "node2_base_start_stargz"))

        pull_phase = phase_start("node2_rpull_env_esgz")
        node2.ctr("images rpull --plain-http --snapshotter stargz %s" % q(env_esgz))
        phases.append(phase_end(pull_phase, image=env_esgz))
        phases.append(run_proof(node2, env_esgz))

        summary = {
            "image": IMAGE,
            "base_normal": base_normal,
            "env_normal": env_normal,
            "base_esgz": base_esgz,
            "env_esgz": env_esgz,
            "proof": PROOF,
            "proof_path": PROOF_PATH,
            "proof_ok": phases[-1].get("proof_ok"),
            "work_dir": str(work),
            "phases": phases,
        }
        print("\n[stargz-layer-resume-summary]")
        for phase in phases:
            print("{name:28s} {duration_ms:7d}ms  {started_at} -> {finished_at}".format(**phase))
        raw = json.dumps(summary, sort_keys=True)
        (work / "result.json").write_text(raw + "\n", encoding="utf-8")
        print("\nSUMMARY_JSON=%s" % raw)
    finally:
        node2.stop()
        optimizer.stop()
        run("docker rm -f %s >/dev/null 2>&1 || true" % registry_name, check=False)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/sysbox-stargz-first-output"))
RESULTS_FILE = Path(os.environ.get("RESULTS_FILE", "docs/benchmarks/sysbox-stargz-first-output-scenarios.md"))
BASE_IMAGE = os.environ.get("BASE_IMAGE", "alpine:3.22")
REGISTRY_PORT = int(os.environ.get("REGISTRY_PORT", "15002"))
REGISTRY_LOCAL = os.environ.get("REGISTRY_LOCAL", f"127.0.0.1:{REGISTRY_PORT}")
REGISTRY_NAME = os.environ.get("REGISTRY_NAME", "orca-sysbox-stargz-registry")
STARGZ_TOOLS_DIR = Path(os.environ.get("STARGZ_TOOLS_DIR", ".tmp/stargz-tools"))
DISK_BENCH_BIN = Path(os.environ.get("DISK_BENCH_BIN", ".tmp/disk-bench/disk-bench-linux-amd64"))
SIZE_MB = int(os.environ.get("DISK_BENCH_SIZE_MB", "256"))
WAIT_SECONDS = int(os.environ.get("WAIT_SECONDS", "120"))
SYSBOX_IMAGE = os.environ.get("SYSBOX_IMAGE", "docker:29-dind")
FORCE_IMAGE = os.environ.get("FORCE_IMAGE", "").lower() in {"1", "true", "yes", "on"}


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def run(cmd, check=True):
    print("$ %s" % cmd, flush=True)
    result = subprocess.run(cmd, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n")
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def require(path, label):
    if not Path(path).exists():
        raise RuntimeError("missing %s at %s" % (label, path))


def registry_has_image(ref):
    host_repo, tag = ref.rsplit(":", 1)
    _, repo = host_repo.split("/", 1)
    url = "http://%s/v2/%s/manifests/%s" % (REGISTRY_LOCAL, repo, tag)
    out = run("curl -fsS -H %s %s >/dev/null 2>&1 && echo yes || true" % (
        q("Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"),
        q(url),
    ), check=False).strip()
    return out == "yes"


def wait_for_registry():
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        out = run(
            "docker exec %s wget -qO- http://127.0.0.1:%d/v2/ >/dev/null 2>&1 && echo ok || true"
            % (q(REGISTRY_NAME), REGISTRY_PORT),
            check=False,
        ).strip()
        if out == "ok":
            return
        time.sleep(1)
    raise RuntimeError("registry did not become ready")


def ensure_registry():
    running = run("docker inspect -f '{{.State.Running}}' %s 2>/dev/null || true" % q(REGISTRY_NAME), check=False).strip()
    if running != "true":
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(REGISTRY_NAME), check=False)
        run("docker run -d --name %s --network host -e REGISTRY_HTTP_ADDR=0.0.0.0:%d registry:2" % (q(REGISTRY_NAME), REGISTRY_PORT))
    wait_for_registry()


def host_containerd_ctr(work, args):
    root = work / "optimizer-root"
    state = work / "optimizer-state"
    sock = work / "optimizer.sock"
    log_path = work / "optimizer-containerd.log"
    root.mkdir(parents=True, exist_ok=True)
    state.mkdir(parents=True, exist_ok=True)
    log = log_path.open("ab")
    proc = subprocess.Popen(
        ["containerd", "--address", str(sock), "--root", str(root), "--state", str(state), "--log-level", "warn"],
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    try:
        deadline = time.time() + WAIT_SECONDS
        while time.time() < deadline:
            if sock.exists():
                break
            if proc.poll() is not None:
                raise RuntimeError("optimizer containerd exited:\n%s" % log_path.read_text(errors="replace"))
            time.sleep(0.2)
        else:
            raise RuntimeError("optimizer containerd socket did not appear")
        return run("%s --address %s --namespace default %s" % (q(STARGZ_TOOLS_DIR / "ctr-remote"), q(sock), args))
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        log.close()


def optimize_and_push(work, source, target):
    if not FORCE_IMAGE and registry_has_image(target):
        return False
    host_containerd_ctr(work, "images pull --plain-http %s" % q(source))
    host_containerd_ctr(work, "images optimize --oci %s %s" % (q(source), q(target)))
    host_containerd_ctr(work, "images push --plain-http %s" % q(target))
    return True


def build_and_push_actual(work, image):
    if not FORCE_IMAGE and registry_has_image(image):
        return False
    context = work / "image-context"
    context.mkdir(parents=True, exist_ok=True)
    run("cp %s %s" % (q(DISK_BENCH_BIN), q(context / "disk-bench")))
    (context / "Dockerfile").write_text(
        "\n".join([
            "FROM %s" % BASE_IMAGE,
            "ARG SIZE_MB",
            "COPY disk-bench /disk-bench",
            "RUN chmod +x /disk-bench && mkdir -p /data && dd if=/dev/urandom of=/data/bench.dat bs=1M count=${SIZE_MB} status=none",
            'ENTRYPOINT ["/disk-bench"]',
            "",
        ]),
        encoding="utf-8",
    )
    run("docker build --network host --build-arg SIZE_MB=%d -t %s %s" % (SIZE_MB, q(image), q(context)))
    run("docker push %s" % q(image))
    return True


def push_base(image):
    if not FORCE_IMAGE and registry_has_image(image):
        return False
    run("docker pull %s" % q(BASE_IMAGE))
    run("docker tag %s %s" % (q(BASE_IMAGE), q(image)))
    run("docker push %s" % q(image))
    return True


def start_node(name, state_dir):
    run("docker rm -f %s >/dev/null 2>&1 || true" % q(name), check=False)
    containerd_dir = state_dir / "containerd"
    stargz_dir = state_dir / "stargz"
    containerd_dir.mkdir(parents=True, exist_ok=True)
    stargz_dir.mkdir(parents=True, exist_ok=True)
    run(
        "docker run -d --runtime=sysbox-runc --name %s "
        "--add-host=host.docker.internal:host-gateway "
        "-v %s:/var/lib/containerd "
        "-v %s:/var/lib/containerd-stargz-grpc "
        "-v %s:/tools:ro %s sh -lc 'sleep infinity'"
        % (q(name), q(containerd_dir.resolve()), q(stargz_dir.resolve()), q(STARGZ_TOOLS_DIR.resolve()), q(SYSBOX_IMAGE))
    )


def exec_in(name, cmd, check=True):
    return run("docker exec %s sh -lc %s" % (q(name), q(cmd)), check=check)


def wait_for_online(name):
    online_start = now_ms()
    exec_in(name, "mkdir -p /run/containerd /run/containerd-stargz-grpc /var/lib/containerd /var/lib/containerd-stargz-grpc /etc/containerd")
    exec_in(name, "cat >/etc/containerd/config.toml <<'EOF'\nversion = 2\n[proxy_plugins]\n  [proxy_plugins.stargz]\n    type = \"snapshot\"\n    address = \"/run/containerd-stargz-grpc/containerd-stargz-grpc.sock\"\nEOF")
    exec_in(name, "/tools/containerd-stargz-grpc -address /run/containerd-stargz-grpc/containerd-stargz-grpc.sock -root /var/lib/containerd-stargz-grpc -log-level warn >/tmp/stargz.log 2>&1 &")
    exec_in(name, "containerd --address /run/containerd/containerd.sock --config /etc/containerd/config.toml --log-level warn >/tmp/containerd.log 2>&1 &")
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        out = exec_in(name, "test -S /run/containerd/containerd.sock && test -S /run/containerd-stargz-grpc/containerd-stargz-grpc.sock && echo ready || true", check=False).strip()
        if out == "ready":
            return now_ms() - online_start
        time.sleep(1)
    logs = exec_in(name, "cat /tmp/stargz.log /tmp/containerd.log 2>/dev/null || true", check=False)
    raise RuntimeError("containerd/stargz not ready:\n%s" % logs)


def guest_ref(ref):
    return "host.docker.internal:%d/%s" % (REGISTRY_PORT, ref.split("/", 1)[1])


def rpull(name, ref):
    start = now_ms()
    exec_in(name, "/tools/ctr-remote --address /run/containerd/containerd.sock --namespace default images rpull --plain-http --snapshotter stargz %s" % q(guest_ref(ref)))
    return now_ms() - start


def run_first_output(name, ref):
    out = exec_in(name, "/tools/ctr-remote --address /run/containerd/containerd.sock --namespace default run --rm --snapshotter stargz --net-host %s first-output /bin/sh -lc 'echo ORCA_FIRST_USER_OUTPUT'" % q(guest_ref(ref)))
    if "ORCA_FIRST_USER_OUTPUT" not in out:
        raise RuntimeError("first-output marker missing:\n%s" % out)


def measure_scenario(work, scenario, actual_esgz, base_esgz):
    name = "orca-sysbox-online-%s-%d" % (scenario.replace("_", "-"), int(time.time() * 1000))
    state_dir = work / name
    start_node(name, state_dir)
    try:
        node_online_ms = wait_for_online(name)
        prefetch = {}
        if scenario == "parent_present":
            prefetch["parent_prefetch_ms"] = rpull(name, base_esgz)
        elif scenario == "actual_present":
            prefetch["actual_prefetch_ms"] = rpull(name, actual_esgz)

        start = now_ms()
        rpull_ms = ""
        if scenario in {"no_image_present", "parent_present"}:
            rpull_ms = rpull(name, actual_esgz)
        run_first_output(name, actual_esgz)
        first_output_ms = now_ms() - start
        return {
            "scenario": scenario,
            "node_online_ms": node_online_ms,
            **prefetch,
            "measured_rpull_ms": rpull_ms,
            "first_output_ms": first_output_ms,
        }
    finally:
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(name), check=False)


def table(rows):
    lines = [
        "| Scenario | Node online prep | Prefetch | Measured rpull | First output |",
        "| --- | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        prefetch = row.get("parent_prefetch_ms", row.get("actual_prefetch_ms", ""))
        if prefetch != "":
            prefetch = "%s ms" % prefetch
        rpull_ms = row["measured_rpull_ms"]
        if rpull_ms != "":
            rpull_ms = "%s ms" % rpull_ms
        lines.append("| %s | %s ms | %s | %s | %s ms |" % (
            row["scenario"],
            row["node_online_ms"],
            prefetch,
            rpull_ms,
            row["first_output_ms"],
        ))
    return "\n".join(lines)


def write_results(started, rows, summary):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    RESULTS_FILE.write_text(
        "\n".join([
            "# Sysbox stargz first-output scenarios",
            "",
            "Generated: `%s`" % started,
            "",
            "Each scenario starts a fresh Sysbox container and waits until containerd plus `containerd-stargz-grpc` are online. The timer for `First output` starts only after the node is online and after any scenario prefetch is complete.",
            "",
            table(rows),
            "",
            "```json",
            json.dumps(summary, indent=2, sort_keys=True),
            "```",
            "",
        ]),
        encoding="utf-8",
    )


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root")
    require(STARGZ_TOOLS_DIR / "ctr-remote", "ctr-remote")
    require(STARGZ_TOOLS_DIR / "containerd-stargz-grpc", "containerd-stargz-grpc")
    require(DISK_BENCH_BIN, "disk-bench binary")
    started = now_utc()
    work = (WORK_PARENT / time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())).resolve()
    work.mkdir(parents=True, exist_ok=True)
    ensure_registry()
    suffix = "size%dm" % SIZE_MB
    actual_normal = f"{REGISTRY_LOCAL}/orca-sysbox-actual-normal:{suffix}"
    actual_esgz = f"{REGISTRY_LOCAL}/orca-sysbox-actual-esgz:{suffix}"
    base_normal = f"{REGISTRY_LOCAL}/orca-sysbox-base-normal:{BASE_IMAGE.replace(':', '-')}"
    base_esgz = f"{REGISTRY_LOCAL}/orca-sysbox-base-esgz:{BASE_IMAGE.replace(':', '-')}"
    built_actual = build_and_push_actual(work, actual_normal)
    optimized_actual = optimize_and_push(work, actual_normal, actual_esgz)
    built_base = push_base(base_normal)
    optimized_base = optimize_and_push(work, base_normal, base_esgz)
    rows = [
        measure_scenario(work, "no_image_present", actual_esgz, base_esgz),
        measure_scenario(work, "parent_present", actual_esgz, base_esgz),
        measure_scenario(work, "actual_present", actual_esgz, base_esgz),
    ]
    summary = {
        "started": started,
        "work_dir": str(work),
        "actual_normal": actual_normal,
        "actual_esgz": actual_esgz,
        "base_normal": base_normal,
        "base_esgz": base_esgz,
        "built_actual": built_actual,
        "optimized_actual": optimized_actual,
        "built_base": built_base,
        "optimized_base": optimized_base,
        "rows": rows,
    }
    write_results(started, rows, summary)
    print(table(rows))
    print("results_file=%s" % RESULTS_FILE)


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

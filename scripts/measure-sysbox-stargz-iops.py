#!/usr/bin/env python3
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


WORK_PARENT = Path(os.environ.get("WORK_PARENT", ".tmp/sysbox-stargz-iops"))
RESULTS_FILE = Path(os.environ.get("RESULTS_FILE", "docs/benchmarks/sysbox-stargz-iops-results.md"))
BASE_IMAGE = os.environ.get("BASE_IMAGE", "alpine:3.22")
REGISTRY_PORT = int(os.environ.get("REGISTRY_PORT", "15002"))
REGISTRY_LOCAL = os.environ.get("REGISTRY_LOCAL", f"127.0.0.1:{REGISTRY_PORT}")
REGISTRY_NAME = os.environ.get("REGISTRY_NAME", "orca-sysbox-stargz-registry")
STARGZ_TOOLS_DIR = Path(os.environ.get("STARGZ_TOOLS_DIR", ".tmp/stargz-tools"))
PREBUILT_DISK_BENCH = Path(os.environ.get("DISK_BENCH_BIN", ".tmp/disk-bench/disk-bench-linux-amd64"))
SIZE_MB = int(os.environ.get("DISK_BENCH_SIZE_MB", "256"))
RANDOM_OPS = int(os.environ.get("DISK_BENCH_RANDOM_OPS", "65536"))
SEQ_BLOCK_BYTES = int(os.environ.get("DISK_BENCH_SEQ_BLOCK_BYTES", str(1024 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("DISK_BENCH_RANDOM_BLOCK_BYTES", "4096"))
IO_DEPTH = int(os.environ.get("DISK_BENCH_IO_DEPTH", "1"))
WAIT_SECONDS = int(os.environ.get("WAIT_SECONDS", "90"))
FORCE_IMAGE = os.environ.get("FORCE_IMAGE", "").lower() in {"1", "true", "yes", "on"}
SYSBOX_IMAGE = os.environ.get("SYSBOX_IMAGE", "docker:29-dind")
PARTIAL_BASE_CACHE = os.environ.get("PARTIAL_BASE_CACHE", "").lower() in {"1", "true", "yes", "on"}


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


def extract_results(output):
    return [json.loads(m) for m in re.findall(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)]


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


def tag_suffix():
    return "size%dm-rand%d-qd%d" % (SIZE_MB, RANDOM_OPS, IO_DEPTH)


def build_and_push(work, image):
    if not FORCE_IMAGE and registry_has_image(image):
        return False
    context = work / "image-context"
    context.mkdir(parents=True, exist_ok=True)
    run("cp %s %s" % (q(PREBUILT_DISK_BENCH), q(context / "disk-bench")))
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


def push_base_image(image):
    if not FORCE_IMAGE and registry_has_image(image):
        return False
    run("docker pull %s" % q(BASE_IMAGE))
    run("docker tag %s %s" % (q(BASE_IMAGE), q(image)))
    run("docker push %s" % q(image))
    return True


def host_containerd_ctr(work, args):
    root = work / "optimizer-root"
    state = work / "optimizer-state"
    sock = work / "optimizer.sock"
    log_path = work / "optimizer-containerd.log"
    root.mkdir(parents=True, exist_ok=True)
    state.mkdir(parents=True, exist_ok=True)
    proc = subprocess.Popen(
        ["containerd", "--address", str(sock), "--root", str(root), "--state", str(state), "--log-level", "warn"],
        stdout=log_path.open("ab"),
        stderr=subprocess.STDOUT,
    )
    try:
        deadline = time.time() + WAIT_SECONDS
        while time.time() < deadline:
            if sock.exists():
                break
            if proc.poll() is not None:
                raise RuntimeError("optimizer containerd exited")
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


def optimize_and_push(work, source, target):
    if not FORCE_IMAGE and registry_has_image(target):
        return False
    host_containerd_ctr(work, "images pull --plain-http %s" % q(source))
    host_containerd_ctr(work, "images optimize --oci %s %s" % (q(source), q(target)))
    host_containerd_ctr(work, "images push --plain-http %s" % q(target))
    return True


def start_sysbox_container(name, state_dir):
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


def wait_for_sysbox_containerd(name):
    exec_in(name, "mkdir -p /run/containerd /run/containerd-stargz-grpc /var/lib/containerd /var/lib/containerd-stargz-grpc /etc/containerd")
    exec_in(name, "cat >/etc/containerd/config.toml <<'EOF'\nversion = 2\n[proxy_plugins]\n  [proxy_plugins.stargz]\n    type = \"snapshot\"\n    address = \"/run/containerd-stargz-grpc/containerd-stargz-grpc.sock\"\nEOF")
    exec_in(name, "/tools/containerd-stargz-grpc -address /run/containerd-stargz-grpc/containerd-stargz-grpc.sock -root /var/lib/containerd-stargz-grpc -log-level warn >/tmp/stargz.log 2>&1 &")
    exec_in(name, "containerd --address /run/containerd/containerd.sock --config /etc/containerd/config.toml --log-level warn >/tmp/containerd.log 2>&1 &")
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        out = exec_in(name, "test -S /run/containerd/containerd.sock && test -S /run/containerd-stargz-grpc/containerd-stargz-grpc.sock && echo ready || true", check=False).strip()
        if out == "ready":
            return
        time.sleep(1)
    logs = exec_in(name, "cat /tmp/stargz.log /tmp/containerd.log 2>/dev/null || true", check=False)
    raise RuntimeError("sysbox containerd/stargz not ready:\n%s" % logs)


def sysbox_operation(image, operation, work, prefetch_image=None):
    name = "orca-sysbox-stargz-%s-%d" % (operation.replace("_", "-"), int(time.time() * 1000))
    state_dir = work / name
    start_sysbox_container(name, state_dir)
    try:
        wait_for_sysbox_containerd(name)
        guest_image = "host.docker.internal:%d/%s" % (REGISTRY_PORT, image.split("/", 1)[1])
        prefetch_ms = ""
        if prefetch_image:
            guest_prefetch_image = "host.docker.internal:%d/%s" % (REGISTRY_PORT, prefetch_image.split("/", 1)[1])
            prefetch_start = now_ms()
            exec_in(name, "/tools/ctr-remote --address /run/containerd/containerd.sock --namespace default images rpull --plain-http --snapshotter stargz %s" % q(guest_prefetch_image))
            prefetch_ms = now_ms() - prefetch_start
        mode = {"sequential_read": "sequential", "random_read": "random"}.get(operation)
        start = now_ms()
        rpull_start = now_ms()
        exec_in(name, "/tools/ctr-remote --address /run/containerd/containerd.sock --namespace default images rpull --plain-http --snapshotter stargz %s" % q(guest_image))
        rpull_ms = now_ms() - rpull_start
        if operation == "first_user_output":
            out = exec_in(name, "/tools/ctr-remote --address /run/containerd/containerd.sock --namespace default run --rm --snapshotter stargz --net-host %s first-output /bin/sh -lc 'echo ORCA_FIRST_USER_OUTPUT'" % q(guest_image))
            if "ORCA_FIRST_USER_OUTPUT" not in out:
                raise RuntimeError("first output marker missing:\n%s" % out)
            return {"runtime": "sysbox-stargz-partial-base" if prefetch_image else "sysbox-stargz", "operation": operation, "wall_ms": now_ms() - start, "rpull_ms": rpull_ms, "prefetch_ms": prefetch_ms, "bench_duration_ms": "", "mb_per_sec": "", "iops": ""}
        block = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
        out = exec_in(
            name,
            "/tools/ctr-remote --address /run/containerd/containerd.sock --namespace default run --rm --snapshotter stargz --net-host %s bench /disk-bench -kind sysbox-stargz-%s -mode %s -path /data/bench.dat -size-bytes %d -block-bytes %d -random-ops %d -io-depth %d"
            % (q(guest_image), mode, mode, SIZE_MB * 1024 * 1024, block, RANDOM_OPS, IO_DEPTH),
        )
        results = extract_results(out)
        if len(results) != 1:
            raise RuntimeError("expected one disk-bench result, got %d:\n%s" % (len(results), out))
        result = results[0]
        return {
            "runtime": "sysbox-stargz-partial-base" if prefetch_image else "sysbox-stargz",
            "operation": operation,
            "wall_ms": now_ms() - start,
            "rpull_ms": rpull_ms,
            "prefetch_ms": prefetch_ms,
            "bench_duration_ms": result.get("duration_ms", ""),
            "mb_per_sec": result.get("mb_per_sec", ""),
            "iops": result.get("iops", ""),
        }
    finally:
        run("docker rm -f %s >/dev/null 2>&1 || true" % q(name), check=False)


def markdown_table(rows):
    lines = [
        "| Runtime | Operation | Wall | prefetch | rpull | Bench | Throughput | IOPS |",
        "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        lines.append(
            "| {runtime} | {operation} | {wall_ms} ms | {prefetch} | {rpull_ms} ms | {bench} | {throughput} | {iops} |".format(
                runtime=row["runtime"],
                operation=row["operation"],
                wall_ms=row["wall_ms"],
                prefetch=("" if row.get("prefetch_ms", "") == "" else "%s ms" % row["prefetch_ms"]),
                rpull_ms=row["rpull_ms"],
                bench=("" if row["bench_duration_ms"] == "" else "%s ms" % row["bench_duration_ms"]),
                throughput=("" if row["mb_per_sec"] == "" else "%s MiB/s" % row["mb_per_sec"]),
                iops=row["iops"],
            )
        )
    return "\n".join(lines)


def write_results(started, rows, summary):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    text = "\n".join([
        "# Sysbox stargz IOPS",
        "",
        "Generated: `%s`" % started,
        "",
        "Each operation starts a fresh Sysbox container, starts containerd plus `containerd-stargz-grpc` inside it, then measures `ctr-remote images rpull` plus the container action. The local registry and eStargz image are prepared on the host.",
        "",
        markdown_table(rows),
        "",
        "```json",
        json.dumps(summary, indent=2, sort_keys=True),
        "```",
        "",
    ])
    RESULTS_FILE.write_text(text, encoding="utf-8")


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root")
    require(STARGZ_TOOLS_DIR / "ctr-remote", "ctr-remote")
    require(STARGZ_TOOLS_DIR / "containerd-stargz-grpc", "containerd-stargz-grpc")
    require(PREBUILT_DISK_BENCH, "disk-bench binary")
    started = now_utc()
    work = (WORK_PARENT / time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())).resolve()
    work.mkdir(parents=True, exist_ok=True)
    ensure_registry()
    suffix = tag_suffix()
    normal = f"{REGISTRY_LOCAL}/orca-sysbox-disk-bench-normal:{suffix}"
    esgz = f"{REGISTRY_LOCAL}/orca-sysbox-disk-bench-esgz:{suffix}"
    base_normal = f"{REGISTRY_LOCAL}/orca-sysbox-base-normal:{BASE_IMAGE.replace(':', '-')}"
    base_esgz = f"{REGISTRY_LOCAL}/orca-sysbox-base-esgz:{BASE_IMAGE.replace(':', '-')}"
    built = build_and_push(work, normal)
    optimized = optimize_and_push(work, normal, esgz)
    base_built = False
    base_optimized = False
    prefetch = None
    if PARTIAL_BASE_CACHE:
        base_built = push_base_image(base_normal)
        base_optimized = optimize_and_push(work, base_normal, base_esgz)
        prefetch = base_esgz
    rows = [sysbox_operation(esgz, op, work, prefetch) for op in ["first_user_output", "sequential_read", "random_read"]]
    summary = {
        "started": started,
        "work_dir": str(work),
        "normal_image": normal,
        "esgz_image": esgz,
        "built": built,
        "optimized": optimized,
        "partial_base_cache": PARTIAL_BASE_CACHE,
        "base_normal_image": base_normal if PARTIAL_BASE_CACHE else "",
        "base_esgz_image": base_esgz if PARTIAL_BASE_CACHE else "",
        "base_built": base_built,
        "base_optimized": base_optimized,
        "rows": rows,
    }
    write_results(started, rows, summary)
    print(markdown_table(rows))
    print("results_file=%s" % RESULTS_FILE)


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print("error: %s" % err, file=sys.stderr)
        sys.exit(1)

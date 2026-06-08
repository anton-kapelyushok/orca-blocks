#!/usr/bin/env python3
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


WORK_DIR = Path(os.environ.get("COLD_IMAGE_WORK_DIR", ".tmp/cold-image-startup"))
RESULTS_FILE = Path(os.environ.get("COLD_IMAGE_RESULTS_FILE", "docs/benchmarks/cold-image-startup-results.txt"))
IMAGE_NAME = os.environ.get("COLD_IMAGE_NAME", "orca-cold-image")
IMAGE_TAG = os.environ.get("COLD_IMAGE_TAG", time.strftime("run-%Y%m%dT%H%M%SZ", time.gmtime()))
BLOB_MB = int(os.environ.get("COLD_IMAGE_BLOB_MB", "256"))
ROOTFS_SIZE_MB = int(os.environ.get("COLD_IMAGE_ROOTFS_SIZE_MB", "1024"))
MEMORY_MIB = int(os.environ.get("COLD_IMAGE_MEMORY_MIB", "1024"))
VCPU_COUNT = int(os.environ.get("COLD_IMAGE_VCPU_COUNT", "1"))
CONTROL_URL = os.environ.get("CONTROL_URL", "http://localhost:18080").rstrip("/")
REGISTRY_UPSTREAM_PORT = int(os.environ.get("COLD_REGISTRY_UPSTREAM_PORT", "15110"))
REGISTRY_PROXY_PORT = int(os.environ.get("COLD_REGISTRY_PROXY_PORT", "15111"))
TOXIPROXY_API_PORT = int(os.environ.get("COLD_REGISTRY_TOXIPROXY_API_PORT", "18475"))
TOXIPROXY_LATENCY_MS = int(os.environ.get("COLD_REGISTRY_TOXIPROXY_LATENCY_MS", "20"))
TOXIPROXY_BANDWIDTH_KBPS = int(os.environ.get("COLD_REGISTRY_TOXIPROXY_BANDWIDTH_KBPS", "1250"))
STACK_TIMEOUT_SECONDS = int(os.environ.get("COLD_STACK_TIMEOUT_SECONDS", "240"))
REQUEST_TIMEOUT_SECONDS = int(os.environ.get("COLD_REQUEST_TIMEOUT_SECONDS", "900"))
DROP_STACK = os.environ.get("COLD_IMAGE_DROP_STACK", "true").lower() in {"1", "true", "yes", "on"}
BUILD_STACK = os.environ.get("COLD_IMAGE_BUILD_STACK", "true").lower() in {"1", "true", "yes", "on"}
KEEP_REGISTRY = os.environ.get("COLD_IMAGE_KEEP_REGISTRY", "true").lower() in {"1", "true", "yes", "on"}
RESET_REGISTRY = os.environ.get("COLD_IMAGE_RESET_REGISTRY", "false").lower() in {"1", "true", "yes", "on"}
STACK_BIN_DIR = os.environ.get("COLD_STACK_BIN_DIR", "").strip()
STARGZ_VERSION = os.environ.get("STARGZ_VERSION", "v0.18.2")
STARGZ_TOOLS_DIR = Path(os.environ.get("STARGZ_TOOLS_DIR", ".tmp/stargz-tools"))
MARKER = "ORCA_COLD_READY"


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def run(cmd, check=True, timeout=None):
    print(f"$ {cmd}", flush=True)
    result = subprocess.run(
        cmd,
        shell=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=timeout,
    )
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n", flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def http_json(method, url, payload=None, timeout=REQUEST_TIMEOUT_SECONDS):
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["content-type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as err:
        body = err.read().decode(errors="replace")
        raise RuntimeError(f"{method} {url} failed: {err.code} {err.reason}: {body}") from err


def wait_http(url, timeout=STACK_TIMEOUT_SECONDS):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=5) as resp:
                if 200 <= resp.status < 500:
                    return
        except Exception as err:
            last = str(err)
        time.sleep(1)
    raise TimeoutError(f"timed out waiting for {url}: {last}")


def setup_registry_toxiproxy():
    if RESET_REGISTRY:
        run("docker rm -f orca-cold-registry orca-cold-registry-toxiproxy >/dev/null 2>&1 || true", check=False)
    registry_running = run("docker inspect -f '{{.State.Running}}' orca-cold-registry 2>/dev/null || true", check=False).strip()
    if registry_running != "true":
        run("docker rm -f orca-cold-registry >/dev/null 2>&1 || true", check=False)
        run(
            "docker run -d --name orca-cold-registry --network host "
            "-e REGISTRY_HTTP_ADDR=127.0.0.1:%d registry:2" % REGISTRY_UPSTREAM_PORT
        )
    proxy_running = run("docker inspect -f '{{.State.Running}}' orca-cold-registry-toxiproxy 2>/dev/null || true", check=False).strip()
    if proxy_running != "true":
        run("docker rm -f orca-cold-registry-toxiproxy >/dev/null 2>&1 || true", check=False)
        run(
            "docker run -d --name orca-cold-registry-toxiproxy --network host "
            "ghcr.io/shopify/toxiproxy:latest -host=127.0.0.1 -port=%d" % TOXIPROXY_API_PORT
        )
    wait_http(f"http://127.0.0.1:{TOXIPROXY_API_PORT}/proxies", timeout=60)
    proxy_api = f"http://127.0.0.1:{TOXIPROXY_API_PORT}"
    run("curl -fsS -X DELETE %s/proxies/cold-registry >/dev/null 2>&1 || true" % q(proxy_api), check=False)
    proxy = {
        "name": "cold-registry",
        "listen": f"127.0.0.1:{REGISTRY_PROXY_PORT}",
        "upstream": f"127.0.0.1:{REGISTRY_UPSTREAM_PORT}",
    }
    run(
        "curl -fsS -X POST %s/proxies -H 'content-type: application/json' -d %s"
        % (q(proxy_api), q(json.dumps(proxy)))
    )
    latency = {
        "name": "registry-downstream-latency",
        "type": "latency",
        "stream": "downstream",
        "toxicity": 1,
        "attributes": {"latency": TOXIPROXY_LATENCY_MS, "jitter": 0},
    }
    bandwidth = {
        "name": "registry-downstream-bandwidth",
        "type": "bandwidth",
        "stream": "downstream",
        "toxicity": 1,
        "attributes": {"rate": TOXIPROXY_BANDWIDTH_KBPS},
    }
    for toxic in [latency, bandwidth]:
        run(
            "curl -fsS -X POST %s/proxies/cold-registry/toxics -H 'content-type: application/json' -d %s"
            % (q(proxy_api), q(json.dumps(toxic)))
        )
    wait_http(f"http://127.0.0.1:{REGISTRY_PROXY_PORT}/v2/", timeout=60)


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


class ContainerdNode:
    def __init__(self, name, work, ctr_remote, snapshotter_bin=None):
        self.name = name
        self.work = (work / name).resolve()
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
        config_args = []
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
            deadline = time.time() + 60
            while time.time() < deadline and not self.stargz_sock.exists():
                if self.stargz_proc.poll() is not None:
                    raise RuntimeError("stargz exited early:\n%s" % (self.work / "stargz.log").read_text(errors="replace"))
                time.sleep(0.1)
            if not self.stargz_sock.exists():
                raise TimeoutError("stargz socket did not appear")
            self.config.write_text(
                'version = 2\n'
                '[proxy_plugins]\n'
                '  [proxy_plugins.stargz]\n'
                '    type = "snapshot"\n'
                f'    address = "{self.stargz_sock}"\n',
                encoding="utf-8",
            )
            config_args = ["--config", str(self.config)]
        self.proc = subprocess.Popen(
            ["containerd", "--address", str(self.sock), "--root", str(self.root), "--state", str(self.state), "--log-level", "warn", *config_args],
            stdout=self.log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        deadline = time.time() + 60
        while time.time() < deadline:
            if self.sock.exists():
                return
            if self.proc.poll() is not None:
                raise RuntimeError("containerd exited early:\n%s" % (self.work / "containerd.log").read_text(errors="replace"))
            time.sleep(0.1)
        raise TimeoutError("containerd socket did not appear")

    def ctr(self, args, check=True, timeout=None):
        return run("%s --address %s --namespace default %s" % (q(self.ctr_remote), q(self.sock), args), check=check, timeout=timeout)

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


def cleanup_registry_toxiproxy():
    if not KEEP_REGISTRY:
        run("docker rm -f orca-cold-registry orca-cold-registry-toxiproxy >/dev/null 2>&1 || true", check=False)


def make_image_refs():
    normal_direct = f"127.0.0.1:{REGISTRY_UPSTREAM_PORT}/{IMAGE_NAME}:{IMAGE_TAG}"
    normal_proxy = f"127.0.0.1:{REGISTRY_PROXY_PORT}/{IMAGE_NAME}:{IMAGE_TAG}"
    stargz_direct = f"127.0.0.1:{REGISTRY_UPSTREAM_PORT}/{IMAGE_NAME}-esgz:{IMAGE_TAG}"
    stargz_proxy = f"127.0.0.1:{REGISTRY_PROXY_PORT}/{IMAGE_NAME}-esgz:{IMAGE_TAG}"
    return normal_direct, normal_proxy, stargz_direct, stargz_proxy


def build_and_push_test_image(direct_ref, proxy_ref):
    if registry_has_image(direct_ref):
        print(f"reusing normal image {direct_ref}", flush=True)
        remove_local_images(direct_ref, proxy_ref)
        return
    context = WORK_DIR / "image-context"
    context.mkdir(parents=True, exist_ok=True)
    blob = context / "blob.dat"
    dockerfile = context / "Dockerfile"
    if not blob.exists() or blob.stat().st_size != BLOB_MB * 1024 * 1024:
        blob.unlink(missing_ok=True)
        run("dd if=/dev/urandom of=%s bs=1M count=%d status=none" % (q(blob), BLOB_MB))
    dockerfile.write_text(
        "FROM alpine:3.22\n"
        "COPY blob.dat /blob.dat\n"
        "RUN ls -lh /blob.dat\n"
        "CMD [\"/bin/sh\", \"-lc\", \"test -s /blob.dat && echo ORCA_COLD_READY\"]\n",
        encoding="utf-8",
    )
    run("docker build --network host -t %s %s" % (q(direct_ref), q(context)))
    run("docker push %s" % q(direct_ref))
    remove_local_images(direct_ref, proxy_ref)


def registry_has_image(ref):
    if "/" not in ref or ":" not in ref.rsplit("/", 1)[-1]:
        return False
    host_and_repo, tag = ref.rsplit(":", 1)
    host, repo = host_and_repo.split("/", 1)
    url = "http://%s/v2/%s/manifests/%s" % (host, repo, tag)
    out = run(
        "curl -fsS -H %s %s >/dev/null 2>&1 && echo yes || true"
        % (
            q("Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"),
            q(url),
        ),
        check=False,
    ).strip()
    return out == "yes"


def optimize_lazy_image(normal_ref, stargz_ref, ctr_remote):
    if registry_has_image(stargz_ref):
        print(f"reusing lazy image {stargz_ref}", flush=True)
        return
    optimizer = ContainerdNode("optimizer", WORK_DIR, ctr_remote)
    try:
        optimizer.start()
        optimizer.ctr("images pull --plain-http %s" % q(normal_ref), timeout=REQUEST_TIMEOUT_SECONDS)
        optimizer.ctr("images optimize --oci %s %s" % (q(normal_ref), q(stargz_ref)), timeout=REQUEST_TIMEOUT_SECONDS)
        optimizer.ctr("images push --plain-http %s" % q(stargz_ref), timeout=REQUEST_TIMEOUT_SECONDS)
    finally:
        optimizer.stop()


def remove_local_images(*refs):
    for ref in refs:
        run("docker image rm -f %s >/dev/null 2>&1 || true" % q(ref), check=False)
    run("docker builder prune -af >/dev/null 2>&1 || true", check=False)
    run("docker image prune -af >/dev/null 2>&1 || true", check=False)


def restart_stack():
    if DROP_STACK:
        run("docker compose down -v --remove-orphans", check=False, timeout=300)
    env = (
        "TOXIPROXY_S3_TOXICS_ENABLED=true "
        f"TOXIPROXY_S3_LATENCY_MS={TOXIPROXY_LATENCY_MS} "
        f"TOXIPROXY_S3_BANDWIDTH_KBPS={TOXIPROXY_BANDWIDTH_KBPS} "
    )
    if BUILD_STACK:
        manual_build_stack_images()
    run(env + "docker compose up -d --no-build", timeout=900)
    wait_http(f"{CONTROL_URL}/healthz", timeout=STACK_TIMEOUT_SECONDS)
    wait_http("http://127.0.0.1:18474/proxies", timeout=60)


def manual_build_stack_images():
    project = Path.cwd().name
    if STACK_BIN_DIR:
        manual_build_stack_images_from_bins(project, Path(STACK_BIN_DIR))
        return
    specs = [
        ("base-image-service", "./cmd/base-image-service"),
        ("control-service", "./cmd/control-service"),
        ("node-1", "./cmd/node-service"),
        ("node-2", "./cmd/node-service"),
        ("sandbox-service", "./cmd/sandbox-service"),
    ]
    for service, target in specs:
        image = f"{project}-{service}"
        run(
            "docker build --network host "
            "--build-arg TARGET_CMD=%s --build-arg TARGET=%s "
            "-t %s ."
            % (q(service), q(target), q(image)),
            timeout=900,
        )


def manual_build_stack_images_from_bins(project, bin_dir):
    bin_dir = bin_dir.resolve()
    init_bin = bin_dir / "orca-init"
    specs = [
        ("base-image-service", "base-image-service"),
        ("control-service", "control-service"),
        ("node-1", "node-service"),
        ("node-2", "node-service"),
        ("sandbox-service", "sandbox-service"),
    ]
    dockerfile = bin_dir / "Dockerfile.runtime"
    dockerfile.write_text(
        "FROM alpine:3.22\n"
        "RUN apk add --no-cache docker-cli e2fsprogs iproute2 iptables kmod nbd tar util-linux\n"
        "ARG SERVICE_BIN\n"
        "COPY ${SERVICE_BIN} /service\n"
        "COPY orca-init /orca-init\n"
        "ENTRYPOINT [\"/service\"]\n",
        encoding="utf-8",
    )
    for service, service_bin in specs:
        image = f"{project}-{service}"
        if not (bin_dir / service_bin).exists():
            raise RuntimeError(f"missing prebuilt service binary: {bin_dir / service_bin}")
        if not init_bin.exists():
            raise RuntimeError(f"missing prebuilt orca-init binary: {init_bin}")
        run(
            "docker build --network host --build-arg SERVICE_BIN=%s -f %s -t %s %s"
            % (q(service_bin), q(dockerfile), q(image), q(bin_dir)),
            timeout=300,
        )


def timing_value(session, name):
    raw = session.get("firecracker_timings", "[]")
    try:
        timings = json.loads(raw) if isinstance(raw, str) else raw
    except Exception:
        return None
    for item in timings or []:
        if item.get("name") == name:
            return item.get("duration_ms")
    return None


def measure_orca(proxy_ref):
    build_payload = {"image": proxy_ref, "rootfs_size_mb": ROOTFS_SIZE_MB}
    build_start = now_ms()
    built = http_json("POST", f"{CONTROL_URL}/buildImage", build_payload, timeout=REQUEST_TIMEOUT_SECONDS)
    build_wall = now_ms() - build_start
    command = "test -s /blob.dat && ls -lh /blob.dat && echo %s" % MARKER
    start_payload = {
        "image": proxy_ref,
        "command": command,
        "force_node": "node-1",
        "cpu_count": VCPU_COUNT,
        "memory_mib": MEMORY_MIB,
    }
    start = now_ms()
    session = http_json("POST", f"{CONTROL_URL}/startEnv", start_payload, timeout=REQUEST_TIMEOUT_SECONDS)
    start_wall = now_ms() - start
    output = str(session.get("firecracker_output", ""))
    if MARKER not in output:
        raise RuntimeError("Orca start did not print marker; output tail:\n%s" % output[-4000:])
    return {
        "build_wall_ms": build_wall,
        "build_duration_ms": built.get("duration_ms"),
        "build_timings": built.get("build_timings"),
        "base_image_id": built.get("base_image_id"),
        "rootfs_size_bytes": built.get("rootfs_size_bytes"),
        "start_wall_ms": start_wall,
        "request_to_first_user_output_ms": timing_value(session, "request_to_first_user_output"),
        "run_firecracker_ms": timing_value(session, "run_firecracker"),
        "session_id": session.get("session_id"),
        "env_id": session.get("env_id"),
    }


def measure_lazy_no_vm(proxy_ref, ctr_remote, snapshotter_bin):
    work = WORK_DIR / "lazy-node"
    run("rm -rf %s" % q(work), check=False)
    node = ContainerdNode("lazy-node", WORK_DIR, ctr_remote, snapshotter_bin=snapshotter_bin)
    try:
        node.start()
        start = now_ms()
        node.ctr("images rpull --plain-http --snapshotter stargz %s" % q(proxy_ref), timeout=REQUEST_TIMEOUT_SECONDS)
        rpull_ms = now_ms() - start
        cname = "cold-lazy-" + str(int(time.time() * 1000))
        run_start = now_ms()
        out = node.ctr(
            "run --rm --net-host --snapshotter stargz %s %s /bin/sh -lc %s"
            % (q(proxy_ref), q(cname), q("test -s /blob.dat && echo ORCA_COLD_READY")),
            timeout=REQUEST_TIMEOUT_SECONDS,
        )
        run_ms = now_ms() - run_start
    finally:
        node.stop()
    wall = rpull_ms + run_ms
    if MARKER not in out:
        raise RuntimeError("Lazy no-VM run did not print marker; output:\n%s" % out)
    return {"pull_run_wall_ms": wall, "rpull_ms": rpull_ms, "run_ms": run_ms, "output": out.strip()}


def measure_docker(proxy_ref):
    remove_local_images(proxy_ref)
    start = now_ms()
    out = run("docker run --rm %s" % q(proxy_ref), timeout=REQUEST_TIMEOUT_SECONDS)
    wall = now_ms() - start
    if MARKER not in out:
        raise RuntimeError("Docker run did not print marker; output:\n%s" % out)
    return {"pull_run_wall_ms": wall, "output": out.strip()}


def format_build_steps(steps):
    out = []
    for step in steps or []:
        out.append((step.get("name", ""), step.get("duration_ms", "")))
    return out


def write_results(normal_direct_ref, normal_proxy_ref, stargz_direct_ref, stargz_proxy_ref, orca, lazy):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        "cold-image startup benchmark",
        "============================",
        "",
        f"date={now_utc()}",
        f"normal_image_direct={normal_direct_ref}",
        f"normal_image_proxy={normal_proxy_ref}",
        f"lazy_image_direct={stargz_direct_ref}",
        f"lazy_image_proxy={stargz_proxy_ref}",
        f"blob_mb={BLOB_MB}",
        f"rootfs_size_mb={ROOTFS_SIZE_MB}",
        f"toxiproxy_latency_ms={TOXIPROXY_LATENCY_MS}",
        f"toxiproxy_bandwidth_kbps={TOXIPROXY_BANDWIDTH_KBPS}",
        "",
        "Summary",
        "-------",
        "",
        "Path                 Operation              Time",
        "-------------------  --------------------  --------",
        f"Orca VM+NBD          buildImage            {orca['build_wall_ms']:7d} ms",
        f"Orca VM+NBD          startEnv              {orca['start_wall_ms']:7d} ms",
        f"Orca VM+NBD          first_user_output     {str(orca.get('request_to_first_user_output_ms')):>7s} ms",
        f"No VM lazy layers    rpull                 {lazy['rpull_ms']:7d} ms",
        f"No VM lazy layers    run                   {lazy['run_ms']:7d} ms",
        f"No VM lazy layers    rpull+run             {lazy['pull_run_wall_ms']:7d} ms",
        "",
        "Orca buildImage steps",
        "---------------------",
        "",
        "Step                           Time",
        "-----------------------------  --------",
    ]
    for name, duration in format_build_steps(orca.get("build_timings")):
        lines.append(f"{name:<29}  {str(duration):>7s} ms")
    lines.extend([
        "",
        "Details",
        "-------",
        "",
        json.dumps({"orca": orca, "lazy_no_vm": lazy}, sort_keys=True),
        "",
    ])
    RESULTS_FILE.write_text("\n".join(lines), encoding="utf-8")
    print("\n".join(lines), flush=True)


def main():
    WORK_DIR.mkdir(parents=True, exist_ok=True)
    normal_direct_ref, normal_proxy_ref, stargz_direct_ref, stargz_proxy_ref = make_image_refs()
    ctr_remote, snapshotter_bin = ensure_stargz_tools()
    try:
        setup_registry_toxiproxy()
        build_and_push_test_image(normal_direct_ref, normal_proxy_ref)
        optimize_lazy_image(normal_direct_ref, stargz_direct_ref, ctr_remote)
        restart_stack()
        remove_local_images(normal_proxy_ref, stargz_proxy_ref)
        orca = measure_orca(normal_proxy_ref)
        lazy = measure_lazy_no_vm(stargz_proxy_ref, ctr_remote, snapshotter_bin)
        write_results(normal_direct_ref, normal_proxy_ref, stargz_direct_ref, stargz_proxy_ref, orca, lazy)
    finally:
        cleanup_registry_toxiproxy()


if __name__ == "__main__":
    try:
        main()
    except Exception as err:
        print(f"error: {err}", file=sys.stderr)
        sys.exit(1)

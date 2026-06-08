#!/usr/bin/env python3
import json
import os
import shlex
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


IMAGE = os.environ.get("IMAGE", "registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643")
REGISTRY = os.environ.get("REGISTRY", "127.0.0.1:5000")
REPO = os.environ.get("REPO", "orca/overlaybd-jb-real")
TAG_SUFFIX = os.environ.get("TAG_SUFFIX", datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"))
NORMAL_REF = os.environ.get("NORMAL_REF", f"{REGISTRY}/{REPO}:normal-{TAG_SUFFIX}")
NAMESPACE = os.environ.get("NAMESPACE", "moby")
CTR = os.environ.get("CTR", "/opt/overlaybd/snapshotter/ctr")
RUNTIME = os.environ.get("CTR_RUN_RUNTIME", "io.containerd.runc.v2")
RUNC_BINARY = os.environ.get("CTR_RUN_RUNC_BINARY", "")
RUNTIME_LABEL = os.environ.get("RUNTIME_LABEL", "sysbox-runc" if RUNC_BINARY else "runc")
CTR_NET_MODE = os.environ.get("CTR_NET_MODE", "cni" if RUNC_BINARY else "host")
ALLOW_NEW_PRIVS = os.environ.get("ALLOW_NEW_PRIVS", "0").lower() in {"1", "true", "yes", "on"}
SKIP_RUN = os.environ.get("SKIP_RUN", "0").lower() in {"1", "true", "yes", "on"}
RESULTS_FILE = Path(os.environ.get("RESULTS_FILE", f"/root/overlaybd-jb-real-{RUNTIME_LABEL}-{TAG_SUFFIX}.md"))
LOG_FILE = Path(os.environ.get("LOG_FILE", f"/root/overlaybd-jb-real-{RUNTIME_LABEL}-{TAG_SUFFIX}.log"))
TIMEOUT_SECONDS = int(os.environ.get("TIMEOUT_SECONDS", "300"))
SKIP_CONVERT = os.environ.get("SKIP_CONVERT", "0").lower() in {"1", "true", "yes", "on"}
SKIP_MIRROR = os.environ.get("SKIP_MIRROR", "0").lower() in {"1", "true", "yes", "on"}
DB_STR = os.environ.get("DB_STR", "overlaybd:overlaybd@tcp(127.0.0.1:3306)/overlaybd")
OBD_REF = os.environ.get("OBD_REF", f"{REGISTRY}/{REPO}:obd-{TAG_SUFFIX}")
REWRITE_REPO_BLOB_URL_SCHEME = os.environ.get("REWRITE_REPO_BLOB_URL_SCHEME", "").strip()
SNAPSHOT_ROOT = Path(os.environ.get("OVERLAYBD_SNAPSHOT_ROOT", "/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd"))

JOIN_MARKER = "Join this workspace using URL:"
MARKERS = [
    "Dock HTTP Api listening",
    "Workspace Server listening",
    "Version: 261.643",
    "Smart Mode: enabled",
    "Published to JetBrains Relay: true",
    JOIN_MARKER,
]


def q(value):
    return shlex.quote(str(value))


def now_ms():
    return time.time_ns() // 1_000_000


def now_utc():
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def run(cmd, check=True, timeout=None):
    print(f"$ {cmd}", flush=True)
    result = subprocess.run(cmd, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    if check and result.returncode != 0:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n")
        raise subprocess.CalledProcessError(result.returncode, cmd, output=result.stdout)
    return result.stdout


def require_clean_mounts():
    out = run("findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true", check=False)
    if out.strip():
        raise RuntimeError(f"OverlayBD/SCSI mounts are active:\n{out}")


def wait_for_clean_mounts(timeout_seconds=30):
    deadline = time.time() + timeout_seconds
    last = ""
    while time.time() < deadline:
        last = run("findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true", check=False)
        if not last.strip():
            return
        time.sleep(1)
    raise RuntimeError(f"OverlayBD/SCSI mounts stayed active:\n{last}")


def registry_size_bytes():
    out = run("du -sb /var/lib/docker-registry 2>/dev/null | cut -f1 || echo 0", check=False).strip()
    return int(out) if out.isdigit() else 0


def registry_blob_count():
    out = run("find /var/lib/docker-registry/docker/registry/v2/blobs -type f 2>/dev/null | wc -l", check=False).strip()
    return int(out) if out.isdigit() else 0


def convert_image():
    if SKIP_CONVERT:
        return {"skipped": True, "elapsed_ms": None, "registry_delta_bytes": None, "blob_delta": None}

    before_bytes = registry_size_bytes()
    before_blobs = registry_blob_count()
    start = now_ms()
    if not SKIP_MIRROR:
        run(f"docker pull {q(IMAGE)}", timeout=TIMEOUT_SECONDS * 4)
        run(f"docker tag {q(IMAGE)} {q(NORMAL_REF)}", timeout=TIMEOUT_SECONDS)
        run(f"docker push {q(NORMAL_REF)}", timeout=TIMEOUT_SECONDS * 4)
    run(f"{q(CTR)} -n {q(NAMESPACE)} images pull --local --plain-http {q(NORMAL_REF)}", timeout=TIMEOUT_SECONDS)
    run(
        f"{q(CTR)} -n {q(NAMESPACE)} obdconv --plain-http --fstype ext4 --dbstr {q(DB_STR)} {q(NORMAL_REF)} {q(OBD_REF)}",
        timeout=TIMEOUT_SECONDS * 4,
    )
    run(f"{q(CTR)} -n {q(NAMESPACE)} images push --local --plain-http {q(OBD_REF)}", timeout=TIMEOUT_SECONDS * 4)
    elapsed = now_ms() - start
    after_bytes = registry_size_bytes()
    after_blobs = registry_blob_count()
    return {
        "skipped": False,
        "elapsed_ms": elapsed,
        "registry_delta_bytes": after_bytes - before_bytes,
        "blob_delta": after_blobs - before_blobs,
    }


def rpull_image():
    start = now_ms()
    run(f"{q(CTR)} -n {q(NAMESPACE)} rpull --plain-http {q(OBD_REF)}", timeout=TIMEOUT_SECONDS)
    return now_ms() - start


def rewrite_repo_blob_url_scheme():
    if not REWRITE_REPO_BLOB_URL_SCHEME:
        return {"skipped": True, "elapsed_ms": None, "changed": 0}

    start = now_ms()
    changed = 0
    for path in SNAPSHOT_ROOT.glob("snapshots/*/block/config.v1.json"):
        text = path.read_text(encoding="utf-8")
        new_text = text.replace('"repoBlobUrl":"https://', f'"repoBlobUrl":"{REWRITE_REPO_BLOB_URL_SCHEME}://')
        if new_text != text:
            path.write_text(new_text, encoding="utf-8")
            changed += 1
    return {"skipped": False, "elapsed_ms": now_ms() - start, "changed": changed}


def run_workspace():
    name = f"overlaybd-jb-real-{RUNTIME_LABEL}-{int(time.time())}"
    cmd = [
        CTR,
        "-n",
        NAMESPACE,
        "run",
        "--snapshotter",
        "overlaybd",
        "--runtime",
        RUNTIME,
    ]
    if CTR_NET_MODE == "host":
        cmd += ["--net-host"]
    elif CTR_NET_MODE == "cni":
        cmd += ["--cni"]
    elif CTR_NET_MODE == "none":
        pass
    else:
        raise RuntimeError(f"unknown CTR_NET_MODE={CTR_NET_MODE!r}")
    if ALLOW_NEW_PRIVS:
        cmd += ["--allow-new-privs"]
    if RUNC_BINARY:
        cmd += ["--runc-binary", RUNC_BINARY]
    cmd += ["--rm", OBD_REF, name]

    start = now_ms()
    first_seen = {}
    output_chunks = []
    with LOG_FILE.open("w", encoding="utf-8", errors="replace") as log:
        print("$ " + " ".join(q(part) for part in cmd), flush=True)
        process = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
        try:
            deadline = time.time() + TIMEOUT_SECONDS
            assert process.stdout is not None
            while time.time() < deadline:
                line = process.stdout.readline()
                if line:
                    elapsed = now_ms() - start
                    if "first_output_line" not in first_seen:
                        first_seen["first_output_line"] = elapsed
                        first_seen["first_output_text"] = line.rstrip()
                    output_chunks.append(line)
                    log.write(line)
                    log.flush()
                    sys.stdout.write(line)
                    sys.stdout.flush()
                    joined = "".join(output_chunks)
                    for marker in MARKERS:
                        if marker not in first_seen and marker in joined:
                            first_seen[marker] = elapsed
                    if JOIN_MARKER in joined:
                        return {
                            "elapsed_ms": now_ms() - start,
                            "first_seen_ms": first_seen,
                            "returncode": None,
                            "joined": True,
                        }
                elif process.poll() is not None:
                    break
                else:
                    time.sleep(0.1)
            return {
                "elapsed_ms": now_ms() - start,
                "first_seen_ms": first_seen,
                "returncode": process.poll(),
                "joined": False,
            }
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)


def fmt_ms(value):
    if value is None:
        return "skipped"
    return f"{value} ms / {value / 1000:.2f} s"


def fmt_bytes(value):
    if value is None:
        return "skipped"
    if abs(value) >= 1024 * 1024:
        return f"{value} bytes / {value / 1024 / 1024:.1f} MiB"
    return f"{value} bytes / {value / 1024:.1f} KiB"


def write_results(conversion, rpull_ms, rewrite_result, workspace):
    first_seen = workspace.get("first_seen_ms", {})
    with RESULTS_FILE.open("w", encoding="utf-8") as f:
        f.write("# OverlayBD JetBrains Workspace Real-Env Timing\n\n")
        f.write(f"Generated: `{now_utc()}`\n\n")
        f.write("| Field | Value |\n| --- | --- |\n")
        f.write(f"| Source image | `{IMAGE}` |\n")
        f.write(f"| Mirrored normal image | `{NORMAL_REF}` |\n")
        f.write(f"| OverlayBD image | `{OBD_REF}` |\n")
        f.write(f"| Runtime label | `{RUNTIME_LABEL}` |\n")
        f.write(f"| containerd runtime | `{RUNTIME}` |\n")
        f.write(f"| runc binary | `{RUNC_BINARY or 'default'}` |\n")
        f.write(f"| network mode | `{CTR_NET_MODE}` |\n")
        f.write(f"| allow new privileges | `{ALLOW_NEW_PRIVS}` |\n")
        f.write(f"| repoBlobUrl scheme rewrite | `{REWRITE_REPO_BLOB_URL_SCHEME or 'disabled'}` |\n")
        f.write(f"| skip run | `{SKIP_RUN}` |\n")
        f.write(f"| Marker | `{JOIN_MARKER}` |\n")
        f.write(f"| Log file | `{LOG_FILE}` |\n\n")
        f.write("## Publish / Pull\n\n")
        f.write("| Step | Time | Registry delta | Blob delta |\n| --- | ---: | ---: | ---: |\n")
        f.write(
            f"| Mirror and convert original image to OverlayBD | {fmt_ms(conversion.get('elapsed_ms'))} | "
            f"{fmt_bytes(conversion.get('registry_delta_bytes'))} | {conversion.get('blob_delta', 'skipped')} |\n"
        )
        f.write(f"| rpull OverlayBD image | {fmt_ms(rpull_ms)} | n/a | n/a |\n")
        f.write(
            f"| Rewrite OverlayBD repoBlobUrl scheme | {fmt_ms(rewrite_result.get('elapsed_ms'))} | "
            f"changed configs: {rewrite_result.get('changed', 'skipped')} | n/a |\n\n"
        )
        f.write("## Runtime Markers\n\n")
        f.write("| Marker | First seen |\n| --- | ---: |\n")
        first_output_line = first_seen.get("first_output_line")
        f.write(f"| first output line | {fmt_ms(first_output_line) if first_output_line is not None else 'not seen'} |\n")
        for marker in MARKERS:
            value = first_seen.get(marker)
            f.write(f"| `{marker}` | {fmt_ms(value) if value is not None else 'not seen'} |\n")
        f.write("\n")
        f.write("First output text:\n\n")
        f.write("```text\n")
        f.write(f"{first_seen.get('first_output_text', '')}\n")
        f.write("```\n\n")
        f.write("## Result\n\n")
        f.write("| Field | Value |\n| --- | ---: |\n")
        f.write(f"| Joined | `{workspace.get('joined')}` |\n")
        f.write(f"| Elapsed to Join URL / stop | {fmt_ms(workspace.get('elapsed_ms'))} |\n")
        f.write(f"| Process return code before stop | `{workspace.get('returncode')}` |\n")


def main():
    require_clean_mounts()
    conversion = convert_image()
    require_clean_mounts()
    if SKIP_RUN:
        rpull_ms = None
        rewrite_result = {"skipped": True, "elapsed_ms": None, "changed": 0}
        workspace = {
            "elapsed_ms": None,
            "first_seen_ms": {},
            "returncode": None,
            "joined": False,
        }
    else:
        rpull_ms = rpull_image()
        rewrite_result = rewrite_repo_blob_url_scheme()
        require_clean_mounts()
        workspace = run_workspace()
        wait_for_clean_mounts()
    write_results(conversion, rpull_ms, rewrite_result, workspace)
    print(f"\nresults={RESULTS_FILE}")
    print(RESULTS_FILE.read_text())


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
import json
import os
import re
import shlex
import signal
import subprocess
import time
from pathlib import Path


WORK_DIR = Path(os.environ.get("FILE_NBD_WORK_DIR", ".tmp/file-vs-nbd"))
RESULTS_FILE = Path(os.environ.get("FILE_NBD_RESULTS_FILE", "docs/benchmarks/file-vs-nbd-results.txt"))
DISK_BENCH_BIN = Path(os.environ.get("FILE_NBD_DISK_BENCH_BIN", str(WORK_DIR / "disk-bench")))
SERVER_BIN = Path(os.environ.get("FILE_NBD_SERVER_BIN", str(WORK_DIR / "local-nbd-file-server")))
DATA_FILE = Path(os.environ.get("FILE_NBD_DATA_FILE", str(WORK_DIR / "bench.dat")))
SIZE_MB = int(os.environ.get("FILE_NBD_SIZE_MB", "256"))
RANDOM_OPS = int(os.environ.get("FILE_NBD_RANDOM_OPS", "16384"))
SEQ_BLOCK_BYTES = int(os.environ.get("FILE_NBD_SEQ_BLOCK_BYTES", str(1024 * 1024)))
RANDOM_BLOCK_BYTES = int(os.environ.get("FILE_NBD_RANDOM_BLOCK_BYTES", "4096"))
IO_DEPTH = int(os.environ.get("FILE_NBD_IO_DEPTH", "1"))
IO_DEPTHS = [
    int(v.strip())
    for v in os.environ.get("FILE_NBD_IO_DEPTHS", "").split(",")
    if v.strip()
]
DIRECT = os.environ.get("FILE_NBD_DIRECT", "false").lower() in {"1", "true", "yes", "on"}
DROP_CACHES = os.environ.get("FILE_NBD_DROP_CACHES", "false").lower() in {"1", "true", "yes", "on"}
NBD_ADDR = os.environ.get("FILE_NBD_ADDR", "127.0.0.1:10909")
EXPORT_NAME = os.environ.get("FILE_NBD_EXPORT", "bench")
MODES = [m.strip() for m in os.environ.get("FILE_NBD_MODES", "sequential,random").split(",") if m.strip()]
TARGETS = [t.strip() for t in os.environ.get("FILE_NBD_TARGETS", "file,loop,nbd").split(",") if t.strip()]


def run(cmd, check=True):
    print(f"$ {cmd}", flush=True)
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


def extract_result(output):
    match = re.search(r"DISK_BENCH_RESULT=(\{[^\r\n]*\})", output)
    if not match:
        raise RuntimeError("DISK_BENCH_RESULT marker not found:\n%s" % output[-4000:])
    return json.loads(match.group(1))


def maybe_build_binaries():
    if not DISK_BENCH_BIN.exists():
        print(f"building {DISK_BENCH_BIN}", flush=True)
        run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/disk-bench" % q(DISK_BENCH_BIN))
    if not SERVER_BIN.exists():
        print(f"building {SERVER_BIN}", flush=True)
        run("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o %s ./cmd/local-nbd-file-server" % q(SERVER_BIN))


def prepare_data_file():
    size_bytes = SIZE_MB * 1024 * 1024
    DATA_FILE.parent.mkdir(parents=True, exist_ok=True)
    if DATA_FILE.exists() and DATA_FILE.stat().st_size == size_bytes:
        print(f"reusing {DATA_FILE}", flush=True)
        return
    print(f"creating {SIZE_MB} MiB data file at {DATA_FILE}", flush=True)
    DATA_FILE.unlink(missing_ok=True)
    run("dd if=/dev/urandom of=%s bs=1M count=%d status=none" % (q(DATA_FILE), SIZE_MB))
    run("sync", check=False)


def drop_caches():
    if not DROP_CACHES:
        return
    run("sync", check=False)
    run("sh -c 'echo 3 > /proc/sys/vm/drop_caches'", check=False)


def find_free_nbd():
    preferred = os.environ.get("FILE_NBD_DEVICE", "").strip()
    if preferred:
        return preferred
    for i in range(0, 32):
        dev = Path(f"/dev/nbd{i}")
        pid = Path(f"/sys/block/nbd{i}/pid")
        if dev.exists() and not pid.exists():
            return str(dev)
    raise RuntimeError("no free /dev/nbdX device found")


def wait_for_server(proc):
    deadline = time.time() + 10
    host, port = NBD_ADDR.rsplit(":", 1)
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("NBD server exited early")
        out = run("timeout 1 bash -lc '</dev/tcp/%s/%s' >/dev/null 2>&1; echo $?" % (q(host), q(port)), check=False).strip()
        if out.endswith("0"):
            return
        time.sleep(0.1)
    raise TimeoutError(f"timed out waiting for NBD server at {NBD_ADDR}")


def attach_nbd(device):
    host, port = NBD_ADDR.rsplit(":", 1)
    run("nbd-client %s %s %s -N %s" % (q(host), q(port), q(device), q(EXPORT_NAME)))


def detach_nbd(device):
    run("nbd-client -d %s" % q(device), check=False)


def attach_loop():
    return run("losetup --find --show --read-only %s" % q(DATA_FILE)).strip()


def detach_loop(device):
    if device:
        run("losetup -d %s" % q(device), check=False)


def bench(kind, path, mode, io_depth):
    block_bytes = SEQ_BLOCK_BYTES if mode == "sequential" else RANDOM_BLOCK_BYTES
    args = [
        q(DISK_BENCH_BIN),
        "-kind", q(kind),
        "-mode", q(mode),
        "-path", q(path),
        "-size-bytes", str(SIZE_MB * 1024 * 1024),
        "-block-bytes", str(block_bytes),
        "-random-ops", str(RANDOM_OPS),
        "-io-depth", str(io_depth),
    ]
    if DIRECT:
        args.append("-direct")
    drop_caches()
    output = run(" ".join(args))
    result = extract_result(output)
    result["raw_output"] = output.strip()
    return result


def append_results(lines):
    RESULTS_FILE.parent.mkdir(parents=True, exist_ok=True)
    with RESULTS_FILE.open("a", encoding="utf-8") as f:
        for line in lines:
            f.write(line.rstrip() + "\n")


def main():
    WORK_DIR.mkdir(parents=True, exist_ok=True)
    maybe_build_binaries()
    prepare_data_file()

    device = find_free_nbd()
    detach_nbd(device)
    loop_device = ""
    server = subprocess.Popen(
        [
            str(SERVER_BIN),
            "-addr", NBD_ADDR,
            "-file", str(DATA_FILE),
            "-export", EXPORT_NAME,
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        preexec_fn=os.setsid,
    )
    rows = []
    try:
        wait_for_server(server)
        attach_nbd(device)
        if "loop" in TARGETS:
            loop_device = attach_loop()
        target_paths = {
            "file": str(DATA_FILE),
            "loop": loop_device,
            "nbd": device,
        }
        depths = IO_DEPTHS if IO_DEPTHS else [IO_DEPTH]
        for mode in MODES:
            for io_depth in depths:
                for label in TARGETS:
                    path = target_paths[label]
                    if not path:
                        continue
                    print(f"running {label} {mode} io_depth={io_depth}", flush=True)
                    result = bench(f"{label}-{mode}-qd{io_depth}", path, mode, io_depth)
                    row = {
                        "target": label,
                        "mode": mode,
                        "path": str(path),
                        "mb_per_sec": result["mb_per_sec"],
                        "iops": result["iops"],
                        "duration_ms": result["duration_ms"],
                        "ops": result["ops"],
                        "bytes_read": result["bytes_read"],
                        "block_bytes": result["block_bytes"],
                        "io_depth": result["io_depth"],
                        "checksum": result["checksum"],
                    }
                    rows.append(row)
                    print(json.dumps(row, sort_keys=True), flush=True)
    finally:
        detach_loop(loop_device)
        detach_nbd(device)
        if server.poll() is None:
            os.killpg(os.getpgid(server.pid), signal.SIGTERM)
            try:
                server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(os.getpgid(server.pid), signal.SIGKILL)
        if server.stdout:
            server_log = server.stdout.read()
            if server_log:
                (WORK_DIR / "local-nbd-file-server.log").write_text(server_log, encoding="utf-8")

    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    lines = [
        "",
        f"file-vs-nbd run {started}",
        f"data_file={DATA_FILE}",
        f"nbd_device={device}",
        f"loop_device={loop_device}",
        f"size_mb={SIZE_MB} random_ops={RANDOM_OPS} io_depths={','.join(str(d) for d in (IO_DEPTHS if IO_DEPTHS else [IO_DEPTH]))} direct={str(DIRECT).lower()} drop_caches={str(DROP_CACHES).lower()} targets={','.join(TARGETS)}",
        "",
        "target\tmode\tio_depth\tmb_per_sec\tiops\tduration_ms\tblock_bytes\tops\tbytes_read\tchecksum",
    ]
    for row in rows:
        lines.append(
            "{target}\t{mode}\t{io_depth}\t{mb_per_sec}\t{iops}\t{duration_ms}\t{block_bytes}\t{ops}\t{bytes_read}\t{checksum}".format(**row)
        )
    append_results(lines)
    print(f"wrote {RESULTS_FILE}", flush=True)


if __name__ == "__main__":
    main()

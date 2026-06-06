# Orca Blocks MVP

This repository contains a local Docker Compose prototype for the hard storage path of a remote-execution block backend. It emulates two execution nodes with independent persistent local caches, MinIO as durable S3-compatible chunk storage, and Postgres metadata for volumes, snapshots, and scheduling hints.

ublk, auth, encryption, Kubernetes, and advanced prefetch are intentionally out of scope. Firecracker is present as an MVP runtime for proving the lazy block path, not yet as a production VM product. The current runtimes are thin test/debug surfaces over the storage package. NBD is an internal device implementation for the mounted filesystem and Firecracker runtimes, plus a low-level protocol test path.

## Architecture

- `control-service`: creates volumes and schedules sessions. It tracks `last_node` in Postgres and prefers that node when it is healthy.
- `node-1` and `node-2`: expose the block backend HTTP API and run a local NBD listener used by node-owned block devices. Each node has its own Docker volume mounted as `/cache`.
- `minio`: stores immutable chunks under `chunks/{sha256}` and snapshot manifests under `manifests/{snapshot_id}.json`.
- `toxiproxy`: sits between nodes and MinIO so integration tests can add latency, bandwidth limits, or failures to the S3 path.
- `postgres`: stores volume metadata, latest snapshot pointers, snapshot records, and last-node hints.
- `pkg/storage`: reusable storage engine for chunk math, manifests, lazy reads, dirty overlays, commits, local cache lookup/fill, and LRU eviction.

## Commands

```sh
make up
make test
make demo
make down
make clean
```

The services are exposed on host ports `18080` (control), `18081` (node-1), `18082` (node-2), `19000` (MinIO API through Toxiproxy), `19001` (MinIO console), `19002` (direct MinIO API for debugging), and `18474` (Toxiproxy control API).

## Remote Linux/KVM Dev

Firecracker development needs Linux KVM. You can keep editing locally and run the KVM-dependent loop on a reachable Linux VM:

```sh
make remote-authorize-key REMOTE_HOST=vboxuser@192.168.178.201
make remote-check REMOTE_HOST=vboxuser@192.168.178.201
make remote-setup REMOTE_HOST=vboxuser@192.168.178.201
make remote-test REMOTE_HOST=vboxuser@192.168.178.201
make remote-demo REMOTE_HOST=vboxuser@192.168.178.201
```

For a forwarded SSH port, prefer the single `REMOTE_PORT` knob:

```sh
make remote-check REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
make remote-setup REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
make remote-test REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
```

`remote-authorize-key` installs your local SSH public key, defaulting to `~/.ssh/id_ed25519.pub`, into the remote user's `authorized_keys`. `remote-setup` first installs a dev-VM sudoers rule for the remote user so repeated setup runs do not prompt for a password, then verifies `/dev/kvm`, installs Docker Compose and development packages, enables Docker, configures the host to load the `nbd` module on boot, and checks that containers can see `/dev/kvm` and `/dev/nbd0`. If Docker only works through `sudo` immediately after setup, log out and back in or run `newgrp docker`.

Useful knobs:

```sh
REMOTE_HOST=vboxuser@192.168.178.201
REMOTE_PORT=2222
REMOTE_DIR=~/orca-blocks
LOCAL_PUBLIC_KEY=~/.ssh/id_ed25519.pub
REMOTE_SSH_OPTS="-p 2222"
REMOTE_TTY_SSH_OPTS="-tt"
REMOTE_SCP_OPTS="-P 2222"
REMOTE_RSYNC_SSH_OPTS="-p 2222"
```

## Runtimes

`runtime:"http-block"` is the default. It does not use NBD or mount anything; the HTTP read/write endpoints call the storage backend directly. This is useful for storage tests, cache demos, and debugging.

`runtime:"mounted-fs"` is the pre-Firecracker integration harness. Session start happens on the selected node, registers a session-local NBD export, attaches it to a free `/dev/nbdX` inside that node container, optionally formats it, and mounts it under `/mnt/orca-sessions/{session_id}`. The response includes `mount_path` and `nbd_device`. `POST /sessions/{id}/commit` unmounts, disconnects the NBD device, commits dirty chunks to a new snapshot, and releases the device. `POST /sessions/{id}/stop` unmounts and disconnects without committing.

NBD devices are host kernel devices, not image-local devices. `remote-setup` loads the host `nbd` module and configures `/dev/nbd0..15` on boot. The node containers run privileged so they can see those devices and perform test mounts. Compose gives each node a disjoint allocation range: node-1 uses `/dev/nbd0..7`, and node-2 uses `/dev/nbd8..15`. At startup, each node preflights `nbd-client`, `mkfs.ext4`, `mount`, `umount`, and visible NBD devices in its assigned range; if the host module or container device access is missing, the node fails with a direct setup error instead of a later opaque mount failure. Nodes also check `/dev/kvm` visibility before running Firecracker sessions.

`runtime:"nbd-export-test"` is a low-level protocol test harness. The node creates a session-local NBD export, but the caller attaches it. This preserves direct NBD coverage while keeping the future product path centered on node-owned device lifecycle.

## Firecracker Assets

The default Firecracker MVP boots a small initramfs and attaches the Orca lazy block volume as `/dev/vda`. This is intentionally tiny: it proves the VM-to-NBD-to-cache-to-MinIO storage path without paying for a full guest root filesystem on every test run.

```sh
make remote-firecracker-assets REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
make remote-firecracker-initramfs REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
make remote-firecracker-boot-check REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
```

`remote-firecracker-assets` downloads the Firecracker binary and a matching CI kernel into `firecracker-assets/`. The initramfs builder writes `firecracker-assets/initramfs.cpio.gz` from BusyBox and installs an `/init` script that understands these kernel args:

```text
orca.mode=smoke
orca.mode=write orca.payload_b64=... orca.data_dev=/dev/vda
orca.mode=read orca.payload_b64=... orca.data_dev=/dev/vda
```

The old Alpine rootfs path is still available for experiments:

```sh
make remote-firecracker-rootfs REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
FIRECRACKER_BOOT_MODE=rootfs make remote-firecracker-boot-check REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
```

In rootfs mode, the rootfs is the boot environment and the Orca volume is attached separately as `/dev/vdb`. The rootfs builder installs a VM-local Docker daemon and seeds an offline Alpine container image so `firecracker_mode:"docker-smoke"` and `firecracker_mode:"docker-read"` can run containers without guest networking or registry pulls. `docker-smoke` formats the Orca volume and writes `proof.txt` from inside a container; `docker-read` reconnects a Docker-capable VM and verifies the same file from inside a container. In initramfs mode there is no rootfs drive, so the Orca volume is `/dev/vda`.

Run the Docker-in-Firecracker smoke test from your Mac against the Linux VM:

```sh
make remote-firecracker-rootfs FORCE=true REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
make remote-firecracker-docker-test REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
```

`firecracker-rootfs` caches a Docker-installed Alpine base image under `firecracker-assets/rootfs-base-*.ext4`; `FORCE=true` regenerates only the final rootfs from that cache. Use `REBUILD_BASE=true` when Alpine packages, rootfs size, or the offline image seed should be rebuilt. `remote-firecracker-docker-test` starts existing Compose images by default; add `COMPOSE_BUILD=true` when node/control service code changed.

`runtime:"firecracker"` keeps per-session debug data on the selected node under `/sessions/firecracker/{session_id}`. The retained directory includes `firecracker.json`, `firecracker.log`, `serial.log`, a copied boot artifact, and `timings.json` with step durations for preflight, NBD attach/detach, VM run, flush, and commit. Firecracker write sessions only save node-local `memory.snap` and `vmstate.snap` files when `save_memory_snapshot:true` is requested. The MVP can restore those files on the same node with `firecracker_mode:"restore"` plus the saved memory path, VM state path, and original NBD device path. These memory snapshots are deliberately not uploaded to MinIO yet; durable cross-node truth remains the volume snapshot/chunk manifest path. In Compose these debug directories are backed by per-node persistent volumes, separate from the chunk cache volumes.

Run the focused Firecracker integration test from your Mac against the Linux VM:

```sh
make remote-sync REMOTE_HOST=vboxuser@192.168.178.134 REMOTE_PORT=2222
ssh -p 2222 vboxuser@192.168.178.134 \
  'cd ~/orca-blocks && FORCE=true make firecracker-initramfs && TOXIPROXY_S3_TOXICS_ENABLED=false docker compose up --build -d node-1 node-2 control-service && GOCACHE=$(pwd)/.gocache go test -count=1 -v -tags=integration ./integration -run TestFirecrackerSessionNodeOneThenNodeTwo'
```

Example mounted filesystem session:

```json
{
  "volume_id": "mount-demo",
  "force_node": "node-1",
  "runtime": "mounted-fs",
  "format": true,
  "fs_type": "ext4"
}
```

Example low-level NBD protocol test flow on a Linux machine with NBD tools:

```sh
sudo modprobe nbd
curl -sS -X POST localhost:18080/volumes/create \
  -H 'content-type: application/json' \
  -d '{"volume_id":"nbd-demo","size_bytes":1073741824,"chunk_size":4194304}'
SESSION_JSON=$(curl -sS -X POST localhost:18080/sessions/start \
  -H 'content-type: application/json' \
  -d '{"volume_id":"nbd-demo","force_node":"node-1","runtime":"nbd-export-test","commit_on_disconnect":true}')
EXPORT_NAME=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["nbd_export_name"])' <<< "$SESSION_JSON")
sudo nbd-client localhost 11081 /dev/nbd0 -N "$EXPORT_NAME"
sudo mkfs.ext4 /dev/nbd0
sudo mount /dev/nbd0 /mnt/orca
```

Unmount/disconnect:

```sh
sudo umount /mnt/orca
sudo nbd-client -d /dev/nbd0
```

`make demo` prints the validation flow:

- volume created
- session started on node-1
- write completed
- snapshot committed
- resume on node-1 with cache hits
- resume on node-2 with cache misses and MinIO fetches
- final read data equals originally written data

## Node API

Each node exposes:

```text
POST /volumes/create
POST /sessions/start
GET  /sessions/{id}/read?offset=&length=
PUT  /sessions/{id}/write?offset=
POST /sessions/{id}/commit
POST /sessions/{id}/stop
GET  /sessions/{id}/stats
```

The control service exposes:

```text
POST /volumes/create
GET  /volumes/{id}
POST /sessions/start
POST /scheduler/force
GET  /nodes
```

`POST /sessions/start` accepts `{"volume_id":"...", "force_node":"node-1", "runtime":"http-block"}` for deterministic tests. Without `runtime`, sessions default to `http-block`. Without `force_node`, the scheduler prefers the volume's healthy `last_node`, then falls back to another healthy node.

## Storage Semantics

A virtual volume has a `volume_id`, `size_bytes` defaulting to 10 GiB, `chunk_size` defaulting to 4 MiB, and a latest snapshot pointer. A snapshot manifest maps `chunk_index` to `chunk_id`, where `chunk_id` is the SHA-256 digest of the immutable chunk bytes. Missing manifest entries read as zero-filled chunks.

Reads check the dirty session overlay first, then local node cache, then MinIO, then zero-fill. Writes materialize the base chunk on first write, patch bytes into it, and mark it dirty. Commit hashes dirty chunks, uploads missing immutable chunks to MinIO, writes a new manifest, updates the latest snapshot pointer, keeps committed chunks in the local cache, and clears dirty state.

The dirty session overlay is disk-backed under each node's local cache volume at `dirty-sessions/{session_id}/{chunk_index}`. The node still tracks dirty chunk indexes in memory while the session is active, but the patched chunk bytes are written to disk until commit or stop. Commit reads those dirty chunk files, writes immutable chunks/manifests, then clears the overlay directory.

Commit processes dirty chunks in bounded batches. The default batch is 16 chunks, configurable for node NBD exports with `NBD_COMMIT_BATCH_CHUNKS`. This keeps commit memory bounded now and leaves a clean path to make remote uploads asynchronous later.

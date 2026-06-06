# Orca Blocks MVP

This repository contains a local Docker Compose prototype for the hard storage path of a remote-execution block backend. It emulates two execution nodes with independent persistent local caches, MinIO as durable S3-compatible chunk storage, and Postgres metadata for volumes, snapshots, and scheduling hints.

Firecracker, ublk, auth, encryption, Kubernetes, and advanced prefetch are intentionally out of scope. The current runtimes are thin test/debug surfaces over the storage package. NBD is an internal device implementation for the mounted filesystem runtime and a low-level protocol test path.

## Architecture

- `control-service`: creates volumes and schedules sessions. It tracks `last_node` in Postgres and prefers that node when it is healthy.
- `node-1` and `node-2`: expose the block backend HTTP API and run a local NBD listener used by node-owned block devices. Each node has its own Docker volume mounted as `/cache`.
- `minio`: stores immutable chunks under `chunks/{sha256}` and snapshot manifests under `manifests/{snapshot_id}.json`.
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

The services are exposed on host ports `18080` (control), `18081` (node-1), `18082` (node-2), and `19000`/`19001` (MinIO API/console).

## Remote Linux/KVM Dev

Firecracker development needs Linux KVM. You can keep editing locally and run the KVM-dependent loop on a reachable Linux VM:

```sh
make remote-authorize-key REMOTE_HOST=vboxuser@192.168.178.201
make remote-check REMOTE_HOST=vboxuser@192.168.178.201
make remote-setup REMOTE_HOST=vboxuser@192.168.178.201
make remote-test REMOTE_HOST=vboxuser@192.168.178.201
make remote-demo REMOTE_HOST=vboxuser@192.168.178.201
```

`remote-authorize-key` installs your local SSH public key, defaulting to `~/.ssh/id_ed25519.pub`, into the remote user's `authorized_keys`. The remote setup script verifies `/dev/kvm`, installs Docker Compose and development packages, enables Docker, and checks that a container can see `/dev/kvm`. If Docker only works through `sudo` immediately after setup, log out and back in or run `newgrp docker`.

Useful knobs:

```sh
REMOTE_HOST=vboxuser@192.168.178.201
REMOTE_DIR=~/orca-blocks
LOCAL_PUBLIC_KEY=~/.ssh/id_ed25519.pub
REMOTE_SSH_OPTS="-p 2222"
REMOTE_TTY_SSH_OPTS="-tt -p 2222"
REMOTE_SCP_OPTS="-P 2222"
REMOTE_RSYNC_SSH_OPTS="-p 2222"
```

## Runtimes

`runtime:"http-block"` is the default. It does not use NBD or mount anything; the HTTP read/write endpoints call the storage backend directly. This is useful for storage tests, cache demos, and debugging.

`runtime:"mounted-fs"` is the pre-Firecracker integration harness. Session start happens on the selected node, registers a session-local NBD export, attaches it to a free `/dev/nbdX` inside that node container, optionally formats it, and mounts it under `/mnt/orca-sessions/{session_id}`. The response includes `mount_path` and `nbd_device`. `POST /sessions/{id}/commit` unmounts, disconnects the NBD device, commits dirty chunks to a new snapshot, and releases the device. `POST /sessions/{id}/stop` unmounts and disconnects without committing.

`runtime:"nbd-export-test"` is a low-level protocol test harness. The node creates a session-local NBD export, but the caller attaches it. This preserves direct NBD coverage while keeping the future product path centered on node-owned device lifecycle.

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

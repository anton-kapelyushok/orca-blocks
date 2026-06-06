# Orca Blocks MVP

This repository contains a local Docker Compose prototype for the hard storage path of a remote-execution block backend. It emulates two execution nodes with independent persistent local caches, MinIO as durable S3-compatible chunk storage, and Postgres metadata for volumes, snapshots, and scheduling hints.

Firecracker, ublk, auth, encryption, Kubernetes, and advanced prefetch are intentionally out of scope. The HTTP node API, raw NBD frontend, and node-managed mounted-session frontend are thin frontends over the storage package.

## Architecture

- `control-service`: creates volumes and schedules sessions. It tracks `last_node` in Postgres and prefers that node when it is healthy.
- `node-1` and `node-2`: expose the block backend HTTP API and run a local NBD listener. Each node has its own Docker volume mounted as `/cache`.
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

Each node also exposes a session-local NBD listener: `11081` for node-1 and `11082` for node-2. NBD exports are created by starting a session with `{"frontend":"nbd"}`. The response includes `nbd_addr` and `nbd_export_name`; the export name is the session ID. NBD `READ` and `WRITE` call that session's storage backend directly. NBD `FLUSH` fsyncs the disk-backed dirty overlay but does not create a snapshot commit. Use `commit_on_disconnect:true` in the session-start request if you want the MVP export to commit when the NBD client disconnects.

For the pre-Firecracker milestone, prefer the node-managed mounted frontend:

```json
{
  "volume_id": "mount-demo",
  "force_node": "node-1",
  "frontend": "mount",
  "format": true,
  "fs_type": "ext4"
}
```

With `frontend:"mount"`, session start happens on the selected node, registers a session-local NBD export, attaches it to a free `/dev/nbdX` inside that node container, optionally formats it, and mounts it under `/mnt/orca-sessions/{session_id}`. The response includes `mount_path` and `nbd_device`. `POST /sessions/{id}/commit` unmounts, disconnects the NBD device, commits dirty chunks to a new snapshot, and releases the device. `POST /sessions/{id}/stop` unmounts and disconnects without committing.

Example host-side attach flow on a Linux machine with NBD tools:

```sh
sudo modprobe nbd
curl -sS -X POST localhost:18080/volumes/create \
  -H 'content-type: application/json' \
  -d '{"volume_id":"nbd-demo","size_bytes":1073741824,"chunk_size":4194304}'
SESSION_JSON=$(curl -sS -X POST localhost:18080/sessions/start \
  -H 'content-type: application/json' \
  -d '{"volume_id":"nbd-demo","force_node":"node-1","frontend":"nbd","commit_on_disconnect":true}')
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

`POST /sessions/start` accepts `{"volume_id":"...", "force_node":"node-1"}` for deterministic tests. Without `force_node`, the scheduler prefers the volume's healthy `last_node`, then falls back to another healthy node.

## Storage Semantics

A virtual volume has a `volume_id`, `size_bytes` defaulting to 10 GiB, `chunk_size` defaulting to 4 MiB, and a latest snapshot pointer. A snapshot manifest maps `chunk_index` to `chunk_id`, where `chunk_id` is the SHA-256 digest of the immutable chunk bytes. Missing manifest entries read as zero-filled chunks.

Reads check the dirty session overlay first, then local node cache, then MinIO, then zero-fill. Writes materialize the base chunk on first write, patch bytes into it, and mark it dirty. Commit hashes dirty chunks, uploads missing immutable chunks to MinIO, writes a new manifest, updates the latest snapshot pointer, keeps committed chunks in the local cache, and clears dirty state.

The dirty session overlay is disk-backed under each node's local cache volume at `dirty-sessions/{session_id}/{chunk_index}`. The node still tracks dirty chunk indexes in memory while the session is active, but the patched chunk bytes are written to disk until commit or stop. Commit reads those dirty chunk files, writes immutable chunks/manifests, then clears the overlay directory.

Commit processes dirty chunks in bounded batches. The default batch is 16 chunks, configurable for node NBD exports with `NBD_COMMIT_BATCH_CHUNKS`. This keeps commit memory bounded now and leaves a clean path to make remote uploads asynchronous later.

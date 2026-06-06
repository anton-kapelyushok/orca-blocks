# Orca Blocks MVP

This repository contains a local Docker Compose prototype for the hard storage path of a remote-execution block backend. It emulates two execution nodes with independent persistent local caches, MinIO as durable S3-compatible chunk storage, and Postgres metadata for volumes, snapshots, and scheduling hints.

Firecracker, NBD, ublk, auth, encryption, Kubernetes, and advanced prefetch are intentionally out of scope. The HTTP node API is a temporary frontend over the storage package so it can later be replaced by a block-device frontend.

## Architecture

- `control-service`: creates volumes and schedules sessions. It tracks `last_node` in Postgres and prefers that node when it is healthy.
- `node-1` and `node-2`: expose the block backend HTTP API. Each node has its own Docker volume mounted as `/cache`.
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

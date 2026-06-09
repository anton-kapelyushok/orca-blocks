# OverlayBD JB Two-Node Demo Architecture

This document describes the demo architecture for the single-tenant Docker,
Sysbox, and OverlayBD path. It is intentionally practical: the names match the
scripts and machines used by the demo.

## Goal

The demo shows that we can:

1. Start a large JetBrains Workspace image lazily without pulling all bytes to a
   node first.
2. Run the workspace as a Sysbox container, so user code can behave like it is in
   a lightweight VM-like environment.
3. Preserve user state by capturing the container overlay upperdir.
4. Convert that state into a new lazy OverlayBD layer.
5. Push the derived image to a shared registry.
6. Start the derived image on another node and see the preserved state there.

The concrete user-state payload is Spring Petclinic: clone the repository, run a
Maven package build, commit the result, then rebuild from the derived image on
both nodes.

## Topology

```text
                     +------------------------------+
                     | local                        |
                     |                              |
                     | orchestration script         |
                     | demos/.../demo.py            |
                     +---------------+--------------+
                                     |
                   +-----------------+-----------------+
                   |                                   |
                   | ssh root@178.128.247.74           | ssh + sudo
                   v                                   v
  +--------------------------------------+   +------------------------------+
  | master                               |   | slave                        |
  | root@178.128.247.74                  |   | anton.kapeliushok@...        |
  |                                      |   |                              |
  | runtime: containerd + OverlayBD      |   | runtime: containerd          |
  |          Sysbox                      |   |          OverlayBD           |
  | registry: 178.128.247.74:5000        |   |          Sysbox              |
  |           127.0.0.1:5000             |   +---------------+--------------+
  | layers: registry blob storage        |                   |
  | metadata: MySQL for OverlayBD        |<------------------+
  +--------------------------------------+    registry + MySQL
```

| Name | Role |
| --- | --- |
| `local` | Runs the Python orchestrator and streams remote logs. It does not do image conversion itself. |
| `master` | Droplet node. Runs containers and hosts durable registry, layer blobs, and MySQL metadata. |
| `slave` | Google Cloud node. Runtime-only node for workspace starts, mutable build, and commit execution. |
| `registry/layers` | Docker registry on master. Stores OCI manifests/configs and OverlayBD layer blobs. |
| `MySQL` | Metadata DB on master used by the OverlayBD converter/snapshotter stack. |

## Runtime Stack On Each Node

Each node has the same runtime shape:

| Component | Responsibility |
| --- | --- |
| `containerd` | Runtime control plane. The demo uses namespace `moby`. |
| `/opt/overlaybd/snapshotter/ctr` | OverlayBD-aware `ctr` wrapper used for `rpull`, `run`, image inspection, and snapshot cleanup. |
| `overlaybd-snapshotter` | Snapshotter that mounts lazy OverlayBD layers and serves block reads from local cache or the registry. |
| `overlaybd` local caches | Node-local cache directories, currently `/opt/overlaybd/registry_cache` and `/opt/overlaybd/gzip_cache`. |
| `sysbox-runc` | Container runtime used for workspace containers. Gives a VM-like container environment without Firecracker. |
| CNI | Network setup for `ctr run --cni`, required for workspace publishing and Maven/Git network access. |
| `runc` | Baseline runtime for simple checks, not the main demo isolation path. |

## Node Setup Reference

The node setup we used during the study is captured in these scripts. They are
useful as a record of what had to exist on a node for the demo, but they are not
a polished product bootstrap.

| Script | Purpose |
| --- | --- |
| [`scripts/setup-sysbox-docker-vm.sh`](../../scripts/setup-sysbox-docker-vm.sh) | Installs/configures the Sysbox Docker VM pieces used by the runtime nodes. |
| [`scripts/setup-overlaybd-containerd-snapshotter.sh`](../../scripts/setup-overlaybd-containerd-snapshotter.sh) | Installs/configures containerd, OverlayBD snapshotter, and related runtime wiring. |

Warning: these are historical setup references from the benchmark/demo work.
They may be outdated relative to the current VMs and should be reviewed before
running on a fresh node.

`setup-sysbox-docker-vm.sh` prepares the general container host:

| Area | What the script sets up |
| --- | --- |
| Docker Engine | Installs Docker from Docker's apt repository, pins/holds the selected package version, enables Docker and containerd. |
| Sysbox | Downloads and installs Sysbox CE, then enables `sysbox`, `sysbox-fs`, and `sysbox-mgr`. |
| Docker runtime config | Adds `sysbox-runc` to `/etc/docker/daemon.json`, optionally enables Docker's containerd image store, configures insecure registry access, and adjusts Docker bridge/address-pool settings. This follows the Docker/containerd setup shape from [`containerd/accelerated-container-image` Docker docs](https://github.com/containerd/accelerated-container-image/blob/main/docs/DOCKER.md). |
| CNI | Installs `containernetworking-plugins`, creates `/opt/cni/bin`, writes `/etc/cni/net.d/10-orca-bridge.conf`, and enables IPv4 forwarding. This is what makes `ctr run --cni` work in the demo. |
| Native registry | Optionally installs the host `docker-registry` service and configures it at `REGISTRY_ADDR`, default `127.0.0.1:5000`. |
| Stargz tools | Optionally downloads `ctr-remote` and `containerd-stargz-grpc`; these are leftovers from the earlier stargz path investigation, not the main OverlayBD demo path. |
| Verification | Checks Docker/containerd, Sysbox, CNI files, `hello-world`, a Sysbox Alpine run, registry `/v2/`, and optional stargz tool versions. |

`setup-overlaybd-containerd-snapshotter.sh` prepares the lazy-image runtime:

| Area | What the script sets up |
| --- | --- |
| OverlayBD packages | Downloads and installs the `overlaybd` and `overlaybd-snapshotter` Debian packages used in the study. |
| Kernel/runtime support | Loads `target_core_user`, enables `overlaybd-tcmu`, enables `overlaybd-snapshotter`, and restarts containerd. |
| Snapshotter config | Writes `/etc/overlaybd-snapshotter/config.json` with root `/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd`, socket `/run/overlaybd-snapshotter/overlaybd.sock`, `rwMode=overlayfs`, `autoRemoveDev=true`, and an insecure mirror registry entry. |
| containerd integration | Adds `proxy_plugins.overlaybd` to `/etc/containerd/config.toml` so containerd can call the OverlayBD snapshotter. |
| MySQL metadata DB | Optionally installs MySQL, creates the `overlaybd` database/user, and creates the `overlaybd_layers` and `overlaybd_manifests` tables used by the converter/metadata path. |
| Verification | Checks `overlaybd-tcmu`, `overlaybd-snapshotter`, containerd, optional MySQL table access, the containerd overlaybd plugin, and `/opt/overlaybd/snapshotter/ctr`. |

The demo VMs also had manual or environment-specific fixes layered on top:

| Gap | Why it matters |
| --- | --- |
| Registry/MySQL placement | In the demo, master hosts registry storage and MySQL. A real deployment should make these separate durable services or an explicit node-local cache layer backed by durable storage. |
| Remote MySQL access | The script creates local MySQL users by default. Cross-node conversion/metadata access needs explicit bind-address, grants, firewall, and credentials. |
| Registry exposure | The script can install a native registry, but exposing it to other nodes depends on bind address, firewall rules, and whether the node should use `127.0.0.1:5000` or `178.128.247.74:5000`. |
| CNI forwarding | The setup writes basic CNI config. Some demo scripts still add temporary `iptables` forwarding rules for `cni0`, so networking is not fully productized. |
| Plain HTTP/insecure registry | The demo relies on insecure/plain registry access and URL rewrites from `https://` to `http://`. Product needs TLS/auth and a cleaner registry config path. |
| Version drift | Package names, versions, checksums, Ubuntu release compatibility, and OverlayBD behavior were moving targets during the study. Treat these scripts as a reproducibility aid, not guaranteed fresh-node automation. |
| Idempotency and rollback | The scripts are mostly rerunnable, but they are not a full provisioning system with rollback, health remediation, or upgrade safety. |

## Main Commands

This is the main path without the demo-runner plumbing.

Build or prepare an image:

```bash
# For a normal derived image, build and push it first.
docker build -t "$NORMAL_REF" "$CONTEXT_DIR"
docker push "$NORMAL_REF"

# For the JetBrains base image, mirror the upstream image into our registry.
docker pull registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643
docker tag registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643 "$NORMAL_REF"
docker push "$NORMAL_REF"

# Convert the normal OCI image into an OverlayBD image.
/opt/overlaybd/snapshotter/ctr -n moby images pull --local --plain-http "$NORMAL_REF"
/opt/overlaybd/snapshotter/ctr -n moby obdconv \
  --plain-http \
  --fstype ext4 \
  --dbstr "$DB_STR" \
  "$NORMAL_REF" "$OVERLAYBD_REF"
/opt/overlaybd/snapshotter/ctr -n moby images push --local --plain-http "$OVERLAYBD_REF"
```

Run an OverlayBD image lazily:

```bash
/opt/overlaybd/snapshotter/ctr -n moby rpull --plain-http "$IMAGE_REF"

/opt/overlaybd/snapshotter/ctr -n moby run \
  --snapshotter overlaybd \
  --runtime io.containerd.runc.v2 \
  --cni \
  --allow-new-privs \
  --runc-binary /usr/bin/sysbox-runc \
  --rm \
  "$IMAGE_REF" "$NAME"
```

For a mutable run that will be committed later, the demo omits `--rm`, stops the
task, and keeps the container/snapshot metadata:

```bash
/opt/overlaybd/snapshotter/ctr -n moby tasks kill -s SIGTERM "$NAME"
/opt/overlaybd/snapshotter/ctr -n moby tasks kill -s SIGKILL "$NAME"
/opt/overlaybd/snapshotter/ctr -n moby tasks rm "$NAME"
```

The important bit is that we do not remove the container or snapshot before the
commit step; the snapshot upperdir is the source of the user-state diff.

Commit a mutable snapshot into a derived lazy layer:

```bash
# 1. Discover the snapshot mount and export its upperdir as an OCI diff tar.
/opt/overlaybd/snapshotter/ctr -n moby snapshots --snapshotter overlaybd mounts /tmp/probe "$NAME"
# export upperdir -> "$DIFF_TAR"

# 2. Create an OverlayBD writable pair and apply the diff tar into it.
/opt/overlaybd/bin/overlaybd-create "$APPLY_DATA" "$APPLY_INDEX" 64
/opt/overlaybd/bin/overlaybd-apply "$DIFF_TAR" "$APPLY_CONFIG"

# 3. Commit the writable pair into a compressed OverlayBD layer.
/opt/overlaybd/bin/overlaybd-commit -z -f "$APPLY_DATA" "$APPLY_INDEX" "$COMMIT_OBD"
```

After `overlaybd-commit`, the demo uploads the committed `.obd` blob, writes a
derived image config/manifest, and pushes the manifest to the registry. That
registry upload is script plumbing; the product primitive we need is "commit
this container snapshot as a new lazy image layer".

## Images

| Image | Meaning |
| --- | --- |
| `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` | Original JetBrains workspace image. Used for plain Docker comparison. |
| `178.128.247.74:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z` | Converted lazy OverlayBD base image used by the demo on slave. |
| `127.0.0.1:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z` | Same converted base image from the master node's local registry perspective. |
| `178.128.247.74:5000/orca/overlaybd-jb-real:petclinic-build-$RUN_ID` | Derived image pushed by the commit step and used by slave. |
| `127.0.0.1:5000/orca/overlaybd-jb-real:petclinic-build-$RUN_ID` | Same derived image from master. |

The base image conversion happens before the demo. The demo does not rebuild or
reconvert the JetBrains workspace image; it assumes the converted base image is
already present in the registry and metadata DB.

## Demo Flow

```text
local demo.py
  |
  +--> master: cleanup local runtime state
  +--> slave:  cleanup local runtime state
  |
  +--> master: run base workspace until Join URL
  |       `--> registry: lazy base-layer reads as needed
  |
  +--> slave: run base workspace until Join URL
  |       `--> registry: lazy base-layer reads as needed
  |
  +--> slave: run base workspace again until Join URL
  |
  +--> slave: clone + build Petclinic in mutable container
  |       `--> registry: lazy base reads; Git/Maven use network
  |
  +--> slave: commit overlay upperdir as derived OverlayBD layer
  |       `--> registry: upload derived layer, config, manifest
  |
  +--> slave: rebuild Petclinic from derived image
  |       `--> registry: first-use derived-layer reads as needed
  |
  +--> master: rebuild Petclinic from derived image
  |       `--> registry: first-use derived-layer reads as needed
  |
  `--> master: rebuild Petclinic from derived image again
```

The scenario implemented by `demo.py` is:

| Step | Short name | Purpose |
| ---: | --- | --- |
| 1 | `cleanup` | Remove local demo state and local OverlayBD caches on both nodes. Durable registry/MySQL state is kept. |
| 2 | `master workspace cold` | Start the converted base workspace image on master after cleanup. |
| 3 | `slave workspace cold` | Start the converted base workspace image on slave after cleanup. |
| 4 | `slave workspace hot` | Start the same base image on slave again to show warmed local state. |
| 5 | `slave clone+build coldish` | In a mutable slave container, clone Petclinic and run the first Maven package build. Workspace layers are warm; Petclinic state is new. |
| 6 | `commit derived image` | Convert the mutable upperdir into a new OverlayBD layer and push a derived image. |
| 7 | `slave build coldish` | Run the derived image on slave. Petclinic state is present, but the derived layer may be first-use. |
| 8 | `master build coldish` | Run the derived image on master. Petclinic state is present, but the derived layer may be first-use on master. |
| 9 | `master build warm` | Run the derived image on master again to show warmed derived-layer behavior. |

## Scripts And Responsibilities

| Script | Runs on | Responsibility |
| --- | --- | --- |
| `demo.py` | local | Interactive orchestration, confirmations, SSH invocation, dry-run, optional command preview. |
| `remote/cleanup.sh` | master, slave | Kill demo tasks, remove demo containers/snapshots, clear local OverlayBD image refs/content/caches, enforce `rwMode=overlayfs`. |
| `remote/run-workspace-until-join.sh` | master, slave | `rpull` an image, run it with OverlayBD + Sysbox, and measure until the Join URL. |
| `remote/petclinic-build-mutable.sh` | slave | Run base image, clone Petclinic, build it, keep the container snapshot, and record upperdir metadata. |
| `remote/commit-snapshot.sh` | slave | Export upperdir as OCI diff tar, apply it into an OverlayBD writable pair, commit an `.obd` layer, upload blobs/config/manifest. |
| `remote/petclinic-build-repeat.sh` | master, slave | Run the derived image and measure repeat Maven package build. |
| `remote/mutable-touch.sh` | legacy | Older small-file mutation path, kept for simple verification/debugging. |
| `remote/verify-touch.sh` | legacy | Older touched-file verification path. |

## Local Resume Path

There is a second hot path that does not go through the registry:

```text
first local start:
  ctr run ... IMAGE NAME
  wait for Join URL

pause/restart filesystem state:
  ctr tasks kill NAME
  ctr tasks rm NAME
  # keep the container metadata and snapshot
  ctr tasks start NAME
```

This keeps the local container/snapshot and restarts the task from that local
filesystem state. It is not process-memory resume: the JVM starts again, PIDs
change, and sockets are recreated. It is also not portable to another node until
the upperdir is committed into a derived image and pushed.

Measured on slave, this path reached the Join URL in 5,411 ms after `ctr tasks
start`, with first workspace log at 608 ms. That is close to warm Docker for the
workspace-start path and much faster than starting a fresh OverlayBD container
from the same image reference.

## Commit Path

The commit path is the most important demo-specific mechanism.

```text
  +-------------------------------+
  | running mutable Sysbox task   |
  +---------------+---------------+
                  |
                  v
  +-------------------------------+
  | overlayfs upperdir            |
  | user changes live here        |
  +---------------+---------------+
                  |
                  | export with whiteouts + uid/gid mapping
                  v
  +-------------------------------+
  | OCI diff tar                  |
  +---------------+---------------+
                  |
                  | overlaybd-apply
                  v
  +-------------------------------+
  | OverlayBD writable pair       |
  | data file + index file        |
  +---------------+---------------+
                  |
                  | overlaybd-commit -z
                  v
  +-------------------------------+       +--------------------------+
  | committed .obd layer          | ----> | registry blob upload     |
  +-------------------------------+       +--------------------------+

  +-------------------------------+       +--------------------------+
  | derived image config          | ----> | registry config upload   |
  +-------------------------------+       +--------------------------+

  +-------------------------------+       +--------------------------+
  | derived manifest              | ----> | registry manifest push   |
  +-------------------------------+       +--------------------------+
```

The remote script records a `mutable.env` file with runtime-discovered paths.
They are taken from the local containerd snapshot mount description after the
mutable container has been created:

```bash
/opt/overlaybd/snapshotter/ctr -n moby snapshots --snapshotter overlaybd mounts /tmp/orca-demo-probe "$NAME"
```

The script parses that returned overlay mount command for `upperdir=`,
`workdir=`, and `lowerdir=`. These values are repeatably discoverable for a
given still-existing container snapshot name, but the actual snapshot ids and
filesystem paths are not stable across runs, nodes, cleanup, or container
recreation.

| Field | Why it matters |
| --- | --- |
| `NAME` | Container/snapshot name to clean up after commit. |
| `IMAGE_REF` | Base image reference used for the mutable run. |
| `UPPERDIR` | Runtime-discovered overlayfs diff directory containing user changes. Valid only while that local container snapshot still exists. |
| `LOWERDIR` | Runtime-discovered lowerdir stack, useful for diagnosing mount layout. Diagnostic only; not a stable product contract. |
| `OBD_CONFIG_PATH` | Base OverlayBD config used to construct the writable pair config. |
| `UID_MAP`, `GID_MAP` | Sysbox/idmapped container ownership mapping used when exporting the diff tar. |
| `PROJECT_DIR`, `TOUCH_PATH` | Payload paths captured in image history/verification. |

The concrete commands in `commit-snapshot.sh` are:

1. Source `mutable.env`.
2. Export `UPPERDIR` as an OCI-style diff tar.
3. Create an empty OverlayBD writable pair with `overlaybd-create`.
4. Build an apply config by adding `.upper` and `.resultFile` to the base
   OverlayBD config.
5. Apply the OCI diff tar with `overlaybd-apply`.
6. Commit the writable pair with `overlaybd-commit -z`.
7. Fetch the base manifest and config from the registry.
8. Upload the new `.obd` blob.
9. Upload the derived config.
10. Push the derived manifest.
11. Remove the mutable container/snapshot.

Current caveat: this OverlayBD build can print success and then exit `139` from
`overlaybd-apply`; the script treats the apply as successful only when the
result file contains `success`.

## Cache Model

There are two classes of state:

| State | Location | Cleared by demo cleanup? | Purpose |
| --- | --- | --- | --- |
| Registry blobs/manifests | master registry | No | Durable image storage. |
| OverlayBD metadata DB | MySQL on master | No | Durable OverlayBD metadata. |
| containerd image refs/content | each node | Yes | Local image metadata/content cache. |
| OverlayBD snapshots | each node | Yes | Local mounted/unpacked snapshot state. |
| OverlayBD registry/gzip caches | each node | Yes | Local block/cache data used by lazy reads. |
| Local uncommitted container/snapshot | each node | Demo cleanup removes it | Fast local restart path via `ctr tasks start`; not portable across nodes. |
| Running containers/tasks | each node | Demo tasks only | Avoid clearing state while unknown tasks are active. |

This is why the demo can model a cold node without rebuilding or deleting the
durable registry state.

## Timings And Metrics

Every container-run script reports `first user text appears`, measured from the
host-side `ctr run` or `docker run` start until the first stdout/stderr line is
read from the container. For the workspace scripts, the README benchmark table
renames that metric to `Workspace first log`, because it is the real JetBrains
entrypoint's first log line, not the generic container startup floor.

The main metrics are:

| Metric | Meaning |
| --- | --- |
| `rpull` | OverlayBD metadata pull before `ctr run`. |
| Image prep | Benchmark-table umbrella for the pre-run image step. For OverlayBD this is `rpull`; for the Docker baseline script it is image inspect plus `docker pull` only when the image is missing. The demo itself does not run Docker. |
| `first user text appears` | First container stdout/stderr line after run starts. |
| `Dock HTTP API` | Workspace dock API is listening. |
| `Workspace Server` | Workspace API server is listening. |
| `Join URL` | Workspace prints the user-facing Fleet join URL. |
| hot local container snapshot | Existing container/snapshot restarted with `ctr tasks start`, without registry commit/pull. |
| `clone_ms` | Time for `git clone --depth 1` inside the mutable Petclinic container. |
| `first_build_ms` | First Maven package build inside the mutable Petclinic container. |
| `repeat_build_ms` | Maven package build from the derived image. |
| `approximate transferred bytes` | Sum of uploaded derived layer/config/manifest bytes in the commit step. |

## Glossary

| Term | Meaning in this demo | Reference |
| --- | --- | --- |
| Docker containerd image store | Docker mode where image storage is backed by containerd snapshotters. We enable this in Docker config because the OverlayBD ecosystem expects containerd-native image/snapshotter behavior. | [`containerd/accelerated-container-image` Docker docs](https://github.com/containerd/accelerated-container-image/blob/main/docs/DOCKER.md) |
| containerd | Runtime control plane used by Docker and directly by the demo through `ctr`. The demo uses namespace `moby`. | [containerd](https://github.com/containerd/containerd) |
| `ctr` | Low-level containerd CLI. We use `/opt/overlaybd/snapshotter/ctr` because it includes OverlayBD-specific commands such as `rpull` and `obdconv`. | [containerd `ctr`](https://github.com/containerd/containerd/tree/main/cmd/ctr) |
| OverlayBD | Lazy block-based image format/runtime. Instead of downloading all layer bytes before start, it can fetch blocks as they are read. | [containerd/overlaybd](https://github.com/containerd/overlaybd) |
| accelerated-container-image | The broader OverlayBD/conversion tooling project used for image conversion and Docker/containerd integration. | [accelerated-container-image](https://github.com/containerd/accelerated-container-image) |
| `obdconv` | OverlayBD image conversion command exposed by the OverlayBD `ctr` wrapper. It converts a normal OCI image ref into an OverlayBD image ref and records metadata in MySQL when `--dbstr` is used. | [conversion docs](https://deepwiki.com/containerd/accelerated-container-image/4-image-conversion) |
| `rpull` | Lazy pull/metadata fetch step before `ctr run`. It prepares the local image/snapshot metadata without downloading all image bytes up front. | [accelerated image docs](https://github.com/containerd/accelerated-container-image) |
| `overlaybd-snapshotter` | containerd snapshotter plugin that mounts OverlayBD layers and serves block reads through local cache or registry-backed storage. | [overlaybd snapshotter](https://github.com/containerd/accelerated-container-image) |
| `rwMode=overlayfs` | OverlayBD snapshotter mode where container writes go to a normal overlayfs upperdir. This made Sysbox startup much faster than the `dev` writable-device path in the demo. | [OverlayBD config in setup script](../../scripts/setup-overlaybd-containerd-snapshotter.sh) |
| overlayfs `upperdir` | Writable overlay directory that contains user changes for a mutable container. The demo exports this directory as an OCI diff tar during commit. | [Linux overlayfs docs](https://docs.kernel.org/filesystems/overlayfs.html) |
| overlayfs `lowerdir` | Read-only lower layer stack for the overlay mount. In the demo it is mainly diagnostic, not a stable product API. | [Linux overlayfs docs](https://docs.kernel.org/filesystems/overlayfs.html) |
| whiteout | OCI/overlay marker for a deleted file. Exporting a user diff correctly must preserve whiteouts, otherwise deletions disappear when the derived image is started elsewhere. | [OCI image layer spec](https://github.com/opencontainers/image-spec/blob/main/layer.md) |
| idmapped ownership | Sysbox/idmapped container files may have host ids that need mapping back to container ids when exporting a diff tar. | [Linux idmapped mounts docs](https://docs.kernel.org/filesystems/idmappings.html) |
| Sysbox | OCI runtime used for the container isolation path. It gives a more VM-like container environment than plain Docker/runc while staying in the container model. | [Sysbox](https://github.com/nestybox/sysbox) |
| `sysbox-runc` | Sysbox runtime binary passed to containerd with `--runc-binary /usr/bin/sysbox-runc`. | [Sysbox](https://github.com/nestybox/sysbox) |
| CNI | Container Network Interface. The demo uses `ctr run --cni`, so nodes need CNI plugins and a bridge config. | [CNI](https://github.com/containernetworking/cni) |
| MySQL metadata DB | OverlayBD conversion metadata store. The upstream sample schema defines `overlaybd_layers` and `overlaybd_manifests`; our setup script creates the same tables. Product should make this a real durable service. | [upstream schema sample](https://github.com/containerd/accelerated-container-image/blob/main/cmd/convertor/resources/samples/mysql.conf), [setup script](../../scripts/setup-overlaybd-containerd-snapshotter.sh) |
| Docker registry | OCI distribution endpoint used for normal image metadata and OverlayBD layer blobs. The demo hosts it on master. | [OCI distribution spec](https://github.com/opencontainers/distribution-spec) |
| local container snapshot resume | Hot path where an existing local container/snapshot is restarted with `ctr tasks start`. It is not process-memory resume and is not portable to another node. | [Local Resume Path](#local-resume-path) |

## Why This Architecture

The architecture is meant to test a middle path between two extremes:

| Option | Pros | Cons |
| --- | --- | --- |
| Plain multi-tenant Docker | Fast warm starts, simple runtime. | Weak isolation for untrusted workloads. |
| Firecracker VM + NBD | Stronger isolation. | Higher startup and disk-path complexity; [prior VMM disk benchmarks](../../docs/benchmarks/vmm-full-matrix-fresh-results.txt) showed weak sequential read throughput. |
| Single-tenant Docker + Sysbox + lazy OverlayBD | No full image pull, simpler than microVMs, user state can become a lazy image layer. | Warm workspace start is slower than plain Docker today; needs cache/prefetch tuning. |

The current demo benchmark, together with the earlier
[JetBrains Workspace Real-Env Timings](../../docs/benchmarks/jetbrains-workspace-real-env-results.md),
says the architecture is good enough for the demo:

- Cold no-image startup is competitive because OverlayBD avoids the full Docker
  image pull.
- Warm trivial command startup is sub-second with Sysbox.
- Warm JetBrains workspace startup is still slower than plain Docker, so the
  product optimization target is the JB/OverlayBD hot working set.

## Productization Gaps And Hacks

The demo intentionally keeps several pieces local and scriptable. A production
version needs to replace these with owned services, APIs, and lifecycle rules.

| Area | Current demo shape | Product requirement |
| --- | --- | --- |
| Registry and metadata placement | The Docker registry, layer blob storage, and OverlayBD MySQL metadata DB all live on `master`. | Separate durable services, or at least explicit node-local caches backed by durable shared storage. Master should not be a special pet node. |
| Snapshotting/commit path | We scrape the container overlay upperdir, export it as an OCI diff tar, create an OverlayBD writable pair, run `overlaybd-apply`, then push blobs/config/manifest by script. | A real commit API with containerd/snapshotter ownership, idempotency, locking, cleanup, and clear failure recovery. |
| Snapshot metadata handoff | The mutable build step writes a local `mutable.env` file with container/snapshot paths that the commit step reads later. | Structured state in the control plane, not shell env files on the worker. |
| Whiteouts and uid/gid mapping | Diff export has to preserve deletions as whiteouts and remap Sysbox/idmapped ownership correctly. | A tested library path for OCI diffs from idmapped overlayfs upperdirs. This is correctness-sensitive. |
| OverlayBD tooling behavior | This build of `overlaybd-apply` can print success and then exit `139`; scripts have to treat that carefully. | Stable tool behavior, version pinning, and explicit success criteria. |
| CNI setup | `ctr run --cni` depends on node-local CNI configuration prepared by setup scripts. | Managed networking with predictable IPAM cleanup, policy, observability, and integration with workspace publishing. |
| Local cache lifecycle | The demo mostly writes refs, content, snapshots, and OverlayBD block caches; cleanup is blunt and local. | Eviction, reference counting, quotas, cache warming, cache invalidation, and safe deletion under running tasks. |
| Durable deletion path | Registry blobs and MySQL metadata are kept forever during the demo. | Garbage collection for derived images/layers, metadata rows, and orphaned upload attempts. |
| Setup | Node preparation is partly manual and partly shell scripts from the study. | Reproducible provisioning, health checks, version locks, rollbacks, and bootstrap validation. |
| Security posture | Sysbox is probably the strongest practical isolation option on this Docker/container path, but it is still not a microVM boundary. The demo also uses local/plain registry plumbing. | A clear single-tenant threat model that explicitly chooses Sysbox, plus TLS/auth for registry and DB, secret handling, and documented isolation limits. |
| Failure recovery | If conversion, upload, manifest push, or cleanup dies halfway, the demo mostly relies on reruns and manual inspection. | Transactional-ish state transitions, retry semantics, resumable uploads, and orphan cleanup. |
| Scheduling and placement | The demo hardcodes `master` and `slave`. | Scheduler integration: pick nodes by cache state, capacity, tenant ownership, and derived-layer locality. |
| Observability | Timing tables are printed by scripts. | Metrics for cache hits/misses, lazy read latency, snapshot state, commit bytes, CNI setup, workspace milestones, and cleanup failures. |
| Resource isolation | CPU/memory and disk pressure are only lightly controlled in the benchmark path. | Per-env quotas, disk accounting for upperdirs and caches, noisy-neighbor controls, and pressure-based eviction. |

## Open Optimization Targets

| Area | Question |
| --- | --- |
| Startup working set | Which OverlayBD blocks does the JetBrains entrypoint/JBR touch before first log, Dock API, and Join URL? |
| Prefetch | Can we prefetch that block set after `rpull` or during node warmup? |
| Cache retention | Which local caches must survive between environments on a node to make warm starts reliable? |
| Derived layer first-use | Can the commit step or a post-commit hook warm the new Petclinic layer on the target node? |
| Sysbox overhead | Current warm `echo hi` is acceptable, but any avoidable CNI/runtime setup cost should be tracked. |
| Registry locality | Master uses `127.0.0.1:5000`; slave uses `178.128.247.74:5000`. Product deployments need a clear node-local registry/cache story. |

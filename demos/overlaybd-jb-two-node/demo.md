# Demo Framing

This demo is about choosing a practical product path for fast environment
startup and resumable user state.

## Current Product Path

The current product path is based on volumes and volume snapshots. The problem
is not that every operation is equally bad; some costs can be hidden from the
user, and one cannot.

| Operation | Current behavior | User-perceived mitigation |
| --- | --- | --- |
| First startup / cold path | Slow because it requires creating a volume from the base snapshot. | Can be hidden with a hotpool of environments pre-created from base images, getting perceived startup under 1s. |
| Stop | Slow because persisting the environment means taking a large volume snapshot. | Can be moved to background snapshotting, so user-perceived stop can be under 1s. |
| Resume | Slow because it requires creating/restoring a volume from the user's snapshot. | This is the hard blocker. It cannot be hidden the same way unless the exact resumed environment is already warm on a node. |

So the real issue is resume. First startup can be masked by hotpools, and stop
can be masked by background snapshotting, but resume still depends on restoring
the user's previous volume state before the environment is usable.

The goal of the study was to find a faster lazy-start and resume model that does
not put full volume restoration on the critical path.

## Path 1: Firecracker + NBD

Conclusion: the I/O drop makes this path a no-go. Even if the I/O problem were
solved, the overall complexity is probably still a no-go unless hard
multitenant isolation is the dominant requirement.

We tested microVMs backed by NBD storage, with local block cache and durable
MinIO storage behind it.

| Dimension | Result |
| --- | --- |
| Startup | Good. The VM/runtime portion can be around the 1s range. |
| Sequential I/O | Bad. Prior benchmarks showed about a 2x drop from the microVM/block-device path alone, and about a 4x drop with the fuller Orca NBD stack. A realistic optimized target may be closer to 3x, but probably not near native. |
| Complexity | High. Requires custom NBD storage, local block cache, block lifecycle, MinIO persistence, and VM orchestration. Our image path also has to unpack Docker images, make the rootfs bootable, and inject custom user init. |
| Isolation | Strongest option. |

Related benchmark: [JetBrains Workspace Real-Env Timings](../../docs/benchmarks/jetbrains-workspace-real-env-results.md).

## Path 2: Sysbox + OverlayBD

Conclusion: This is the strongest path so far and the one demonstrated here.

| Dimension | Result |
| --- | --- |
| Startup | Low-second startup for lazy images. Trivial commands can start in under 1s on the warm path. |
| I/O | Reasonable compared with the microVM/NBD path. |
| Demo state | Working two-node demo: run workspace, create user state, commit a derived image, run that derived image on another node. |
| Isolation | Stronger than plain Docker through Sysbox. Can be framed as a multitenant container-isolation model, but it is not a hardware/microVM boundary. |
| Snapshot/commit | Works, but the current commit path is hacky. |

The best-performing runtime mode is OverlayBD `rwMode=overlayfs`. In this mode
the container writes into a normal overlayfs upperdir, which gives good startup,
but the commit path must manually capture and convert that upperdir.

OverlayBD `rwMode=dev` is conceptually cleaner for native OverlayBD writable
commits, but in our tests it pushed startup toward about 5s, so it is not
acceptable for the dev/startup path right now.

### Registry And MySQL

The registry stores real OCI images:

```text
base-obd manifest     -> blob A' + blob B'
derived-obd manifest  -> blob A' + blob B' + blob C'
```

The blobs are stored once in the shared registry blob store. Multiple image
manifests can point to the same blobs. In the example above, the base image and
derived image both reference `A'` and `B'`; only `C'` is new user state.

MySQL does not store user files or blocks. It is a shared OverlayBD conversion
index. The important table maps a normal Docker filesystem chain to an
OverlayBD blob:

```text
registry/repo + chain_id -> overlaybd blob digest + size
```

That lets the converter decide:

```text
base chain already converted -> reuse existing OverlayBD blob
new user chain not found     -> convert and push only the new layer
```

This is why derived images can be incremental. The durable bytes are still in
the registry; MySQL is metadata that helps builders and nodes agree which
converted blobs already exist.

### GC Requirement

The demo treats registry and MySQL state as mostly append-only. Product cannot.

Deleting an image tag or manifest is not enough to reclaim bytes, because
multiple images can reference the same blobs. Product needs GC/reachability:

```text
live environment
  -> image manifest
  -> layer/blob digests
  -> OverlayBD metadata rows
  -> registry blobs
```

Minimum GC model:

| State | GC action |
| --- | --- |
| Env image tags/manifests | Delete expired/unneeded manifests. |
| Registry blobs | Let registry GC remove blobs no remaining manifest references. |
| OverlayBD MySQL rows | Remove rows no live image can need, or keep them as cache metadata with TTL. |
| Node-local caches | Evict independently by disk pressure/LRU; durable source is registry plus metadata DB. |
| containerd refs/snapshots | Prune stopped envs and stale local refs. |

Product needs a real lifecycle and GC subsystem.

## Path 3: Stargz

Conclusion: Stargz should not be rejected.

| Dimension | Result |
| --- | --- |
| Small workloads | Worked. Alpine/echo-style images can start quickly after `rpull`. |
| Workload optimization | eStargz can optimize image layout based on a workload. |
| JB workspace image | Not proven. The quick JB image optimization attempt did not finish in useful time on the test node. A stronger build server may change this, but we did not prove it. |
| I/O | Not checked fairly for the JB workspace case. Reasonable performance is plausible, but still an assumption. |
| Commit semantics | Unknown for product. OCI diff-only derived images are possible, but we have not proven the full mutable resume story. |

The useful proof from Stargz was that OCI derived images can be incremental:
the Alpine test produced a derived image that reused the base blob and added
only a tiny new diff layer. That proves the image model works. It does not yet
prove production-grade mutable resume with stargz.

Keep Stargz as an investigation branch, especially for read-only lazy base
images. Do not make it the primary recommendation yet.

## Recommendation

| Option | Recommendation |
| --- | --- |
| MicroVM + NBD | No-go for now. Good isolation, but poor disk-performance and complexity tradeoff. |
| Sysbox + OverlayBD | Best current path. Working demo, fast startup shape, reasonable I/O, and plausible incremental user-state commits. |
| Stargz | Keep investigating. Promising for lazy base images, but JB workspace startup and mutable state semantics are not proven. |

Short version:

```text
Choose Sysbox + OverlayBD as the current recommended direction.
Keep Stargz open as a research branch.
Do not continue with MicroVM + NBD unless hard multitenant isolation is mandatory.
```

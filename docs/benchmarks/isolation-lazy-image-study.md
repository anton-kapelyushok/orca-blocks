# Isolation and Lazy Image Startup Study

Date: 2026-06-08

## Executive Summary

The study points to three viable product shapes:

| Path | Isolation model | Image/data model | Best fit | Main risk |
| --- | --- | --- | --- | --- |
| VM + NBD | Strong multi-tenant isolation | Orca block snapshots backed by durable object storage | Shared multi-tenant infrastructure | NBD/block path must be optimized, especially sequential reads |
| Multi-tenant lazy Docker | Weak tenant isolation | Lazy image layers from registry/snapshotter | Fastest shared-node startup | Not acceptable where tenant isolation is required |
| Single-tenant lazy Docker | Isolation by node ownership | Lazy image layers from registry/snapshotter | Per-user or per-org dedicated nodes | Higher infrastructure cost and scheduling pressure |

The small-image startup test showed that VM setup overhead can be optimized, but the JetBrains workspace benchmark changed the product interpretation: for a real heavy workspace image, local-file VMs are already much slower than Docker before Orca/NBD is added.

## What We Measured

The fair startup comparison intentionally excludes publish/build work.

For Orca, image/rootfs preparation and import into MinIO-backed durable storage is treated as publish time. The measured user path is create environment to first user output, with local node block cache treated as local storage.

For lazy Docker, building the image, optimizing it for lazy pulling, and pushing it to the registry are treated as publish time. The measured user path is lazy pull/run to first user output, with local container runtime cache treated as local storage.

The cold image test used Alpine with a 256 MiB blob, 1 GiB rootfs size for Orca, and toxiproxy enabled at 20 ms latency and 10,000 KB/s bandwidth.

## Startup Results

| Path | Operation | Time |
| --- | --- | ---: |
| Orca VM + NBD | create env to first output | 6,466 ms |
| Orca VM + NBD | full startEnv wall time | 8,652 ms |
| No-VM lazy image | rpull | 990 ms |
| No-VM lazy image | run | 854 ms |
| No-VM lazy image | rpull + run to first output | 1,844 ms |

The important Orca internal timings were:

| Orca phase | Time |
| --- | ---: |
| attach_nbd_device | 1,009 ms |
| sideload_orca_init | 3,048 ms |
| setup_firecracker_network | 72 ms |
| run_firecracker | 2,249 ms |
| request_to_first_user_output | 6,466 ms |

This means the comparable execution portion is roughly:

| Path | Comparable execution time |
| --- | ---: |
| Orca run_firecracker | 2,249 ms |
| No-VM lazy rpull + run | 1,844 ms |

So the VM execution part is only about 405 ms slower in this measurement. The larger 6.5 second user-visible Orca result is mostly explained by NBD attach plus init sideload.

## JetBrains Workspace Check

The Alpine test is useful for isolating platform overhead, but it is not representative of a large workspace image. We also ran the JetBrains Air workspace image with 1 vCPU / 3 GiB RAM and a 6 GiB Orca rootfs. Full details are in `docs/benchmarks/jetbrains-workspace-real-env-results.md`.

| Target | Storage path | Join URL |
| --- | --- | ---: |
| Docker local image | Host Docker, pre-pulled image, cold host cache | 18.71s |
| Firecracker local, no NBD | Local ext4 rootfs file | 39.06s |
| QEMU q35 local, no NBD | Local ext4 rootfs file | 44.05s |
| Orca second env on node-1 | Firecracker + NBD, same node cache | 50.60s |
| Orca first env on node-1 | Firecracker + NBD, cold node cache | 55.64s |

This result shows that the VM penalty is significant for the real JetBrains workload even without NBD. Firecracker on a local file is about 2.1x slower than Docker, and QEMU q35 on a local file is about 2.4x slower than Docker. Orca/NBD adds more overhead, but it is not the whole story.

The conclusion from this benchmark is blunt: for large workspace images, microVMs are materially slower than Docker. They may still be the right choice for dense shared multi-tenant isolation, but they are not the fastest path to workspace startup.

## Disk Results

We reran the disk comparison without Docker warm/page-cache numbers, because those are not a useful product comparison. Full details are in `docs/benchmarks/docker-vs-orca-disk-fair.md`.

| Target | Cache state | Sequential | Random |
| --- | --- | ---: | ---: |
| Docker local image | Host page cache dropped | 490 MiB/s | 1,638 IOPS |
| Orca node-1 | Firecracker + NBD | 46 MiB/s | 1,243 IOPS |
| Orca node-2 cold | Firecracker + NBD | 49 MiB/s | 1,014 IOPS |
| Orca node-2 repeat | Firecracker + NBD | 51 MiB/s | 1,241 IOPS |

Sequential read throughput remains the clear Orca disk weakness: Docker cold-cache sequential reads were about 10x faster than Orca in this rerun.

Random reads are much closer after the range-read fix. Docker cold-cache random read was 1,638 IOPS; Orca landed around 1,014-1,243 IOPS. That is still slower, but no longer the order-of-magnitude failure we saw before fixing 4 MiB read amplification.

## VMM Findings

The VMM benchmark separated VM implementation overhead from the Orca storage path.

| Runtime | Sequential I/O | Random I/O | Startup |
| --- | ---: | ---: | ---: |
| Docker | 1,072 MB/s | 11,536 IOPS | 286 ms |
| Firecracker | 169 MB/s | 4,511 IOPS | 806 ms |
| Cloud Hypervisor | 164 MB/s | 4,653 IOPS | 1,009 ms |
| QEMU q35 | 456 MB/s | 4,732 IOPS | 1,953 ms |
| QEMU microvm | 433-445 MB/s | 4,071 IOPS | 4,737 ms |

QEMU q35 had much better direct-block sequential throughput than Firecracker in this benchmark, but its startup time was higher. For this POC, the stronger conclusion is not "switch VMM immediately"; it is that the storage frontend and data path matter a lot, and Firecracker startup itself is not the dominant issue in the current user-visible Orca measurement.

## Interpretation

### 1. VM + NBD Is the Multi-Tenant Path

This is the path if we need multiple unrelated tenants on shared infrastructure.

The current result is not yet where we want it, but the bottlenecks are identifiable:

| Bottleneck | Current cost | Optimization direction |
| --- | ---: | --- |
| NBD attach | ~1.0 s | keep devices pre-attached, pool NBD devices, or replace the frontend |
| orca-init sideload | ~3.0 s | bake into the guest image/initramfs or mount it without per-env copy |
| Sequential read path | ~52-56 MiB/s | reduce userspace/NBD overhead, improve prefetch/readahead, or test a different block frontend |

If attach and sideload are removed or hidden, the create-env path could move from ~6.5 seconds toward ~2.3-2.5 seconds for this Alpine test. That would put VM isolation much closer to no-VM lazy Docker startup while preserving tenant isolation.

### 2. Multi-Tenant Lazy Docker Is Fast but Not Isolated Enough

Lazy Docker/containerd can produce very attractive startup numbers because it avoids the VM boundary and can use mature lazy image mechanisms.

The problem is the security model. If multiple unrelated tenants share the same host kernel, this path does not provide the isolation story we need for hostile or mutually untrusted workloads.

This path is useful as a performance reference and may be acceptable for trusted internal workloads, but it is not the answer for shared multi-tenant execution.

### 3. Single-Tenant Lazy Docker Is the Simple Isolation Alternative

If we assign a node to one user, workspace, or organization, lazy Docker becomes much more attractive. We get mature image distribution, simpler runtime behavior, and no Firecracker/NBD path.

The tradeoff is cost and utilization. A hot or warm node per tenant is operationally simple, but it gives up the density advantage of a shared multi-tenant system. This may still be a good product tier or an enterprise mode.

## Current Recommendation

Choose single-tenant lazy Docker as the next product path.

It looks like the fastest safe implementation path: it keeps the Docker/container image model, uses mature lazy-image machinery, avoids the current Firecracker/NBD storage work, avoids the broader microVM startup penalty seen in the JetBrains benchmark, and still has an honest isolation story because tenants do not share a host.

VM + NBD remains the right architecture if we need dense shared multi-tenant infrastructure. The latest measurements make the optimization path clear, but the JetBrains benchmark shows that even optimized local-file VMs are much slower than Docker for this workload.

Do not choose multi-tenant lazy Docker for untrusted users. It is a useful performance baseline, but shared-kernel Docker is not equivalent to tenant isolation.

## Next Work

1. Prototype single-tenant lazy Docker end to end with node-local image cache and durable registry-backed images.
2. Measure startup and disk behavior for that path using the same cold/warm cache definitions as the Orca benchmarks.
3. Keep VM + NBD as the fallback/shared-infra path, with known optimization items: remove or hide `sideload_orca_init`, remove or hide `attach_nbd_device`, and improve sequential reads.
4. Keep multi-tenant lazy Docker only as a performance baseline or trusted-workload mode.

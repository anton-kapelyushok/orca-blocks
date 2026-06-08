# JetBrains Workspace Real-Env Timings

Run: 2026-06-08T10:49Z-11:10Z

| Field | Value |
| --- | --- |
| Host | DigitalOcean Linux VM, 2 vCPU / 4 GiB RAM / 80 GiB disk |
| Image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Marker | `Join this workspace using URL:` |
| Runtime limits | 1 vCPU / 3 GiB RAM |
| Orca rootfs | 6 GiB |
| Orca base image | `base-2a1addf3-2cd4-4f0c-96f4-3261c17d00e6` |

Notes:

- Stack was reset with `docker compose down -v` before the Orca base image build.
- Docker image was already present on the host.
- Docker used `--network host --cpus=1 --memory=3g` because the VM's Docker bridge was broken.
- Host page cache was dropped before Docker and local Firecracker.
- Orca first startup used a cold node block cache; Orca second startup reused the same node.

## Startup Results

| Target | Cache shape | Init ready | Dock HTTP | Workspace Server | Join URL |
| --- | --- | ---: | ---: | ---: | ---: |
| Docker local image | Pre-pulled image, cold host cache | n/a | 9.19s | 16.61s | 18.71s |
| Firecracker local, no NBD | Local ext4 rootfs, cold host cache | 2.01s | 18.04s | 35.06s | 39.06s |
| QEMU q35 local, no NBD | Local ext4 rootfs, cold host cache | 5.01s | 19.02s | 39.05s | 44.05s |
| Orca first env on node-1 | Cold node block cache | 5.30s | 30.48s | 51.61s | 55.64s |
| Orca second env on node-1 | Same node, warm-ish local cache | 4.28s | 26.45s | 45.56s | 50.60s |

## QEMU q35 Fresh Rerun

Run: `2026-06-08T13:40Z`

| Target | Cache shape | Init ready | Dock HTTP | Workspace Server | Join URL |
| --- | --- | ---: | ---: | ---: | ---: |
| QEMU q35 local, no NBD | Local ext4 rootfs, cold host cache, reused cached rootfs and `orca-init` | 6.01s | 18.02s | 33.03s | 36.03s |
| QEMU q35 local, no NBD | Repeat run, same settings | 5.01s | 18.01s | 31.02s | 36.03s |

This rerun used `ORCA_INIT_REUSE=1 VCPU_COUNT=1 MEM_SIZE_MIB=3072 ROOTFS_SIZE_MB=6144`, dropped host caches before launch, and measured until `Join this workspace using URL:`.

## Orca Setup Timings

| Phase | First env | Second env |
| --- | ---: | ---: |
| attach_nbd_device | 1.01s | 1.01s |
| sideload_orca_init | 1.65s | 1.52s |
| setup_firecracker_network | 0.19s | 0.05s |
| run_firecracker | 2.05s | 1.56s |
| request_to_first_user_output | 5.00s | 4.16s |

## Orca Base Image Build

| Step | Time |
| --- | ---: |
| pull_image | 0.16s |
| inspect_image | 0.13s |
| create_container | 0.38s |
| export_rootfs_tar | 143.93s |
| unpack_rootfs_tar | 39.22s |
| unmount_rootfs | 2.10s |
| import_rootfs_snapshot | 223.20s |
| total build duration | 410.18s |

| Import metric | Value |
| --- | ---: |
| rootfs_size_bytes | 6,442,450,944 |
| chunks | 1,536 |
| uploaded | 753 |
| skipped | 783 |

Snapshot id: `fc467da3-b55e-447d-979f-0fa61483a53f`

## Takeaway

| Comparison | Delta |
| --- | ---: |
| Firecracker local no NBD vs Docker | +20.35s |
| QEMU q35 local no NBD vs Docker | +25.34s |
| QEMU q35 local no NBD vs Firecracker local no NBD | +4.99s |
| Orca first env vs Firecracker local no NBD | +16.58s |
| Orca second env vs Firecracker local no NBD | +11.54s |
| Orca first env vs QEMU q35 local no NBD | +11.59s |
| Orca second env vs QEMU q35 local no NBD | +6.55s |
| Orca second env vs Orca first env | -5.04s |

Orca remains materially slower than local VMMs without NBD for the JetBrains workload. QEMU q35 on a local file lands between Firecracker local and Orca. The second Orca startup improves on the same node, but the main app-level gap remains: JetBrains reaches Dock HTTP at 26-30s and Join URL at 50-56s inside Orca, while `run_firecracker` itself is only 1.6-2.1s.

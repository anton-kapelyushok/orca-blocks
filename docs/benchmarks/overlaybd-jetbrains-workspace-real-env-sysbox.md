# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T17:18:16Z`

| Field | Value |
| --- | --- |
| Source image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Mirrored normal image | `127.0.0.1:5000/orca/overlaybd-jb-real:normal-jb-real-sysbox-20260608T171059Z` |
| OverlayBD image | `127.0.0.1:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z` |
| Runtime label | `sysbox-runc-cni` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| network mode | `cni` |
| allow new privileges | `True` |
| Marker | `Join this workspace using URL:` |
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-jb-real-sysbox-cni-newprivs-20260608T171801Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 711 ms / 0.71 s | n/a | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| `Dock HTTP Api listening` | 5959 ms / 5.96 s |
| `Workspace Server listening` | 8963 ms / 8.96 s |
| `Version: 261.643` | 11066 ms / 11.07 s |
| `Smart Mode: enabled` | 11067 ms / 11.07 s |
| `Published to JetBrains Relay: true` | 11067 ms / 11.07 s |
| `Join this workspace using URL:` | 11067 ms / 11.07 s |

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 11067 ms / 11.07 s |
| Process return code before stop | `None` |

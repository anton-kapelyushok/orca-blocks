# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T18:35:33Z`

| Field | Value |
| --- | --- |
| Source image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Mirrored normal image | `178.128.247.74:5000/orca/overlaybd-jb-real:normal-jb-real-sysbox-20260608T171059Z` |
| OverlayBD image | `178.128.247.74:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z` |
| Runtime label | `sysbox-runc-cni` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| network mode | `cni` |
| allow new privileges | `True` |
| repoBlobUrl scheme rewrite | `http` |
| skip run | `False` |
| Marker | `Join this workspace using URL:` |
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-warm-jb-real-sysbox-20260608T171059Z-20260608T183522Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 217 ms / 0.22 s | n/a | n/a |
| Rewrite OverlayBD repoBlobUrl scheme | 18 ms / 0.02 s | changed configs: 0 | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 1005 ms / 1.00 s |
| `Dock HTTP Api listening` | 5210 ms / 5.21 s |
| `Workspace Server listening` | 7608 ms / 7.61 s |
| `Version: 261.643` | 8119 ms / 8.12 s |
| `Smart Mode: enabled` | 8119 ms / 8.12 s |
| `Published to JetBrains Relay: true` | 8119 ms / 8.12 s |
| `Join this workspace using URL:` | 8119 ms / 8.12 s |

First output text:

```text
[2m2026-06-08T18:35:23.472768Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 8119 ms / 8.12 s |
| Process return code before stop | `None` |

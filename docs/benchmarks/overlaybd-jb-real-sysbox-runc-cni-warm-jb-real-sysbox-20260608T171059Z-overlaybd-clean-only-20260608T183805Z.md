# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T18:38:29Z`

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
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-warm-jb-real-sysbox-20260608T171059Z-overlaybd-clean-only-20260608T183805Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 408 ms / 0.41 s | n/a | n/a |
| Rewrite OverlayBD repoBlobUrl scheme | 5 ms / 0.01 s | changed configs: 27 | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 3358 ms / 3.36 s |
| `Dock HTTP Api listening` | 12172 ms / 12.17 s |
| `Workspace Server listening` | 20118 ms / 20.12 s |
| `Version: 261.643` | 20632 ms / 20.63 s |
| `Smart Mode: enabled` | 20632 ms / 20.63 s |
| `Published to JetBrains Relay: true` | 20632 ms / 20.63 s |
| `Join this workspace using URL:` | 20632 ms / 20.63 s |

First output text:

```text
[2m2026-06-08T18:38:09.473898Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 20632 ms / 20.63 s |
| Process return code before stop | `None` |

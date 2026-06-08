# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T18:35:11Z`

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
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-cold-jb-real-sysbox-20260608T171059Z-20260608T183445Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 481 ms / 0.48 s | n/a | n/a |
| Rewrite OverlayBD repoBlobUrl scheme | 6 ms / 0.01 s | changed configs: 27 | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 3644 ms / 3.64 s |
| `Dock HTTP Api listening` | 11346 ms / 11.35 s |
| `Workspace Server listening` | 20811 ms / 20.81 s |
| `Version: 261.643` | 21391 ms / 21.39 s |
| `Smart Mode: enabled` | 21391 ms / 21.39 s |
| `Published to JetBrains Relay: true` | 21391 ms / 21.39 s |
| `Join this workspace using URL:` | 21391 ms / 21.39 s |

First output text:

```text
[2m2026-06-08T18:34:50.124233Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 21391 ms / 21.39 s |
| Process return code before stop | `None` |

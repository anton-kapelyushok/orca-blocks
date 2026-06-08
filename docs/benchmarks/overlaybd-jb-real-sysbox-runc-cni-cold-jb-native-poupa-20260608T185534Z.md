# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T18:56:02Z`

| Field | Value |
| --- | --- |
| Source image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Mirrored normal image | `178.128.247.74:5000/orca/overlaybd-jb-real:normal-jb-native-poupa` |
| OverlayBD image | `178.128.247.74:5000/orca/overlaybd-jb-real:native-poupa-obd-20260608T185415Z` |
| Runtime label | `sysbox-runc-cni` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| network mode | `cni` |
| allow new privileges | `True` |
| repoBlobUrl scheme rewrite | `http` |
| skip run | `False` |
| Marker | `Join this workspace using URL:` |
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-cold-jb-native-poupa-20260608T185534Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 456 ms / 0.46 s | n/a | n/a |

| Rewrite OverlayBD repoBlobUrl scheme | 7 ms / 0.01 s | changed configs: 28 | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 3691 ms / 3.69 s |
| `Dock HTTP Api listening` | 14100 ms / 14.10 s |
| `Workspace Server listening` | 22815 ms / 22.82 s |
| `Version: 261.643` | 23583 ms / 23.58 s |
| `Smart Mode: enabled` | 23583 ms / 23.58 s |
| `Published to JetBrains Relay: true` | 23583 ms / 23.58 s |
| `Join this workspace using URL:` | 23583 ms / 23.58 s |

First output text:

```text
[2m2026-06-08T18:55:38.892818Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 23583 ms / 23.58 s |
| Process return code before stop | `None` |

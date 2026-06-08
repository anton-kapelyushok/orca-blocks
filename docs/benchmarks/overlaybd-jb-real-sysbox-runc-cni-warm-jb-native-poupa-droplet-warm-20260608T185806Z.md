# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T18:58:19Z`

| Field | Value |
| --- | --- |
| Source image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Mirrored normal image | `127.0.0.1:5000/orca/overlaybd-jb-real:normal-jb-native-poupa-droplet-warm` |
| OverlayBD image | `127.0.0.1:5000/orca/overlaybd-jb-real:native-poupa-obd-20260608T185415Z` |
| Runtime label | `sysbox-runc-cni` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| network mode | `cni` |
| allow new privileges | `True` |
| repoBlobUrl scheme rewrite | `disabled` |
| skip run | `False` |
| Marker | `Join this workspace using URL:` |
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-warm-jb-native-poupa-droplet-warm-20260608T185806Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 402 ms / 0.40 s | n/a | n/a |
| Rewrite OverlayBD repoBlobUrl scheme | skipped | changed configs: 0 | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 868 ms / 0.87 s |
| `Dock HTTP Api listening` | 4753 ms / 4.75 s |
| `Workspace Server listening` | 7579 ms / 7.58 s |
| `Version: 261.643` | 9644 ms / 9.64 s |
| `Smart Mode: enabled` | 9644 ms / 9.64 s |
| `Published to JetBrains Relay: true` | 9644 ms / 9.64 s |
| `Join this workspace using URL:` | 9644 ms / 9.64 s |

First output text:

```text
[2m2026-06-08T18:58:07.661370Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 9644 ms / 9.64 s |
| Process return code before stop | `None` |

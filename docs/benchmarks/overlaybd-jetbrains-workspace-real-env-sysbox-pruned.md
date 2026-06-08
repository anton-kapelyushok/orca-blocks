# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T17:21:08Z`

| Field | Value |
| --- | --- |
| Source image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Mirrored normal image | `127.0.0.1:5000/orca/overlaybd-jb-real:normal-jb-real-sysbox-20260608T171059Z` |
| OverlayBD image | `127.0.0.1:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z` |
| Runtime label | `sysbox-runc-cni-pruned` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| network mode | `cni` |
| allow new privileges | `True` |
| Marker | `Join this workspace using URL:` |
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-pruned-jb-real-sysbox-cni-pruned-20260608T172053Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 1499 ms / 1.50 s | n/a | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 833 ms / 0.83 s |
| `Dock HTTP Api listening` | 4912 ms / 4.91 s |
| `Workspace Server listening` | 8229 ms / 8.23 s |
| `Version: 261.643` | 10587 ms / 10.59 s |
| `Smart Mode: enabled` | 10587 ms / 10.59 s |
| `Published to JetBrains Relay: true` | 10587 ms / 10.59 s |
| `Join this workspace using URL:` | 10587 ms / 10.59 s |

First output text:

```text
[2m2026-06-08T17:20:55.683560Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 10587 ms / 10.59 s |
| Process return code before stop | `None` |

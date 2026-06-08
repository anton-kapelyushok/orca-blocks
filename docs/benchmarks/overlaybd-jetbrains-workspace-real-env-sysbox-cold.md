# OverlayBD JetBrains Workspace Real-Env Timing

Generated: `2026-06-08T17:27:43Z`

| Field | Value |
| --- | --- |
| Source image | `registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643` |
| Mirrored normal image | `127.0.0.1:5000/orca/overlaybd-jb-real:normal-jb-real-sysbox-20260608T171059Z` |
| OverlayBD image | `127.0.0.1:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z` |
| Runtime label | `sysbox-runc-cni-cold` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| network mode | `cni` |
| allow new privileges | `True` |
| Marker | `Join this workspace using URL:` |
| Log file | `/root/overlaybd-jb-real-sysbox-runc-cni-cold-jb-real-sysbox-cni-cold-20260608T172731Z.log` |

## Publish / Pull

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Mirror and convert original image to OverlayBD | skipped | skipped | None |
| rpull OverlayBD image | 1073 ms / 1.07 s | n/a | n/a |

## Runtime Markers

| Marker | First seen |
| --- | ---: |
| first output line | 996 ms / 1.00 s |
| `Dock HTTP Api listening` | 4867 ms / 4.87 s |
| `Workspace Server listening` | 7754 ms / 7.75 s |
| `Version: 261.643` | 8632 ms / 8.63 s |
| `Smart Mode: enabled` | 8632 ms / 8.63 s |
| `Published to JetBrains Relay: true` | 8632 ms / 8.63 s |
| `Join this workspace using URL:` | 8632 ms / 8.63 s |

First output text:

```text
[2m2026-06-08T17:27:33.856812Z[0m [32m INFO[0m [2mfleet::artefact[0m[2m:[0m dock metadata file exists, inferring JBR version from it
```

## Result

| Field | Value |
| --- | ---: |
| Joined | `True` |
| Elapsed to Join URL / stop | 8632 ms / 8.63 s |
| Process return code before stop | `None` |

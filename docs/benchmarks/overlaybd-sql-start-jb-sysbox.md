# OverlayBD SQL Start Scenarios

Generated: `2026-06-08T17:08:15Z`

Docker stays on normal storage; OverlayBD is used through `ctr --snapshotter overlaybd`.

Source image:

```text
registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643
```

| Runtime | Value |
| --- | --- |
| Runtime label | `sysbox-runc` |
| containerd runtime | `io.containerd.runc.v2` |
| runc binary | `/usr/bin/sysbox-runc` |
| skip publish | `1` |

| Image | Ref |
| --- | --- |
| Base normal | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:base-normal-jb-20260608T165952Z` |
| Derived normal | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:derived-normal-jb-20260608T165952Z` |
| Base OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:base-obd-jb-20260608T165952Z` |
| Derived OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:derived-obd-jb-20260608T165952Z` |

## Publish-Time Conversion

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Base conversion | skipped | skipped | skipped |
| Derived conversion | skipped | skipped | skipped |

## Start-Time Scenarios

| Scenario | Preloaded local image | Prep rpull | Measured rpull | First output | Total | Result |
| --- | --- | ---: | ---: | ---: | ---: | --- |
| no local image | none | 0 ms | 256 ms | 1210 ms | 1466 ms | ok |
| base image local | base | 327 ms | 262 ms | 1143 ms | 1732 ms | ok |
| derived image local | derived | 239 ms | 234 ms | 1068 ms | 1541 ms | ok |

Conclusion:

- `ctr --snapshotter overlaybd` works with `/usr/bin/sysbox-runc` on this VM.
- This run reused the already converted JB OverlayBD refs from the non-Sysbox run, so publish-time conversion is intentionally skipped.
- Sysbox first-output time was about 1.07-1.21 seconds. In the previous runc run, first-output time was 0.55-1.10 seconds, so Sysbox adds visible runtime startup overhead but stays in the same low-second envelope for this synthetic command.

Notes:

- `Prep rpull` is setup for the scenario and is not user-visible start time.
- `Measured rpull` is the lazy image pull/fetch step for the target derived image.
- `First output` starts after measured rpull and ends when the container prints `FIRST_OUTPUT`.
- This benchmark does not delete low-level containerd content blobs; it removes image refs and relies on fresh tags.

## no_local_image output

```text
FIRST_OUTPUT
base-layer
user-layer
```

## base_local output

```text
FIRST_OUTPUT
base-layer
user-layer
```

## derived_local output

```text
FIRST_OUTPUT
base-layer
user-layer
```

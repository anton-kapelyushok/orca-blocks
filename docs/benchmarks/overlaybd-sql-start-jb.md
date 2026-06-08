# OverlayBD SQL Start Scenarios

Generated: `2026-06-08T17:04:30Z`

Docker stays on normal storage; OverlayBD is used through `ctr --snapshotter overlaybd`.

Source image:

```text
registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643
```

| Image | Ref |
| --- | --- |
| Base normal | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:base-normal-jb-20260608T165952Z` |
| Derived normal | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:derived-normal-jb-20260608T165952Z` |
| Base OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:base-obd-jb-20260608T165952Z` |
| Derived OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-start-jb:derived-obd-jb-20260608T165952Z` |

## Publish-Time Conversion

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Base conversion | 241265 ms / 241.3 s | 1634457949 bytes / 1558.7 MiB | 30 |
| Derived conversion | 4009 ms / 4.0 s | 104063 bytes / 101.6 KiB | 3 |

## Start-Time Scenarios

| Scenario | Preloaded local image | Prep rpull | Measured rpull | First output | Total | Result |
| --- | --- | ---: | ---: | ---: | ---: | --- |
| no local image | none | 0 ms | 1521 ms | 1099 ms | 2620 ms | ok |
| base image local | base | 570 ms | 570 ms | 569 ms | 1709 ms | ok |
| derived image local | derived | 461 ms | 346 ms | 550 ms | 1357 ms | ok |

Conclusion:

- The one-time base conversion is expensive for this large image: about 4 minutes and 1.56 GiB of new registry data.
- The derived conversion behaves like the product path wants: about 4 seconds and only 102 KiB of additional registry data.
- Start path is fast once converted: first user output after measured lazy pull was 0.55-1.10 seconds, with total measured path 1.36-2.62 seconds.

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

# OverlayBD SQL Start Scenarios

Generated: `2026-06-08T16:56:22Z`

Docker stays on normal storage; OverlayBD is used through `ctr --snapshotter overlaybd`.

| Image | Ref |
| --- | --- |
| Base normal | `127.0.0.1:5000/orca/overlaybd-sql-start:base-normal-20260608T165610Z` |
| Derived normal | `127.0.0.1:5000/orca/overlaybd-sql-start:derived-normal-20260608T165610Z` |
| Base OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-start:base-obd-20260608T165610Z` |
| Derived OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-start:derived-obd-20260608T165610Z` |

## Publish-Time Conversion

| Step | Time | Registry delta | Blob delta |
| --- | ---: | ---: | ---: |
| Base conversion | 2342 ms | 6008444 bytes | 4 |
| Derived conversion | 1160 ms | 98234 bytes | 3 |

## Start-Time Scenarios

| Scenario | Preloaded local image | Prep rpull | Measured rpull | First output | Total | Result |
| --- | --- | ---: | ---: | ---: | ---: | --- |
| no_local_image | none | 0 | 202 | 403 | 605 | ok |
| base_local | base | 259 | 114 | 424 | 797 | ok |
| derived_local | derived | 135 | 115 | 316 | 566 | ok |

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


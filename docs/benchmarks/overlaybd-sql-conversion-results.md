# OverlayBD SQL Conversion Smoke

Generated: `2026-06-08T16:53Z`

Host: `root@178.128.247.74`, Ubuntu 22.04.5, kernel `5.15.0-171-generic`.

Docker stayed on normal `overlayfs`; OverlayBD was installed only as a
containerd snapshotter. The test used MySQL-backed `obdconv --dbstr`.

Setup scripts:

| Script | Purpose |
| --- | --- |
| `scripts/setup-sysbox-docker-vm.sh` | Docker/Sysbox/stargz VM baseline, Docker pinned to `28.5.2` |
| `scripts/setup-overlaybd-containerd-snapshotter.sh` | OverlayBD snapshotter for `ctr`, without Docker `storage-driver=overlaybd` |
| `scripts/measure-overlaybd-sql-conversion.sh` | SQL-backed base/derived conversion smoke |

Images:

| Image | Ref |
| --- | --- |
| Base normal | `127.0.0.1:5000/orca/overlaybd-sql-smoke:base-normal-20260608T165254Z` |
| Derived normal | `127.0.0.1:5000/orca/overlaybd-sql-smoke:derived-normal-20260608T165254Z` |
| Base OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-smoke:base-obd-20260608T165254Z` |
| Derived OverlayBD | `127.0.0.1:5000/orca/overlaybd-sql-smoke:derived-obd-20260608T165254Z` |

Results:

| Step | Time | Registry delta | Blob delta | Result |
| --- | ---: | ---: | ---: | --- |
| Base conversion | `3867 ms` | `5,988,220 bytes` | `4` | passed |
| Derived conversion | `1649 ms` | `85,818 bytes` | `3` | passed |
| Derived lazy run | `1190 ms` | n/a | n/a | passed |

Derived run output:

```text
FIRST_OUTPUT
base-layer
user-layer
```

DB rows after the run:

| Table | Rows |
| --- | ---: |
| `overlaybd_layers` | `8` |
| `overlaybd_manifests` | `0` |

Important observation:

```text
found remote layer for chainID sha256:0854555d70acaa318b38ee50bc667cb51ff6bf0757624624c7ff3b6fe17459a0
found remote layer for chainID sha256:dc3aa523119367fbdc3acf69db10777a184dea38ffe25b8f6991693f49ae9347
layer not found in remote
```

This is the behavior we wanted: base layers were found through the shared SQL
conversion DB, and only the derived/user layer needed new OverlayBD conversion.

Conclusion:

- SQL-backed OverlayBD conversion can produce a small derived-image delta.
- The converted derived image can be pulled lazily and run with
  `ctr --snapshotter overlaybd`.
- The Docker `storage-driver=overlaybd` path is not required for this flow and
  should be avoided for these tests.
- OverlayBD mounts may remain briefly after `ctr run --rm`; the script waits
  for cleanup instead of rebooting or mutating low-level state.

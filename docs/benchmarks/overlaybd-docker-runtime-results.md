# OverlayBD Docker Runtime Results

Generated: `2026-06-08T15:45:22Z`

Host: `root@68.183.1.109`, Ubuntu 22.04.5, kernel `5.15.0-171-generic`, 2 vCPU / 4 GiB RAM.

Configuration followed `containerd/accelerated-container-image/docs/DOCKER.md`:

| Component | Value |
| --- | --- |
| Docker | `29.5.3` |
| containerd | `2.2.4` |
| OverlayBD | `1.0.17` |
| overlaybd-snapshotter | `1.4.3` |
| OverlayBD `runtimeType` | `docker` |
| OverlayBD `rwMode` | `overlayfs` |
| OverlayBD `autoRemoveDev` | `true` |
| Docker `containerd-snapshotter` | `true` |
| Docker storage driver | `overlaybd` |

Validation:

| Check | Result |
| --- | --- |
| `ctr plugins ls | grep overlaybd` | `io.containerd.snapshotter.v1 overlaybd linux/amd64 ok` |
| `docker info` driver | `overlaybd` |
| Manual Redis image | Started successfully |
| OverlayBD mount during Redis run | `/dev/sda -> /var/lib/containerd/io.containerd.snapshotter.v1.overlaybd/snapshots/8/block/mountpoint` |
| Mount after `docker rm -f` / `docker run --rm` | Removed automatically |

Timings:

| Scenario | Timing |
| --- | ---: |
| Manual Redis image, cold pull + startup logs, timeout wrapper wall time | `50738 ms` |
| Manual Redis image, actual image present, `--entrypoint sh -lc 'echo FIRST_OUTPUT'` | `659 ms` |
| Manual Redis image, Docker image ref absent, short command | `2221 ms` |
| Manual Redis image with `--runtime=sysbox-runc`, short command | failed in `1084 ms` |

Sysbox check:

| Scenario | Result |
| --- | --- |
| `docker run --runtime=sysbox-runc alpine:3.22 ...` | failed because Docker storage driver is `overlaybd` and Alpine is not an OverlayBD image |
| `docker run --runtime=sysbox-runc --entrypoint sh registry.hub.docker.com/overlaybd/redis:7.2.3_obd -lc 'echo ...'` | failed before user code |

Sysbox failure:

```text
docker: Error response from daemon: failed to create task for container:
failed to create shim task: OCI runtime create failed:
namespace {"time" ""} does not exist
```

Derived-image registry cache-state test:

Setup:

- Built a derived image locally:

  ```dockerfile
  FROM registry.hub.docker.com/overlaybd/redis:7.2.3_obd
  RUN echo user-layer > /user-layer.txt
  ENTRYPOINT ["sh", "-lc"]
  CMD ["echo USER_FIRST_OUTPUT"]
  ```

- Local build/run worked:

  ```text
  DERIVED_OK
  user-layer
  ```

- Installed native host registry from the `docker-registry` apt package, bound to `127.0.0.1:5000`, with auth disabled.
- Added `127.0.0.1:5000` as a Docker insecure registry.
- Tagged and pushed the derived image as `127.0.0.1:5000/orca/overlaybd-user:test`.
- Script added for replaying the test on a fresh host: `scripts/measure-overlaybd-derived-diff.sh`.

Results:

| Scenario | Result | Time |
| --- | --- | ---: |
| No image ref local | failed | `240 ms` |
| Root image local, user image ref absent | failed | `149 ms` |
| User image local | failed | `95 ms` |

Failure:

```text
docker: Error response from daemon:
failed to attach overlaybd for docker init layer:
failed to write enable for /sys/kernel/config/target/core/user_999999999/dev_43:
write /sys/kernel/config/target/core/user_999999999/dev_43/enable:
no such file or directory
```

Observation:

- After pulling the pushed derived image from the local registry, Docker listed it as only `130kB`, while the original local derived image was `89.5MB`.
- This suggests a normal Docker build/push of a derived image from an OverlayBD base does not produce a self-contained valid OverlayBD/lazy image for the registry handoff case.
- The public OverlayBD image remains runnable; the locally built and registry-pushed derived image is the failing part.

Derived-image conversion test:

- Pulled the normal pushed image into the containerd local content store:

  ```bash
  /opt/overlaybd/snapshotter/ctr -n moby images pull --local --plain-http 127.0.0.1:5000/orca/overlaybd-user:test
  ```

- Converted and pushed it as an OverlayBD image:

  ```bash
  /opt/overlaybd/snapshotter/ctr -n moby obdconv --plain-http --fstype ext4 \
    127.0.0.1:5000/orca/overlaybd-user:test \
    127.0.0.1:5000/orca/overlaybd-user-obd:test
  /opt/overlaybd/snapshotter/ctr -n moby images push --local --plain-http \
    127.0.0.1:5000/orca/overlaybd-user-obd:test
  ```

DB-backed conversion note:

- Upstream has a MySQL schema sample at `cmd/convertor/resources/samples/mysql.conf`.
- The converter uses MySQL placeholders and opens the DB as `sql.Open("mysql", dbstr)`, so Postgres is not a drop-in `dbstr` backend.
- The converter does not create tables itself; it reads/writes `overlaybd_layers` and `overlaybd_manifests`.
- `scripts/measure-overlaybd-derived-diff.sh` now has `SETUP_MYSQL=1` support to install/provision the sample schema and pass `--dbstr`.
- Use a Go MySQL DSN such as `overlaybd:overlaybd@tcp(127.0.0.1:3306)/overlaybd`; the `mysql://user:pass@host/db` URL form is not what the current code passes to the MySQL driver.

First-run registry growth:

| Metric | Value |
| --- | ---: |
| Registry before | `89,600,239 bytes` |
| Registry after | `172,032,837 bytes` |
| Delta | `82,432,598 bytes` |
| Blob delta | `11` |

Converted image run result:

| Scenario | Result |
| --- | --- |
| Public OverlayBD Redis smoke check after conversion | passed in `575 ms` |
| Converted derived image pulled from local registry | failed |

Converted image failure:

```text
docker: Error response from daemon:
failed to attach overlaybd for docker init layer:
failed to write enable for /sys/kernel/config/target/core/user_999999999/dev_62:
write /sys/kernel/config/target/core/user_999999999/dev_62/enable:
no such file or directory
```

Conclusion for commit/diff handoff:

- A Docker-built child image from an OverlayBD base can run locally before registry handoff.
- A normal Docker push/pull of that child image is not a valid lazy OverlayBD image handoff.
- Running `obdconv` on the child image required local base layer content and pushed about `82 MB` of converted data for a tiny `RUN echo user-layer` change.
- The converted image still failed when Docker pulled and ran it from the local registry.
- This does not currently satisfy the desired product property: "commit only the env diff, then start lazily on another node".

Notes:

- The Redis command is a long-running process. The wrapper used `timeout 12s`; Redis started and printed readiness logs, then was manually removed.
- The first Redis log appeared at `2026-06-08T15:44:16.127Z`; the command started at `2026-06-08T15:43:52.134Z`, so cold pull plus first log was about `23993 ms`.
- This Docker-mode path behaved better than the earlier containerd-mode cache experiments because Docker/OverlayBD auto-removed the device after container removal.
- Docker-mode OverlayBD plus Sysbox did not work on Docker `29.5.3` / Sysbox `0.7.0`; the failure looks like an OCI runtime spec compatibility issue around Docker's `time` namespace entry, not an OverlayBD mount failure.
- Do not mutate low-level OverlayBD/containerd content under mounted snapshots to simulate cache states; that previously produced stuck `rpull` and blocked TCMU/SCSI kernel workers.

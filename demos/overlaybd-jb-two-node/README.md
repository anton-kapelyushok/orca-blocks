# OverlayBD JB Workspace Master/Slave Demo

Interactive demo for the single-tenant Docker/Sysbox/OverlayBD path.
See [architecture.md](architecture.md) for the component map and data flow.

The topology names are intentional:

- `master`: the droplet that hosts the registry, default `root@178.128.247.74`.
- `slave`: the Google Cloud node, default `anton.kapeliushok@104.155.88.61`.

The local Python runner only orchestrates SSH, confirmations, and timing tables.
By default, the confirmation prompt stays compact and only shows which remote
script will run. Add `--show-commands` to print the expanded SSH command and the
main remote commands, such as `ctr rpull`, `ctr run`, `overlaybd-apply`,
`overlaybd-commit`, and registry `curl` uploads, before each step.

## Scenario

1. Clean demo environments on master and slave.
2. Run the base JetBrains Workspace image on master until the Join URL.
3. Run the base JetBrains Workspace image on slave until the Join URL.
4. Run the base JetBrains Workspace image on slave again until the Join URL.
5. Clone and build Spring Petclinic on slave.
6. Export the slave overlay upperdir, convert it to an OverlayBD layer, and push
   the derived image to the master registry.
7. Run `./mvnw -q -DskipTests package` on slave from the first-use derived
   image.
8. Run `./mvnw -q -DskipTests package` on master from the first-use derived
   image.
9. Run `./mvnw -q -DskipTests package` on master again.

Step 5 is "coldish": the workspace/base image layers have already been warmed on
slave by Steps 3 and 4, but the Petclinic checkout, Maven cache, build output,
and jar are new user state for this environment.

Steps 7 and 8 are also "coldish": the Petclinic checkout, `.m2`, and build
output are preserved in the derived image, but the new derived OverlayBD layer
may be first-use on that node after the commit.

The cleanup step keeps durable state intact: registry blobs and MySQL metadata
are not deleted. It clears local OverlayBD image refs, snapshots, content, and
cache directories so the demo warms each node through the scripted runs.

Every container-run step reports `first user text appears`, measured from the
host-side `ctr run` start until the first line of stdout/stderr is read from the
container.

The demo expects OverlayBD `rwMode=overlayfs`. Cleanup enforces it on both
nodes. This keeps Sysbox startup on the fast overlayfs path while preserving
container writes through the overlay upperdir.

## Benchmark Snapshot

Measured on the slave node, `anton.kapeliushok@104.155.88.61`, on 2026-06-08.
For the earlier Docker/Firecracker/Orca comparison, see
[JetBrains Workspace Real-Env Timings](../../docs/benchmarks/jetbrains-workspace-real-env-results.md).
The Docker runs use the original JetBrains image,
`registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643`.
The OverlayBD runs use the converted image,
`178.128.247.74:5000/orca/overlaybd-jb-real:obd-jb-real-sysbox-20260608T171059Z`.

For Docker cold, the local Docker image was removed before the run. For
OverlayBD cold, the demo cleanup removed local OverlayBD image refs, snapshots,
content, and caches while keeping the durable registry/MySQL state.

| Runtime | Cache state | Image prep | Workspace first log | Dock HTTP API | Workspace Server | Join URL | Join incl. prep |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Docker | cold image | 16,526 ms | 1,957 ms | 4,795 ms | 6,775 ms | 7,565 ms | 24,091 ms |
| Docker | warm image | 443 ms | 259 ms | 2,779 ms | 4,637 ms | 5,448 ms | 5,891 ms |
| OverlayBD + Sysbox | cold local cache | 513 ms | 3,424 ms | 12,385 ms | 21,055 ms | 22,049 ms | 22,562 ms |
| OverlayBD + Sysbox | warm local cache | 149 ms | 3,224 ms | 6,023 ms | 8,108 ms | 9,572 ms | 9,721 ms |
| OverlayBD + Sysbox | hot local container snapshot | none | 608 ms | 3,381 ms | 5,255 ms | 5,411 ms | 5,411 ms |

Docker cold pays for the full image pull up front. OverlayBD cold only pulls
metadata up front, but pays for first-use lazy reads while the workspace starts.
The warm Docker run is faster for this workspace-start path on this node.

The `Workspace first log` column is not the generic container-start floor. It is
the time until the real JetBrains Workspace entrypoint prints its first line.
The warm OverlayBD container can run a trivial command much sooner:

| Runtime | Command | First user text |
| --- | --- | ---: |
| OverlayBD + Sysbox + CNI | `sh -lc 'echo hi'` | 706 ms |
| OverlayBD + Sysbox, no CNI | `sh -lc 'echo hi'` | 556 ms |
| OverlayBD + runc, no CNI | `sh -lc 'echo hi'` | 151 ms |

For the warm workspace run, OverlayBD reaches the first workspace log at
3,224 ms and the Join URL at 9,572 ms, so about 6,348 ms is spent after the
first workspace log. Docker warm spends about 5,189 ms after its first workspace
log. The big difference is split between an early OverlayBD/JB-entrypoint
first-log penalty and slower workspace initialization after logging begins.

The `hot local container snapshot` row uses a different path: start the workspace
once, stop only the task, keep the local container/snapshot, then restart the
same container with `ctr tasks start`. It does not push or pull a committed
registry image, and it does not preserve process memory; the JVM starts again
from the existing local filesystem state.

## Run

```bash
python3 demos/overlaybd-jb-two-node/demo.py
```

Useful overrides:

```bash
python3 demos/overlaybd-jb-two-node/demo.py \
  --master root@178.128.247.74 \
  --slave anton.kapeliushok@104.155.88.61 \
  --registry 178.128.247.74:5000 \
  --master-registry 127.0.0.1:5000
```

For a non-interactive rehearsal:

```bash
python3 demos/overlaybd-jb-two-node/demo.py --yes --dry-run
```

For a verbose rehearsal with the pre-step command preview:

```bash
python3 demos/overlaybd-jb-two-node/demo.py --yes --dry-run --show-commands
```

## Structure

- `demo.py`: local interactive orchestrator; optionally prints each remote
  script invocation and main command before running it over SSH.
- `remote/cleanup.sh`: clears local demo state and OverlayBD caches.
- `remote/run-workspace-until-join.sh`: runs the workspace image until the Join
  URL.
- `remote/petclinic-build-mutable.sh`: clones Spring Petclinic on slave, runs
  the first Maven package build, stops the task, and records the overlay
  upperdir plus the base OverlayBD config.
- `remote/commit-snapshot.sh`: exports the overlay upperdir as an OCI diff tar,
  converts it into an OverlayBD layer with `overlaybd-apply` and
  `overlaybd-commit`, then pushes a derived manifest.
- `remote/petclinic-build-repeat.sh`: runs the derived image and measures a
  repeat Maven package build against the preserved checkout, `.m2`, and jar.

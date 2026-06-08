# OverlayBD JB Workspace Two-Node Demo

Interactive demo for the single-tenant Docker/Sysbox/OverlayBD path.

The local Python runner only orchestrates SSH, confirmations, and timing tables.
It prints the SSH/script invocation for each step. The remote scripts print the
actual main commands, such as `ctr rpull`, `ctr run`, `tasks exec`,
`overlaybd-commit`, and registry `curl` uploads, immediately before they run.

## Scenario

1. Clean demo environments and local OverlayBD caches on both nodes.
2. Run the base JetBrains Workspace image on node1 until the Join URL,
   warming node1 by executing the environment.
3. Run the base JetBrains Workspace image on node2 until the Join URL,
   warming node2 by executing the environment.
4. Run the base JetBrains Workspace image on node1 again until the Join URL,
   showing the warm-node timing after Step 2 populated local caches.
5. Start the base image on node1 only long enough to create a writable
   OverlayBD snapshot, touch a file, and stop it. The nodes are kept in
   OverlayBD `rwMode=dev`, so every run has native writable block state.
6. Commit the node1 writable OverlayBD block layer and push a derived image.
7. Run the derived image on node2 until the Join URL.
8. Verify the touched file exists in the derived image on node2.

The cleanup step keeps durable state intact: registry blobs and MySQL metadata
are not deleted. It clears local OverlayBD image refs, snapshots, content, and
cache directories so the demo warms the nodes through the first real runs.

The demo expects OverlayBD `rwMode=dev`. Cleanup enforces it on both nodes.
The setup scripts now default to that mode as well; pass
`OVERLAYBD_RW_MODE=overlayfs` only for comparison experiments.

## Run

```bash
python3 demos/overlaybd-jb-two-node/demo.py
```

Useful overrides:

```bash
python3 demos/overlaybd-jb-two-node/demo.py \
  --node1 anton.kapeliushok@104.155.88.61 \
  --node2 root@178.128.247.74 \
  --registry 178.128.247.74:5000
```

For a non-interactive rehearsal:

```bash
python3 demos/overlaybd-jb-two-node/demo.py --yes --dry-run
```

## Structure

- `demo.py`: local interactive orchestrator; prints each remote script
  invocation before running it over SSH.
- `remote/cleanup.sh`: clears local demo state and OverlayBD caches.
- `remote/run-workspace-until-join.sh`: runs an image until the Join URL.
- `remote/mutable-touch.sh`: starts the base image, touches the demo file, stops
  it, and leaves the writable OverlayBD block files for commit.
- `remote/commit-snapshot.sh`: commits the writable OverlayBD block layer and
  pushes a derived manifest.
- `remote/verify-touch.sh`: runs the derived image and verifies the touched file.

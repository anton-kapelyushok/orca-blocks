# Sysbox Docker VM Setup

Use this on a fresh Ubuntu benchmark VM when testing the single-tenant Docker,
Sysbox, and stargz paths.

```bash
scp scripts/setup-sysbox-docker-vm.sh root@VM_IP:/root/setup-sysbox-docker-vm.sh
ssh root@VM_IP 'bash /root/setup-sysbox-docker-vm.sh'
```

The script installs and configures:

| Component | Default |
| --- | --- |
| Docker Engine | `28.5.2`, held via apt |
| Docker containerd image store | enabled |
| Sysbox CE | `0.7.0` |
| Docker runtime | `sysbox-runc` |
| Native registry | `127.0.0.1:5000` |
| stargz tools | `v0.18.2` under `/opt/orca/stargz-tools` |
| Docker bridge | `172.20.0.1/16` |
| Docker default address pool | `172.25.0.0/16`, size `24` |

Useful overrides:

```bash
SYSBOX_VERSION=0.7.0 \
DOCKER_PACKAGE_VERSION='5:28.5.2-1~ubuntu.22.04~jammy' \
REGISTRY_ADDR=127.0.0.1:5000 \
STARGZ_VERSION=v0.18.2 \
./scripts/setup-sysbox-docker-vm.sh
```

Docker is pinned below `29.x` because Docker `29.5.3` failed the Sysbox smoke
test on Ubuntu 22.04 with:

```text
OCI runtime create failed: namespace {"time" ""} does not exist
```

Disable optional pieces:

```bash
INSTALL_NATIVE_REGISTRY=0 INSTALL_STARGZ_TOOLS=0 ./scripts/setup-sysbox-docker-vm.sh
```

The script intentionally does not configure Docker's storage driver as
OverlayBD. That path is separate:

```bash
./scripts/setup-overlaybd-docker-runtime.sh
```

Do not combine the two on the same throwaway VM unless the benchmark explicitly
needs Docker `storage-driver=overlaybd`; it changes Docker's behavior for normal
images and made cleanup fragile in the OverlayBD experiments.

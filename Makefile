.PHONY: up down test unit integration demo firecracker-assets firecracker-rootfs firecracker-initramfs firecracker-boot-check logs clean remote-check remote-authorize-key remote-enable-passwordless-sudo remote-setup remote-sync remote-shell remote-test remote-demo remote-firecracker-assets remote-firecracker-rootfs remote-firecracker-initramfs remote-firecracker-boot-check remote-firecracker-docker-test remote-env-api-test remote-sandbox-up remote-logs remote-down remote-clean

GO_CACHE_DIR ?= $(CURDIR)/.gocache
REMOTE_HOST ?=
REMOTE_DIR ?= ~/orca-blocks
LOCAL_PUBLIC_KEY ?= $(HOME)/.ssh/id_ed25519.pub
REMOTE_SSH ?= ssh
REMOTE_SCP ?= scp
REMOTE_RSYNC ?= rsync
REMOTE_PORT ?=
REMOTE_SSH_PORT_OPT = $(if $(REMOTE_PORT),-p $(REMOTE_PORT),)
REMOTE_SCP_PORT_OPT = $(if $(REMOTE_PORT),-P $(REMOTE_PORT),)
REMOTE_SSH_OPTS ?= $(REMOTE_SSH_PORT_OPT)
REMOTE_SCP_OPTS ?= $(REMOTE_SCP_PORT_OPT)
REMOTE_TTY_SSH_OPTS ?= -tt
REMOTE_RSYNC_SSH_OPTS ?= $(REMOTE_SSH_OPTS)
REMOTE_RSYNC_OPTS ?= -az --delete \
	--exclude .git \
	--exclude .gocache \
	--exclude firecracker-assets \
	--exclude node_modules \
	--exclude vendor
FORCE ?= false
REBUILD_BASE ?= false
FIRECRACKER_BOOT_MODE ?= initramfs
COMPOSE_BUILD ?= false
COMPOSE_BUILD_FLAG = $(if $(filter true,$(COMPOSE_BUILD)),--build,)

up:
	docker compose up --build -d

down:
	docker compose down

unit:
	GOCACHE=$(GO_CACHE_DIR) go test -count=1 -v ./pkg/...

integration:
	docker compose up --build -d
	GOCACHE=$(GO_CACHE_DIR) go test -count=1 -v -tags=integration ./integration

test: unit integration

demo:
	docker compose up --build -d
	./scripts/demo.sh

firecracker-rootfs:
	FORCE=$(FORCE) REBUILD_BASE=$(REBUILD_BASE) ./scripts/build-alpine-rootfs.sh

firecracker-initramfs:
	FORCE=$(FORCE) ./scripts/build-initramfs.sh

firecracker-assets:
	./scripts/download-firecracker-assets.sh

firecracker-boot-check:
	FIRECRACKER_BOOT_MODE=$(FIRECRACKER_BOOT_MODE) ./scripts/check-firecracker-boot.sh

logs:
	docker compose logs -f --tail=200

clean:
	docker compose down -v

remote-check:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-check REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'set -eu; hostname; uname -a; echo "kvm device:"; ls -l /dev/kvm; echo "virt flags:"; grep -Ewc "vmx|svm" /proc/cpuinfo; echo "nbd devices:"; find /dev -maxdepth 1 -name "nbd[0-9]*" | sort -V | head | xargs -r ls -l; command -v docker >/dev/null && docker version --format "{{.Server.Version}}" || true; docker compose version || true'

remote-authorize-key:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-authorize-key REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	@test -f "$(LOCAL_PUBLIC_KEY)" || (echo "LOCAL_PUBLIC_KEY not found: $(LOCAL_PUBLIC_KEY)" >&2; exit 2)
	cat "$(LOCAL_PUBLIC_KEY)" | $(REMOTE_SSH) $(REMOTE_TTY_SSH_OPTS) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'set -eu; pub=$$(cat); mkdir -p ~/.ssh; chmod 700 ~/.ssh; touch ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys; grep -qxF "$$pub" ~/.ssh/authorized_keys || printf "%s\n" "$$pub" >> ~/.ssh/authorized_keys'

remote-enable-passwordless-sudo:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-enable-passwordless-sudo REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_TTY_SSH_OPTS) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'set -eu; user=$$(id -un); rule="$$user ALL=(ALL) NOPASSWD:ALL"; tmp=$$(mktemp); printf "%s\n" "$$rule" > "$$tmp"; echo "installing passwordless sudo rule for $$user"; sudo install -m 0440 "$$tmp" /etc/sudoers.d/orca-remote-dev; rm -f "$$tmp"; sudo visudo -cf /etc/sudoers.d/orca-remote-dev; sudo -n true; echo "passwordless sudo enabled for $$user"'

remote-setup: remote-enable-passwordless-sudo
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-setup REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SCP) $(REMOTE_SCP_OPTS) scripts/remote-setup.sh $(REMOTE_HOST):/tmp/orca-remote-setup.sh
	$(REMOTE_SSH) $(REMOTE_TTY_SSH_OPTS) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'chmod +x /tmp/orca-remote-setup.sh && /tmp/orca-remote-setup.sh'

remote-sync:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-sync REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'mkdir -p $(REMOTE_DIR)'
	$(REMOTE_RSYNC) -e '$(REMOTE_SSH) $(REMOTE_RSYNC_SSH_OPTS)' $(REMOTE_RSYNC_OPTS) ./ $(REMOTE_HOST):$(REMOTE_DIR)/

remote-shell:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-shell REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && exec bash -l'

remote-test: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && make test'

remote-demo: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && make demo'

remote-firecracker-rootfs: remote-sync
	$(REMOTE_SSH) $(REMOTE_TTY_SSH_OPTS) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && FORCE=$(FORCE) REBUILD_BASE=$(REBUILD_BASE) make firecracker-rootfs'

remote-firecracker-initramfs: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && FORCE=$(FORCE) make firecracker-initramfs'

remote-firecracker-assets: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && make firecracker-assets'

remote-firecracker-boot-check: remote-sync
	$(REMOTE_SSH) $(REMOTE_TTY_SSH_OPTS) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && FIRECRACKER_BOOT_MODE=$(FIRECRACKER_BOOT_MODE) make firecracker-boot-check'

remote-firecracker-docker-test: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && FIRECRACKER_BOOT_MODE=rootfs TOXIPROXY_S3_TOXICS_ENABLED=false docker compose up $(COMPOSE_BUILD_FLAG) -d node-1 node-2 control-service && FIRECRACKER_BOOT_MODE=rootfs FIRECRACKER_DOCKER_TEST=true GOCACHE=$$(pwd)/.gocache go test -count=1 -v -tags=integration ./integration -run TestFirecrackerDockerSmoke'

remote-env-api-test: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && TOXIPROXY_S3_TOXICS_ENABLED=false docker compose up $(COMPOSE_BUILD_FLAG) -d base-image-service control-service node-1 node-2 && BASE_IMAGE_FIRECRACKER_TEST=true GOCACHE=$$(pwd)/.gocache go test -count=1 -v -tags=integration ./integration -run TestEnvAPIStartResumeNodeOneThenNodeTwo'

remote-sandbox-up: remote-sync
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && TOXIPROXY_S3_TOXICS_ENABLED=false docker compose up $(COMPOSE_BUILD_FLAG) -d sandbox-service'

remote-logs:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-logs REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && docker compose logs -f --tail=200'

remote-down:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-down REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && docker compose down'

remote-clean:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-clean REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && docker compose down -v'

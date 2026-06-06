.PHONY: up down test unit integration demo logs clean remote-check remote-authorize-key remote-setup remote-sync remote-shell remote-test remote-demo remote-logs remote-down remote-clean

GO_CACHE_DIR ?= $(CURDIR)/.gocache
REMOTE_HOST ?=
REMOTE_DIR ?= ~/orca-blocks
LOCAL_PUBLIC_KEY ?= $(HOME)/.ssh/id_ed25519.pub
REMOTE_SSH ?= ssh
REMOTE_SCP ?= scp
REMOTE_RSYNC ?= rsync
REMOTE_SSH_OPTS ?=
REMOTE_SCP_OPTS ?=
REMOTE_TTY_SSH_OPTS ?= -tt
REMOTE_RSYNC_SSH_OPTS ?= $(REMOTE_SSH_OPTS)
REMOTE_RSYNC_OPTS ?= -az --delete \
	--exclude .git \
	--exclude .gocache \
	--exclude node_modules \
	--exclude vendor

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

logs:
	docker compose logs -f --tail=200

clean:
	docker compose down -v

remote-check:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-check REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'set -eu; hostname; uname -a; echo "kvm device:"; ls -l /dev/kvm; echo "virt flags:"; grep -Ewc "vmx|svm" /proc/cpuinfo; command -v docker >/dev/null && docker version --format "{{.Server.Version}}" || true; docker compose version || true'

remote-authorize-key:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-authorize-key REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	@test -f "$(LOCAL_PUBLIC_KEY)" || (echo "LOCAL_PUBLIC_KEY not found: $(LOCAL_PUBLIC_KEY)" >&2; exit 2)
	cat "$(LOCAL_PUBLIC_KEY)" | $(REMOTE_SSH) $(REMOTE_TTY_SSH_OPTS) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'set -eu; pub=$$(cat); mkdir -p ~/.ssh; chmod 700 ~/.ssh; touch ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys; grep -qxF "$$pub" ~/.ssh/authorized_keys || printf "%s\n" "$$pub" >> ~/.ssh/authorized_keys'

remote-setup:
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

remote-logs:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-logs REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && docker compose logs -f --tail=200'

remote-down:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-down REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && docker compose down'

remote-clean:
	@test -n "$(REMOTE_HOST)" || (echo "REMOTE_HOST is required, for example: make remote-clean REMOTE_HOST=vboxuser@192.168.178.201" >&2; exit 2)
	$(REMOTE_SSH) $(REMOTE_SSH_OPTS) $(REMOTE_HOST) 'cd $(REMOTE_DIR) && docker compose down -v'

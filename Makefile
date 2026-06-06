.PHONY: up down test unit integration demo logs clean

GO_CACHE_DIR ?= $(CURDIR)/.gocache

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

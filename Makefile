SHELL := /bin/bash

APP := fias-downloader
CMD := ./cmd/fias-downloader
DOCKER_IMAGE := fias-downloader-fias-downloader
API_URL ?= http://localhost:8888

.PHONY: help run test fmt vet build binary docker-images compose-up compose-down compose-rebuild migrate download download-full status

help:
	@echo "Available targets:"
	@echo "  make run             - run service locally"
	@echo "  make test            - run unit tests"
	@echo "  make fmt             - format Go code"
	@echo "  make vet             - run go vet"
	@echo "  make build           - build binary and Docker images"
	@echo "  make binary          - build binary to ./bin/$(APP)"
	@echo "  make docker-images   - build Docker images"
	@echo "  make compose-up      - docker compose up"
	@echo "  make compose-down    - docker compose down"
	@echo "  make compose-rebuild - docker compose down -v && up --build"
	@echo "  make migrate         - apply SQL migrations to Compose PostgreSQL"
	@echo "  make download        - start a manual full/delta sync"
	@echo "  make download-full   - start a forced latest-full download"
	@echo "  make status          - show current downloads"

run:
	go run $(CMD)

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

build: binary docker-images

binary:
	mkdir -p ./bin
	go build -o ./bin/$(APP) $(CMD)

docker-images:
	docker build --tag $(DOCKER_IMAGE) .

compose-up:
	docker compose up -d

compose-down:
	docker compose down

compose-rebuild:
	docker compose down -v
	docker compose up --build

migrate:
	@set -o pipefail; cat migrations/[0-9]*.sql | docker compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -1 -U fias -d fias

download:
	@curl --fail --show-error --silent --output /dev/null --request POST "$(API_URL)/trigger/sync" && echo "Запущена проверка и загрузка full/delta. Результат смотрите в логах сервиса."

download-full:
	@curl --fail --show-error --silent --output /dev/null --request POST "$(API_URL)/trigger/full" && echo "Запущена принудительная загрузка последней full-версии. Результат смотрите в логах сервиса."

status:
	@metrics=$$(curl --fail --show-error --silent "$(API_URL)/metrics") || exit; printf '%s\n' "$$metrics" | awk -f scripts/download-status.awk

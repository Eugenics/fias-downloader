SHELL := /bin/bash

APP := fias-downloader
CMD := ./cmd/fias-downloader
DOCKER_IMAGE := fias-downloader-fias-downloader

.PHONY: help run test fmt vet build binary docker-images compose-up compose-down compose-rebuild

help:
	@echo "Available targets:"
	@echo "  make run             - run service locally"
	@echo "  make test            - run unit tests"
	@echo "  make fmt             - format Go code"
	@echo "  make vet             - run go vet"
	@echo "  make build           - build binary and Docker images"
	@echo "  make binary          - build binary to ./bin/$(APP)"
	@echo "  make docker-images   - build Docker images"
	@echo "  make compose-up      - docker compose up --build"
	@echo "  make compose-down    - docker compose down"
	@echo "  make compose-rebuild - docker compose down -v && up --build"

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
	docker compose up --build

compose-down:
	docker compose down

compose-rebuild:
	docker compose down -v
	docker compose up --build

# Repository Guidelines

This repository contains a Go microservice that downloads FIAS/GAR address-reference full and delta archives, persists download state and logs in PostgreSQL, exposes an HTTP control API and Prometheus metrics, and runs as a Docker Compose stack.

## Project Structure & Module Organization

- `cmd/fias-downloader/` — service entrypoint (`main.go`), dependency wiring, HTTP server, scheduler loop, and graceful shutdown.
- `internal/` — application code (not importable by other modules):
  - `api/` HTTP handlers (`/healthz`, `/metrics`, `/trigger/sync`, `/trigger/full`)
  - `config/` YAML config loading, defaults, and validation
  - `fetcher/` source client for FIAS/GAR `GetAllDownloadFileInfo`
  - `downloader/` streaming download with `Range` resume, stall detection, retries, and SHA-256
  - `scheduler/` version selection and full/delta orchestration; a process-local mutex prevents concurrent cycles
  - `repo/` PostgreSQL persistence for `version_info`
  - `model/` DTOs/models
  - `metrics/` Prometheus instrumentation
  - `logstore/` asynchronous PostgreSQL `slog.Handler` and tee handler
- `migrations/` SQL schema (`0001_init.sql` for `version_info`, `0002_logs.sql` for `logs`). Schemas are also ensured automatically at startup.
- `config.example.yaml` — local configuration template; copy it to the ignored `config.yaml`.
- `deploy/config.docker.yaml` — configuration mounted by Compose; `deploy/prometheus/prometheus.yml` configures scraping.
- `docker-compose.yml`, `Dockerfile` — PostgreSQL, service, and Prometheus stack.
- Runtime archives are stored under the configured download directory (default `./data/downloads`); keep runtime data out of git.

## Build, Test, and Development Commands

- `go run ./cmd/fias-downloader` — run locally after `cp config.example.yaml config.yaml` and editing `postgres.dsn`.
- `go test ./...` — run unit tests.
- `go fmt ./...` — format Go code.
- `go vet ./...` — basic static checks.
- `docker compose up --build` — build and run the full stack.

The Docker build uses vendored dependencies and runs `CGO_ENABLED=0 go build -mod=vendor -o /out/fias-downloader ./cmd/fias-downloader` inside the builder image.

## Configuration & Runtime

- The service reads YAML only. It defaults to `config.yaml` in the current working directory; `--config /path/to/config.yaml` or `FIAS_CONFIG_PATH` selects another file. `FIAS_CONFIG_PATH` is the only environment-variable override.
- Required startup values are `postgres.dsn`, non-empty `source.url`, `download.dir`, `http.listen_addr`, and a positive `scheduler.poll_interval`. Duration fields use strings such as `30s`, `2m`, and `6h`.
- Compose mounts `deploy/config.docker.yaml` at `/app/config.yaml`, exposes service `8080`, PostgreSQL `5432`, and Prometheus `9090`, and persists named volumes `pgdata`, `downloads`, and `prometheus-data`.
- Large downloads intentionally use an HTTP client with no whole-request timeout; `download.stall_timeout` detects an inactive stream. Interrupted files are resumed with `Range`; a `200` response to a ranged request truncates and restarts the file.

## Coding Style & Naming Conventions

- Use standard Go formatting (`gofmt`); avoid manual alignment.
- Prefer small, focused packages under `internal/` over “util” grab-bags.
- Names: exported identifiers in `CamelCase`, unexported in `camelCase`.
- Keep config keys stable (`postgres`, `source`, `download`, `http`, `scheduler`); document new keys in `README.md` and the example YAML.
- Use `log/slog`; the service writes structured JSON to stdout and tees configured logs asynchronously into PostgreSQL.

## Testing Guidelines

- Tests use the standard `testing` package and `httptest` (see `internal/downloader/*_test.go`).
- Name tests `TestXxx` and files `*_test.go`.
- When changing download/resume logic, add coverage for both `206 Partial Content` and “Range ignored” fallback paths.

## Commit & Pull Request Guidelines

- Git history is not available in this environment; default to Conventional Commits (`feat:`, `fix:`, `chore:`) with an imperative summary.
- PRs: include “what/why”, how to run (`go test ./...` and/or `docker compose up --build`), and note any schema/config changes.

## HTTP API

- `GET /healthz` returns `ok`; `GET /metrics` serves Prometheus metrics.
- `POST /trigger/sync` starts an asynchronous normal full/delta cycle and returns `202 Accepted`.
- `POST /trigger/full` starts an asynchronous forced latest-full download and returns `202 Accepted`.

## Pitfalls

- Do not commit `config.yaml` or `/data/`; both are ignored. Do not put secrets in tracked example/deploy config files.
- The service requires reachable PostgreSQL and validates configuration before startup; Compose waits for PostgreSQL health before starting the service.
- A manual trigger and the scheduled cycle share a process-local lock; a concurrent cycle is skipped. Multiple service replicas are not protected by a distributed lock.
- `logs` has no built-in retention/rotation, and downloaded archives use local/volume storage only.

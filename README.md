# fias-downloader

Микросервис на Go для загрузки справочника адресов ФИАС/ГАР
с `https://fias.nalog.ru/WebServices/Public/GetAllDownloadFileInfo`.

Реализовано согласно ТЗ:

- если полной версии ещё нет — загружается последняя доступная полная версия (`GarXMLFullURL`);
- если полная версия уже загружена — догружаются только недостающие delta-версии (`GarXMLDeltaURL`), идущие после неё, строго по возрастанию, без пропусков;
- ручной эндпоинт для принудительной загрузки последней полной версии, даже если полная версия уже есть;
- докачка прерванных загрузок через HTTP `Range`, с фолбэком на полную перезагрузку, если сервер `Range` не поддерживает;
- состояние всех загрузок (full/delta по каждой версии) хранится в PostgreSQL, таблица `version_info` (соответствует `versionInfo` из ТЗ);
- **Prometheus-метрики** по HTTP `/metrics`;
- **логи сервиса дублируются в PostgreSQL** (таблица `logs`), в дополнение к обычному JSON-выводу в stdout;
- весь стек (сервис + PostgreSQL + Prometheus) поднимается одной командой через `docker compose`.

## Структура проекта

```
cmd/fias-downloader/main.go   — точка входа: сборка зависимостей, логирование, graceful shutdown
internal/config                — загрузка и валидация конфигурации из YAML-файла
internal/model                 — модели: SourceVersion (ответ источника), DownloadRecord (строка БД)
internal/fetcher                — клиент GetAllDownloadFileInfo
internal/repo                   — репозиторий PostgreSQL (таблица version_info)
internal/downloader               — потоковая загрузка с докачкой (Range) и ретраями
internal/scheduler                 — бизнес-логика выбора версий + оркестрация загрузки
internal/api                        — HTTP API ручного управления + /metrics
internal/metrics                     — Prometheus-метрики
internal/logstore                     — асинхронная запись логов в БД (slog.Handler) + TeeHandler
migrations/0001_init.sql              — DDL таблицы version_info
migrations/0002_logs.sql               — DDL таблицы logs
config.example.yaml                     — образец конфигурации, скопировать в config.yaml
deploy/config.docker.yaml                — конфигурация для запуска через docker-compose
deploy/prometheus/prometheus.yml          — конфиг скрейпа для Prometheus
deploy/grafana/                            — провижининг Grafana (источник данных + готовый дашборд)
docker-compose.yml, Dockerfile             — контейнеризация всего стека
```

## Конфигурация

Весь конфиг сервиса — в одном YAML-файле (структура и все ключи описаны в
`config.example.yaml`):

```yaml
postgres:
  dsn: "postgres://fias:fias@localhost:5432/fias?sslmode=disable"

source:
  url: "https://fias.nalog.ru/WebServices/Public/GetAllDownloadFileInfo"
  http_timeout: "30s"

download:
  dir: "./data/downloads"
  stall_timeout: "2m"
  max_retries: 5
  retry_base_delay: "5s"
  progress_save_interval: "5s"

http:
  listen_addr: ":8080"

scheduler:
  poll_interval: "6h"
```

По умолчанию сервис ищет `config.yaml` в текущей рабочей директории. Другой
путь можно задать флагом `--config /path/to/config.yaml` или переменной
окружения `FIAS_CONFIG_PATH` — это единственная переменная окружения,
которую понимает сервис; всё остальное читается только из YAML. Не
указанные в файле поля берутся из значений по умолчанию (см.
`internal/config/config.go`), обязательные поля (`postgres.dsn`, непустой
`source.url`, `download.dir`, `http.listen_addr`, положительный
`scheduler.poll_interval`) проверяются при старте — при их отсутствии
сервис не запустится и явно сообщит, чего не хватает.

## Запуск через Docker Compose (рекомендуется)

```bash
docker compose up --build
```

> Если вы уже запускали более раннюю версию `docker-compose.yml` с
> именованным volume `downloads` — он больше не используется и его можно
> удалить: `docker compose down -v && docker compose up --build`.

Поднимает четыре сервиса:

| Сервис | Порт на хосте | Назначение |
|---|---|---|
| `fias-downloader` | `8080` | сам микросервис (API + `/metrics`) |
| `postgres` | `5432` | хранилище `version_info` и `logs` |
| `prometheus` | `9090` | сбор метрик с `fias-downloader:8080/metrics` |
| `grafana` | `3000` | дашборд по метрикам (источник данных и дашборд провижинятся автоматически) |

Grafana доступна на [http://localhost:3000](http://localhost:3000), логин/пароль
по умолчанию — `admin` / `admin` (задаются `GF_SECURITY_ADMIN_USER` /
`GF_SECURITY_ADMIN_PASSWORD` в `docker-compose.yml`, поменяйте перед
эксплуатацией вне локальной машины). Источник данных Prometheus и дашборд
**FIAS Downloader** (загрузки по результату, скорость и длительность
загрузки по типу файла, длительность циклов, последние успешно загруженные
`full`/`delta`-версии и т.д.) подключаются автоматически при первом старте
через `deploy/grafana/provisioning`; вручную ничего настраивать не нужно.

Конфигурация сервиса берётся из `deploy/config.docker.yaml` — он
монтируется в контейнер как `/app/config.yaml` (см. `docker-compose.yml`);
поправьте его под себя перед запуском в проде (например, пароль в
`postgres.dsn`).

БД, метрики Prometheus и настройки/сессии Grafana сохраняются в именованных
docker volume'ах (`pgdata`, `prometheus-data`, `grafana-data`) и переживают
перезапуск контейнеров.
Скачанные архивы справочника, напротив, сохраняются **не в volume, а в
локальный каталог хоста** `./var/data/downloads` (bind mount на
`/app/data/downloads` внутри контейнера) — так к файлам можно обратиться
напрямую из другого ПО на хосте (например, из процесса импорта), не заходя
внутрь контейнера. Каталог создаётся автоматически при первом запуске
(`entrypoint.sh` вызывает `mkdir -p` и выставляет владельца перед стартом
сервиса — см. ниже), в репозитории лежит как пустой с `.gitkeep`.

### О правах на каталог с архивами

Сервис внутри контейнера работает от непривилегированного пользователя
`app` (uid 10001), а `./var/data/downloads` — это обычный каталог хоста, чей
владелец докеру заранее не известен. Чтобы это не приводило к `permission
denied`, контейнер стартует от `root`, `entrypoint.sh` выставляет
`chown -R app:app /app/data`, и только после этого процесс сервиса
запускается от `app` (через `su-exec`). Это стандартный паттерн для
bind-mount'ов с неизвестным UID на хосте и применяется при каждом старте
контейнера — вручную ничего chmod'ить не нужно.

## Запуск без Docker (локально)

```bash
cp config.example.yaml config.yaml
# отредактируйте config.yaml — как минимум postgres.dsn
go run ./cmd/fias-downloader
```

Таблицы `version_info` и `logs` создаются автоматически при старте
(`EnsureSchema`); при желании применяйте `migrations/0001_init.sql` и
`migrations/0002_logs.sql` отдельно через свой инструмент миграций вместо
авто-создания.

## HTTP API

- `GET  /healthz` — liveness-проверка.
- `GET  /metrics` — метрики в формате Prometheus.
- `POST /trigger/sync` — внеплановый цикл (та же логика full/delta, что и по расписанию), асинхронно, ответ `202 Accepted`.
- `POST /trigger/full` — принудительная загрузка последней полной версии, даже если полная версия уже загружена, асинхронно, ответ `202 Accepted`.

## Метрики Prometheus

Все метрики в пространстве имён `fias_`:

| Метрика | Тип | Labels | Описание |
|---|---|---|---|
| `fias_source_fetch_total` | counter | `result` | запросы перечня версий к источнику |
| `fias_source_fetch_duration_seconds` | histogram | — | длительность запроса перечня версий |
| `fias_cycle_total` | counter | `result` | завершённые циклы проверки/загрузки |
| `fias_cycle_duration_seconds` | histogram | — | длительность цикла |
| `fias_download_total` | counter | `kind`, `result` | загрузки файлов версий |
| `fias_download_bytes_total` | counter | `kind` | суммарно загруженных байт |
| `fias_download_duration_seconds` | histogram | `kind` | длительность загрузки файла |
| `fias_download_in_progress` | gauge | `kind` | загрузок выполняется прямо сейчас |
| `fias_last_completed_version_id` | gauge | `kind` | `VersionId` последней успешной загрузки |
| `fias_last_cycle_timestamp_seconds` | gauge | — | unix-время завершения последнего цикла |

Плюс стандартные метрики рантайма Go (`go_*`) и процесса (`process_*`).

## Логи в БД

Логи пишутся одновременно:

- в stdout — структурированный JSON (`docker compose logs -f fias-downloader`, удобно агрегировать во внешнюю систему при необходимости);
- в таблицу `logs` в PostgreSQL — для быстрых запросов и корреляции с
  `version_info` без похода во внешнюю систему логирования.

Запись в БД асинхронная и батчевая (по умолчанию — раз в 2 секунды или по
накоплении 200 записей), с ограниченной по размеру очередью в памяти: при
переполнении новые записи логов отбрасываются, а не блокируют работу
сервиса. Пример запроса последних ошибок:

```sql
SELECT ts, message, attrs
FROM logs
WHERE level = 'ERROR'
ORDER BY ts DESC
LIMIT 50;
```

## Тесты

```bash
go test ./...
```

Покрыты: ключевые сценарии докачки (резюме через `Range` после обрыва,
фолбэк на полную перезагрузку при отсутствии поддержки `Range`), фан-аут
логов на несколько обработчиков (`TeeHandler`) и загрузка/валидация YAML-конфигурации
(значения по умолчанию, переопределение из файла, ошибки при отсутствующем
`postgres.dsn`, некорректной длительности и отсутствующем файле).

## Открытые вопросы (перенесены из ТЗ, не решены в коде)

- Поведение после принудительной загрузки новой полной версии: код сохраняет
  её как ещё одну запись `kind='full'`, но не удаляет и не помечает
  неактуальными предыдущую полную версию и уже накопленные дельты — это
  решение уровня бизнес-логики импорта, вне рамок данного сервиса.
- Распределённая блокировка для запуска в нескольких экземплярах не
  реализована (`sync.Mutex` в `Scheduler` защищает только от параллельного
  запуска в рамках одного процесса).
- Хранение файлов — локальная файловая система (при Docker-запуске — bind
  mount `./var/data/downloads` на хосте); вынесение в объектное хранилище
  не реализовано.
- Ротация/очистка старых записей в таблице `logs` не реализована — при
  долгой эксплуатации таблицу стоит периодически партиционировать или
  чистить по возрасту.

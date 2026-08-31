# fias-downloader

Go-сервис для автоматической загрузки полных и дельта-архивов адресного
справочника ФИАС/ГАР. Список доступных версий сервис получает через
`GetAllDownloadFileInfo`, состояние хранит в PostgreSQL, а архивы — в локальной
файловой системе.

## Как работает сервис

При запуске сервис сразу выполняет цикл синхронизации, затем повторяет его с
интервалом `scheduler.poll_interval`:

1. Получает список версий ФИАС/ГАР.
2. Если завершённой полной версии ещё нет, загружает последнюю доступную full.
3. Если full уже есть, последовательно загружает отсутствующие delta после неё.
4. Сохраняет статус, объём, путь, SHA-256 и ошибки в `version_info`.

Плановый и ручной циклы защищены общим process-local mutex: одновременно внутри
одного экземпляра выполняется не более одного цикла. Для нескольких реплик
распределённой блокировки нет.

Загрузчик реализован на стандартном `net/http`:

- данные пишутся в `<имя>.zip.part`;
- незавершённый файл продолжается запросом `Range`;
- `206 Partial Content` принимается только с корректным `Content-Range`;
- если сервер отвечает `200 OK` на Range-запрос, `.part` очищается и загрузка
  начинается заново;
- ответ `416` считается успешным только когда размер `.part` совпадает с полным
  размером из `Content-Range`;
- отсутствие новых данных дольше `download.stall_timeout` прерывает попытку;
- временные ошибки повторяются с экспоненциальной задержкой;
- готовый файл синхронизируется, получает SHA-256 и атомарно переименовывается
  из `.part` в итоговый `.zip`.

Прогресс сохраняется в PostgreSQL с интервалом
`download.progress_save_interval`. В интерактивном терминале одна строка
прогресса обновляется каждые 5 секунд; в Docker stdout не является терминалом,
поэтому каждое обновление выводится отдельной строкой и видно через
`docker compose logs`.

## Архитектура

```text
FIAS/GAR API
    |
    v
fetcher -> scheduler -> downloader -> .zip.part -> .zip
                |             |
                v             v
           PostgreSQL     текущие метрики
          version_info     Prometheus
              logs             |
                               v
                            Grafana
```

Основные каталоги:

```text
cmd/fias-downloader/          точка входа и сборка зависимостей
internal/api/                 health, metrics и ручные trigger-эндпоинты
internal/config/              загрузка и валидация YAML
internal/downloader/          HTTP-загрузка, resume, retry, stall detection, SHA-256
internal/fetcher/             клиент GetAllDownloadFileInfo
internal/logstore/            асинхронная запись slog в PostgreSQL
internal/metrics/             метрики текущей загрузки
internal/model/               модели источника и БД
internal/repo/                PostgreSQL-репозиторий version_info
internal/scheduler/           выбор full/delta и оркестрация
migrations/                   SQL для version_info и logs
deploy/config.docker.yaml     конфигурация сервиса в Compose
deploy/prometheus/            конфигурация сбора метрик
deploy/grafana/               provisioning и dashboard текущей загрузки
var/data/downloads/           архивы при запуске через Compose
```

## Конфигурация

Сервис читает только YAML. По умолчанию требуется `config.yaml` в текущей
директории. Другой путь задаётся флагом `--config` или переменной
`FIAS_CONFIG_PATH`; это единственная поддерживаемая переменная окружения.

```yaml
postgres:
  dsn: "postgres://fias:fias@localhost:5432/fias?sslmode=disable"

source:
  url: "https://fias.nalog.ru/WebServices/Public/GetAllDownloadFileInfo"
  http_timeout: "30s"

download:
  dir: "./data/downloads"
  stall_timeout: "10m"
  max_retries: 15
  retry_base_delay: "5s"
  progress_save_interval: "30s"

http:
  listen_addr: ":8080"

scheduler:
  poll_interval: "6h"
```

Неуказанные поля получают значения по умолчанию. Обязательны непустые
`postgres.dsn`, `source.url`, `download.dir`, `http.listen_addr` и положительный
`scheduler.poll_interval`. Длительности задаются строками Go duration: `30s`,
`10m`, `6h`.

Для локального запуска скопируйте пример:

```bash
cp config.example.yaml config.yaml
# отредактируйте как минимум postgres.dsn
go run ./cmd/fias-downloader
```

`config.yaml` содержит секреты и исключён из Git.

## Сборка и Makefile

```bash
make help            # список целей
make binary          # бинарник ./bin/fias-downloader
make docker-images   # образ fias-downloader-fias-downloader
make build           # бинарник и Docker-образ
make test            # go test ./...
make vet             # go vet ./...
make fmt              # go fmt ./...
```

Docker-образ собирается с vendored-зависимостями и `CGO_ENABLED=0`. Во время
работы контейнер использует непривилегированного пользователя `app` с uid 10001.

## Docker Compose

Compose ожидает локальный образ `fias-downloader-fias-downloader`, поэтому его
нужно собрать до первого запуска:

```bash
make docker-images
docker compose up -d
```

Либо:

```bash
make build
make compose-up
```

Сервисы и адреса на хосте:

| Сервис | Адрес | Назначение |
|---|---|---|
| `fias-downloader` | `http://localhost:8888` | HTTP API и `/metrics` |
| `postgres` | `localhost:5431` | `version_info` и `logs` |
| `prometheus` | `http://localhost:9999` | хранение и запрос метрик |
| `grafana` | `http://localhost:3000` | dashboard текущей загрузки |

Compose монтирует `deploy/config.docker.yaml` как `/app/config.yaml` и
`./var/data/downloads` как `/app/data/downloads`. `entrypoint.sh` создаёт каталог,
назначает его пользователю `app`, после чего запускает сервис через `su-exec`.
Вручную менять права каталога не требуется.

PostgreSQL, Prometheus и Grafana используют именованные volumes `pgdata`,
`prometheus-data` и `grafana-data`. Архивы хранятся отдельно в bind mount
`./var/data/downloads` и не удаляются командой `docker compose down -v`.

Grafana использует автоматически provisioned Prometheus datasource и dashboard
`FIAS Downloader — текущая загрузка`. Локальные учётные данные по умолчанию:
`admin` / `admin`; перед внешней эксплуатацией их необходимо изменить.

Полезные команды:

```bash
docker compose ps
docker compose logs -f fias-downloader
docker compose down
```

После пересборки образа с тем же тегом контейнер можно пересоздать так:

```bash
make docker-images
docker compose up -d --force-recreate fias-downloader
```

## HTTP API

| Метод и путь | Ответ | Назначение |
|---|---|---|
| `GET /healthz` | `200 ok` | liveness-проверка |
| `GET /metrics` | `200` | метрики Prometheus |
| `POST /trigger/sync` | `202 {"status":"started"}` | асинхронный обычный цикл full/delta |
| `POST /trigger/full` | `202 {"status":"started"}` | асинхронная принудительная latest-full загрузка |

Trigger-запрос возвращается сразу. Если другой цикл уже выполняется, новый цикл
будет пропущен, а причина попадёт в лог.

Примеры:

```bash
curl http://localhost:8888/healthz
curl -X POST http://localhost:8888/trigger/sync
curl -X POST http://localhost:8888/trigger/full
```

## Метрики и Grafana

Экспортируются только метрики текущей загрузки:

| Метрика | Labels | Описание |
|---|---|---|
| `fias_download_in_progress` | `kind` | количество активных загрузок |
| `fias_download_progress_downloaded_bytes` | `kind`, `version_id` | получено байт |
| `fias_download_progress_total_bytes` | `kind`, `version_id` | полный размер или `0`, если неизвестен |

Метрики с `version_id` создаются при старте загрузки и удаляются после успеха
или ошибки. Стандартные collectors `go_*` и `process_*` не регистрируются.
Dashboard показывает активность, процент, загруженный и полный объём, а также
прогресс текущей загрузки во времени. Prometheus опрашивает сервис каждые 15
секунд, dashboard обновляется каждые 5 секунд.

![Dashboard текущей загрузки FIAS Downloader](img/dashboard.jpg)

## PostgreSQL и логи

При старте сервис автоматически создаёт таблицы `version_info` и `logs`.
SQL-версии схем также находятся в `migrations/0001_init.sql` и
`migrations/0002_logs.sql`.

`version_info` содержит отдельную строку для каждой пары `(version_id, kind)` и
статусы `pending`, `downloading`, `completed`, `failed`. Для успешной загрузки
сохраняются итоговый размер, путь и SHA-256; для неуспешной — текст последней
ошибки. Частичный файл остаётся на диске для следующей попытки.

Структурированные логи уровня INFO и выше одновременно пишутся:

- в stdout как JSON;
- асинхронно в таблицу `logs` пакетами до 200 записей или раз в 2 секунды.

Очередь логов содержит до 2000 записей. При переполнении новые записи
отбрасываются, чтобы медленная БД не блокировала загрузку. При штатном завершении
очередь сбрасывается с таймаутом 5 секунд.

Пример запроса:

```sql
SELECT ts, level, message, attrs
FROM logs
WHERE level = 'ERROR'
ORDER BY ts DESC
LIMIT 50;
```

## Диагностика загрузки

Проверка поддержки resume:

```bash
curl -v -L -H 'Range: bytes=1048576-' \
  -o /dev/null 'https://example.invalid/path/archive.zip'
```

| Ответ | Поведение сервиса |
|---|---|
| `206` с ожидаемым `Content-Range` | продолжает `.part` с текущего смещения |
| `200` на Range-запрос | очищает `.part` и начинает заново |
| `416`, полный размер равен размеру `.part` | завершает файл без повторной загрузки |
| другой `416` | завершает попытку ошибкой и повторяет согласно retry policy |
| другой HTTP-статус или сетевая ошибка | сохраняет ошибку и повторяет попытку |

Если `.part` заведомо относится к другому содержимому, остановите сервис,
удалите соответствующий файл из `download.dir` и запустите сервис снова. Не
удаляйте файл во время активной записи.

Типовые сообщения:

- `stalled: no data for ...` — поток не передавал данные дольше stall timeout;
- `source ignored Range request` — сервер не поддержал resume;
- `download interrupted: received ...` — фактический размер не совпал с
  ожидаемым;
- `download failed after ... attempts without progress` — исчерпан retry budget
  последовательных попыток без роста `.part`.

## Тесты

```bash
go test ./...
go vet ./...
```

Тесты покрывают YAML-конфигурацию, resume через `206`, fallback при игнорировании
Range, сброс retry budget после прогресса, консольный прогресс, состав метрик
текущей загрузки и fan-out логов.

## Ограничения

- Нет распределённой блокировки между несколькими экземплярами сервиса.
- Архивы хранятся только в локальной файловой системе.
- Автоматической очистки старых архивов и `.part` нет.
- Для таблицы `logs` нет retention/rotation.
- Принудительная full-загрузка не удаляет предыдущие full и delta.

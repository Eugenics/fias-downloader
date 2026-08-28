// Package repo содержит слой доступа к PostgreSQL — хранение состояния
// загрузок версий справочника (таблица version_info).
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"

	"fias-downloader/internal/model"
)

var ErrNotFound = errors.New("record not found")

type Repo struct {
	db *sql.DB
}

func Open(dsn string) (*Repo, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Repo{db: db}, nil
}

func (r *Repo) Close() error {
	return r.db.Close()
}

// DB возвращает нижележащий пул соединений — используется для переиспользования
// того же соединения другими компонентами (например, logstore).
func (r *Repo) DB() *sql.DB {
	return r.db
}

func (r *Repo) Ping(ctx context.Context) error {
	return r.db.PingContext(ctx)
}

// EnsureSchema создаёт таблицу version_info, если она ещё не существует.
// Соответствует таблице "versionInfo" из ТЗ — имя приведено к идиоматичному
// для PostgreSQL snake_case; при необходимости используйте
// migrations/0001_init.sql отдельно (например, через golang-migrate) вместо
// авто-миграции при старте.
func (r *Repo) EnsureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS version_info (
    id               BIGSERIAL PRIMARY KEY,
    version_id       BIGINT      NOT NULL,
    version_date     DATE        NOT NULL,
    text_version     TEXT        NOT NULL DEFAULT '',
    kind             TEXT        NOT NULL CHECK (kind IN ('full', 'delta')),
    source_url       TEXT        NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'pending'
                                  CHECK (status IN ('pending', 'downloading', 'completed', 'failed')),
    file_path        TEXT        NOT NULL DEFAULT '',
    total_bytes      BIGINT      NOT NULL DEFAULT 0,
    downloaded_bytes BIGINT      NOT NULL DEFAULT 0,
    checksum         TEXT        NOT NULL DEFAULT '',
    is_manual        BOOLEAN     NOT NULL DEFAULT FALSE,
    last_error       TEXT        NOT NULL DEFAULT '',
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (version_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_version_info_kind_status ON version_info (kind, status);
`
	_, err := r.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	return nil
}

// GetLatestCompleted возвращает запись с максимальным version_id среди
// успешно завершённых загрузок указанного вида (full/delta), либо
// ErrNotFound, если таких ещё нет.
func (r *Repo) GetLatestCompleted(ctx context.Context, kind model.Kind) (*model.DownloadRecord, error) {
	const q = `
SELECT id, version_id, version_date, text_version, kind, source_url, status,
       file_path, total_bytes, downloaded_bytes, checksum, is_manual,
       last_error, started_at, completed_at
FROM version_info
WHERE kind = $1 AND status = $2
ORDER BY version_id DESC
LIMIT 1`
	row := r.db.QueryRowContext(ctx, q, kind, model.StatusCompleted)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get latest completed: %w", err)
	}
	return rec, nil
}

// GetByVersionKind возвращает существующую запись для пары (versionID, kind),
// если она есть.
func (r *Repo) GetByVersionKind(ctx context.Context, versionID int64, kind model.Kind) (*model.DownloadRecord, error) {
	const q = `
SELECT id, version_id, version_date, text_version, kind, source_url, status,
       file_path, total_bytes, downloaded_bytes, checksum, is_manual,
       last_error, started_at, completed_at
FROM version_info
WHERE version_id = $1 AND kind = $2`
	row := r.db.QueryRowContext(ctx, q, versionID, kind)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get by version/kind: %w", err)
	}
	return rec, nil
}

// GetOrCreatePending возвращает существующую запись для пары (versionID, kind)
// либо создаёт новую в статусе pending. Используется как точка входа перед
// стартом загрузки — гарантирует отсутствие дублей благодаря уникальному
// индексу (version_id, kind).
func (r *Repo) GetOrCreatePending(ctx context.Context, sv model.SourceVersion, kind model.Kind, isManual bool) (*model.DownloadRecord, error) {
	existing, err := r.GetByVersionKind(ctx, sv.VersionID, kind)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	versionDate, dateErr := sv.ParsedDate()
	if dateErr != nil {
		versionDate = time.Now().UTC()
	}

	const q = `
INSERT INTO version_info (version_id, version_date, text_version, kind, source_url, status, is_manual)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (version_id, kind) DO UPDATE SET updated_at = now()
RETURNING id, version_id, version_date, text_version, kind, source_url, status,
          file_path, total_bytes, downloaded_bytes, checksum, is_manual,
          last_error, started_at, completed_at`
	row := r.db.QueryRowContext(ctx, q,
		sv.VersionID, versionDate, sv.TextVersion, kind, sv.URLFor(kind), model.StatusPending, isManual)
	rec, err := scanRecord(row)
	if err != nil {
		return nil, fmt.Errorf("create pending record: %w", err)
	}
	return rec, nil
}

// MarkDownloading помечает запись как "в процессе загрузки" и фиксирует
// путь к файлу и время старта (если ещё не зафиксировано).
func (r *Repo) MarkDownloading(ctx context.Context, id int64, filePath string) error {
	const q = `
UPDATE version_info
SET status = $2,
    file_path = $3,
    started_at = COALESCE(started_at, now()),
    last_error = '',
    updated_at = now()
WHERE id = $1`
	_, err := r.db.ExecContext(ctx, q, id, model.StatusDownloading, filePath)
	return err
}

// UpdateProgress обновляет счётчики загруженных/ожидаемых байт — вызывается
// периодически в процессе загрузки, чтобы при рестарте сервиса можно было
// корректно продолжить докачку.
func (r *Repo) UpdateProgress(ctx context.Context, id int64, downloaded, total int64) error {
	const q = `
UPDATE version_info
SET downloaded_bytes = $2, total_bytes = $3, updated_at = now()
WHERE id = $1`
	_, err := r.db.ExecContext(ctx, q, id, downloaded, total)
	return err
}

// MarkCompleted помечает загрузку завершённой успешно.
func (r *Repo) MarkCompleted(ctx context.Context, id int64, totalBytes int64, checksum string) error {
	const q = `
UPDATE version_info
SET status = $2, total_bytes = $3, downloaded_bytes = $3, checksum = $4,
    completed_at = now(), last_error = '', updated_at = now()
WHERE id = $1`
	_, err := r.db.ExecContext(ctx, q, id, model.StatusCompleted, totalBytes, checksum)
	return err
}

// MarkFailed помечает загрузку неуспешной, сохраняя текст ошибки для диагностики.
// Файл на диске не удаляется — он используется для последующей докачки.
func (r *Repo) MarkFailed(ctx context.Context, id int64, downloadErr error) error {
	const q = `
UPDATE version_info
SET status = $2, last_error = $3, updated_at = now()
WHERE id = $1`
	msg := ""
	if downloadErr != nil {
		msg = downloadErr.Error()
	}
	_, err := r.db.ExecContext(ctx, q, id, model.StatusFailed, msg)
	return err
}

func scanRecord(row *sql.Row) (*model.DownloadRecord, error) {
	var rec model.DownloadRecord
	var startedAt, completedAt sql.NullTime
	err := row.Scan(
		&rec.ID, &rec.VersionID, &rec.VersionDate, &rec.TextVersion, &rec.Kind,
		&rec.SourceURL, &rec.Status, &rec.FilePath, &rec.TotalBytes, &rec.DownloadedBytes,
		&rec.Checksum, &rec.IsManual, &rec.LastError, &startedAt, &completedAt,
	)
	if err != nil {
		return nil, err
	}
	if startedAt.Valid {
		rec.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		rec.CompletedAt = &completedAt.Time
	}
	return &rec, nil
}

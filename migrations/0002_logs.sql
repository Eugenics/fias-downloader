-- Таблица логов сервиса. Заполняется асинхронным DB-обработчиком slog
-- (internal/logstore) в дополнение к обычному JSON-выводу в stdout.

CREATE TABLE IF NOT EXISTS logs (
    id         BIGSERIAL PRIMARY KEY,
    ts         TIMESTAMPTZ NOT NULL,
    level      TEXT        NOT NULL,
    message    TEXT        NOT NULL,
    attrs      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs (ts);
CREATE INDEX IF NOT EXISTS idx_logs_level ON logs (level);

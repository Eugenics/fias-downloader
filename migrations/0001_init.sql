-- Таблица версий/загрузок справочника ФИАС/ГАР.
-- Соответствует таблице "versionInfo" из ТЗ (имя приведено к snake_case,
-- идиоматичному для PostgreSQL). Одна строка = одна пара (версия, вид файла),
-- т.к. у одной версии из перечня источника есть два самостоятельных файла:
-- полный снимок (full) и дельта (delta).

CREATE TABLE IF NOT EXISTS version_info (
    id               BIGSERIAL PRIMARY KEY,
    version_id       BIGINT      NOT NULL,               -- VersionId из источника (YYYYMMDD)
    version_date     DATE        NOT NULL,                -- Date из источника
    text_version     TEXT        NOT NULL DEFAULT '',      -- TextVersion из источника
    kind             TEXT        NOT NULL
                                  CHECK (kind IN ('full', 'delta')),
    source_url       TEXT        NOT NULL,                 -- GarXMLFullURL либо GarXMLDeltaURL
    status           TEXT        NOT NULL DEFAULT 'pending'
                                  CHECK (status IN ('pending', 'downloading', 'completed', 'failed')),
    file_path        TEXT        NOT NULL DEFAULT '',
    total_bytes      BIGINT      NOT NULL DEFAULT 0,
    downloaded_bytes BIGINT      NOT NULL DEFAULT 0,
    checksum         TEXT        NOT NULL DEFAULT '',       -- sha256 файла после завершения загрузки
    is_manual        BOOLEAN     NOT NULL DEFAULT FALSE,     -- инициировано вручную (принудительный full)
    last_error       TEXT        NOT NULL DEFAULT '',
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (version_id, kind)
);

CREATE INDEX IF NOT EXISTS idx_version_info_kind_status ON version_info (kind, status);

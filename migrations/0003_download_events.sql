CREATE TABLE IF NOT EXISTS download_events (
    id BIGSERIAL PRIMARY KEY,
    version_info_id BIGINT NOT NULL REFERENCES version_info(id),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_download_events_pending ON download_events(id) WHERE published_at IS NULL;

-- Cache of fetched avatars, keyed by lowercased email.
-- Stores raw bytes + MIME so the same row covers PNG/JPEG/SVG/etc.
-- Negative caching: rows with NULL data mean "tried, found nothing" — keeps
-- a shorter TTL so dead addresses get rechecked sooner.

CREATE TABLE IF NOT EXISTS avatar_cache (
    email      TEXT    PRIMARY KEY,
    source     TEXT    NOT NULL,           -- 'carddav' | 'libravatar' | 'gravatar' | 'bimi' | 'favicon' | 'none'
    data       BYTEA,                       -- NULL for negative cache
    mime       TEXT,                        -- 'image/png' | 'image/svg+xml' | ...
    fetched_at BIGINT  NOT NULL,            -- ms since epoch
    ttl_ms     BIGINT  NOT NULL             -- expires_at = fetched_at + ttl_ms
);

CREATE INDEX IF NOT EXISTS idx_avatar_cache_fetched_at ON avatar_cache(fetched_at);

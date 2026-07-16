-- +goose Up
-- Cursor persistence for Jetstream consumers.
-- Each consumer stores the time_us of the last event it fully processed
-- (indexed or dead-lettered). On reconnect the consumer resumes from this
-- cursor instead of the live tail, so restarts/deploys/crashes no longer
-- lose the events that occurred during the gap.
CREATE TABLE jetstream_cursors (
    consumer_name TEXT PRIMARY KEY,
    cursor_time_us BIGINT NOT NULL CHECK (cursor_time_us >= 0),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Dead letter queue for Jetstream events that failed all in-line retries.
-- The raw event is stored as BYTEA, not TEXT/JSONB: byte-corrupt frames
-- (NUL bytes, invalid UTF-8) must also be capturable. A TEXT column would
-- reject them, the failed dead-letter write would tear down the connection
-- without advancing the cursor, and the consumer would replay the same
-- frame forever. Storing the raw bytes lets a background redriver replay
-- the event against the same consumer once the underlying failure (e.g. a
-- transient Postgres error) has cleared.
CREATE TABLE jetstream_dead_letters (
    id BIGSERIAL PRIMARY KEY,
    consumer_name TEXT NOT NULL,
    event_time_us BIGINT NOT NULL DEFAULT 0,
    event_data BYTEA NOT NULL,
    last_error TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,  -- redrive attempts, not in-line retries
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Redriver scans per consumer, oldest first, skipping rows that exhausted
-- their redrive budget.
CREATE INDEX idx_jetstream_dead_letters_redrive
    ON jetstream_dead_letters (consumer_name, attempts, id);

-- Dedup: a poison event inside the reconnect rewind window (or an
-- unparseable frame, which never advances the cursor) would otherwise
-- insert a fresh row — each with its own redrive budget — on every
-- reconnect. AddDeadLetter inserts with ON CONFLICT DO NOTHING: an
-- already-captured event counts as success, so the cursor still advances.
CREATE UNIQUE INDEX idx_jetstream_dead_letters_dedup
    ON jetstream_dead_letters (consumer_name, event_time_us, md5(event_data));

-- +goose Down
DROP INDEX IF EXISTS idx_jetstream_dead_letters_dedup;
DROP INDEX IF EXISTS idx_jetstream_dead_letters_redrive;
DROP TABLE IF EXISTS jetstream_dead_letters;
DROP TABLE IF EXISTS jetstream_cursors;

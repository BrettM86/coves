-- +goose Up
-- One row per change of a community's "nsfw" content warning: the first index
-- of a community whose record carries it (old_nsfw false), and every update
-- that adds or removes it. The community repository writes the row in the
-- same transaction as the community row, so a failed write leaves no event.
-- origin_host is the bridged community's origin host, NULL for a native one.
--
-- communitysignals.Service sends each event to the operators after commit
-- and its sweep retries failed sends: attempts counts failed sends,
-- next_attempt_at is when the event is next due (creation time for a new
-- one), and an event at the configured attempt limit stays undelivered.
CREATE TABLE community_nsfw_flag_events (
    id BIGSERIAL PRIMARY KEY,
    community_did TEXT NOT NULL REFERENCES communities(did) ON DELETE CASCADE,
    origin_host TEXT,
    old_nsfw BOOLEAN NOT NULL,
    new_nsfw BOOLEAN NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error_code TEXT CHECK (last_error_code IN ('send_failed')),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (old_nsfw <> new_nsfw)
);

CREATE INDEX community_nsfw_flag_events_due_idx
    ON community_nsfw_flag_events (next_attempt_at, id)
    WHERE delivered_at IS NULL;

CREATE INDEX community_nsfw_flag_events_community_idx
    ON community_nsfw_flag_events (community_did);

-- The slot (a Monday 09:00 UTC) of the last weekly NSFW share digest run, so
-- a restart in the same week does not send it again. At most one row.
CREATE TABLE community_signal_digest_marker (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    last_slot TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE community_signal_digest_marker;
DROP TABLE community_nsfw_flag_events;

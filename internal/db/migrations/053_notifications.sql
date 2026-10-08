-- +goose Up
CREATE TABLE notification_activation (
    singleton    BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    activated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO notification_activation (activated_at) VALUES (NOW());

CREATE TABLE notifications (
    id            BIGSERIAL PRIMARY KEY,
    recipient_did TEXT NOT NULL REFERENCES users(did) ON DELETE CASCADE,
    reason        TEXT NOT NULL CHECK (reason IN ('postReply','commentReply','mention','upvote')),
    record_uri    TEXT,          -- the reply or mentioning record; NULL for upvote groups
    record_cid    TEXT,
    actor_did     TEXT,          -- NULL for upvote groups
    subject_uri   TEXT,          -- recipient's own post/comment; NULL for mention
    root_post_uri TEXT NOT NULL, -- navigation target for every reason
    record_created_at TIMESTAMPTZ, -- display only ("2h ago"); NULL for upvote groups
    sort_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- index time; bumped by new upvotes
    CHECK (
        (reason = 'upvote' AND record_uri IS NULL AND record_cid IS NULL AND actor_did IS NULL
            AND subject_uri IS NOT NULL)
     OR (reason IN ('postReply','commentReply') AND record_uri IS NOT NULL AND record_cid IS NOT NULL
            AND actor_did IS NOT NULL AND subject_uri IS NOT NULL)
     OR (reason = 'mention' AND record_uri IS NOT NULL AND record_cid IS NOT NULL
            AND actor_did IS NOT NULL AND subject_uri IS NULL)
    )
);

CREATE UNIQUE INDEX uq_notifications_record
    ON notifications (recipient_did, reason, record_uri) WHERE reason <> 'upvote';
CREATE UNIQUE INDEX uq_notifications_upvote_group
    ON notifications (recipient_did, subject_uri) WHERE reason = 'upvote';
CREATE INDEX idx_notifications_recipient_sort
    ON notifications (recipient_did, sort_at DESC, id DESC);
CREATE INDEX idx_notifications_record ON notifications (record_uri) WHERE record_uri IS NOT NULL;
CREATE INDEX idx_notifications_actor ON notifications (actor_did) WHERE actor_did IS NOT NULL;

CREATE TABLE notification_state (
    did              TEXT PRIMARY KEY REFERENCES users(did) ON DELETE CASCADE,
    seen_at          TIMESTAMPTZ,              -- NULL: never marked seen
    disabled_reasons TEXT[] NOT NULL DEFAULT '{}'
);

ALTER TABLE posts ADD COLUMN bridged_upvote_peak INT NOT NULL DEFAULT 0
    CONSTRAINT posts_bridged_upvote_peak_nonnegative CHECK (bridged_upvote_peak >= 0);
ALTER TABLE comments ADD COLUMN bridged_upvote_peak INT NOT NULL DEFAULT 0
    CONSTRAINT comments_bridged_upvote_peak_nonnegative CHECK (bridged_upvote_peak >= 0);
UPDATE posts SET bridged_upvote_peak = bridged_upvote_count WHERE bridged_upvote_count > 0;
UPDATE comments SET bridged_upvote_peak = bridged_upvote_count WHERE bridged_upvote_count > 0;
COMMENT ON COLUMN posts.bridged_upvote_peak IS 'Highest bridged upvote total the poller has applied to this item, including the pre-update stored count it replaced, or the stored total at launch; upvote notifications bump only above GREATEST(bridged_upvote_peak, bridged_upvote_count).';
COMMENT ON COLUMN comments.bridged_upvote_peak IS 'Highest bridged upvote total the poller has applied to this item, including the pre-update stored count it replaced, or the stored total at launch; upvote notifications bump only above GREATEST(bridged_upvote_peak, bridged_upvote_count).';

-- +goose Down
ALTER TABLE comments DROP COLUMN IF EXISTS bridged_upvote_peak;
ALTER TABLE posts DROP COLUMN IF EXISTS bridged_upvote_peak;
DROP TABLE IF EXISTS notification_state;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS notification_activation;

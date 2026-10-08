-- +goose Up
CREATE TABLE notification_public_post_withdrawals (
    post_uri TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('authorDelete', 'communityWithdrawal')),
    community_rev TEXT,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (post_uri, kind),
    CHECK ((kind = 'authorDelete') = (community_rev IS NULL))
);

-- Account erasure deletes a user's markers by the post URI's authority; the
-- expression must match user_repo.Delete exactly for the index to serve it.
CREATE INDEX idx_notification_public_post_withdrawals_author
    ON notification_public_post_withdrawals ((split_part(post_uri, '/', 3)));

-- +goose Down
DROP TABLE notification_public_post_withdrawals;

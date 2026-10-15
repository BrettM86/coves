-- +goose Up
-- The text of a removed post or comment as indexed when it was removed, kept
-- for the modlog while its action row exists. Illegal-content removals keep
-- none. author_did is the account that wrote the record (posts.author_did or
-- comments.commenter_did), not the repository owner, so account erasure can
-- delete an author's snapshots, including those of legacy posts held in a
-- community's repository. author_deleted_at is set when the author deletes a
-- removed comment, so the public log withholds its snapshot even after a new
-- record re-creates the comment's URI.
CREATE TABLE moderation_content_snapshots (
    action_id TEXT PRIMARY KEY REFERENCES moderation_actions(id) ON DELETE CASCADE,
    author_did TEXT NOT NULL,
    title TEXT,
    text TEXT,
    link_url TEXT,
    captured_cid TEXT NOT NULL,
    author_deleted_at TIMESTAMPTZ
);

CREATE INDEX moderation_content_snapshots_author ON moderation_content_snapshots (author_did);

-- The post at the root of a comment subject's thread, recorded when the action
-- was taken. NULL for post subjects.
ALTER TABLE moderation_actions ADD COLUMN subject_root_uri TEXT;

-- +goose Down
ALTER TABLE moderation_actions DROP COLUMN subject_root_uri;
DROP TABLE moderation_content_snapshots;

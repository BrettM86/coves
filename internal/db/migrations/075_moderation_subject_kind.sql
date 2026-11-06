-- +goose Up
-- subject_kind records whether an at://<did> action (no collection) is about
-- an account (banAccount, unbanAccount) or a community (removeCommunity,
-- restoreCommunity). Readers used to infer it: an account ban was a decision
-- on a DID with no communities row, a community removal one with no expiry.
-- The two rules disagreed once an account indexed itself as a community.
ALTER TABLE moderation_actions ADD COLUMN subject_kind TEXT;

-- Backfill with the rules readers applied before this migration. A removal
-- with an expiry is an account ban: RemoveCommunity never sets one, so a timed
-- decision on an indexed community DID, which read as neither before, is an
-- account ban. A permanent removal is a community removal when the DID has a
-- communities row, and otherwise an account ban; a removed community whose row
-- was deleted already read as an account ban, and stays one.
UPDATE moderation_actions a
SET subject_kind = CASE
    WHEN a.expires_at IS NOT NULL THEN 'account'
    WHEN EXISTS (SELECT 1 FROM communities c WHERE c.did = substr(a.subject_uri, 6)) THEN 'community'
    ELSE 'account'
END
WHERE a.subject_collection IS NULL AND a.action <> 'restore';

-- A restore has the kind of the action it reverses.
UPDATE moderation_actions a
SET subject_kind = COALESCE(
    (SELECT reversed.subject_kind FROM moderation_actions reversed WHERE reversed.id = a.reverses_action_id),
    'account')
WHERE a.subject_collection IS NULL AND a.action = 'restore';

ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_subject_kind CHECK (
    subject_kind IN ('account', 'community')
);

ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_subject_kind_shape CHECK (
    (subject_collection IS NULL) = (subject_kind IS NOT NULL)
);

-- RemoveCommunity never sets an expiry, and the backfill above reads a timed
-- decision as an account ban, so a community-kind action is always permanent.
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_community_kind_permanent CHECK (
    subject_kind IS DISTINCT FROM 'community' OR expires_at IS NULL
);

-- index_source records why a users row exists. 'account': the DID was indexed
-- as an account (firehose identity or profile, signup, login).
-- 'author_hydration': the DID was indexed only as the author of a record, which
-- is not account activity by itself (the authored record is). Existing rows
-- are backfilled as 'account': the conservative reading, since their source
-- was not recorded. The exception is a row whose DID has a communities row:
-- post-author hydration wrote users rows for community DIDs before this column
-- existed, so those are backfilled as 'author_hydration'.
ALTER TABLE users ADD COLUMN index_source TEXT NOT NULL DEFAULT 'account'
    CONSTRAINT users_index_source CHECK (index_source IN ('account', 'author_hydration'));
UPDATE users SET index_source = 'author_hydration'
WHERE EXISTS (SELECT 1 FROM communities WHERE communities.did = users.did);

-- +goose Down
-- Fails while any at://<did> action's recorded kind differs from the backfill
-- rule recomputed now, while any account action (timed or not) is on a DID
-- that has a communities row, or while a restore's kind differs from the
-- action it reverses: the 074 schema cannot hold that kind. Dropping it would
-- turn an account ban into a community removal (or the reverse), or, for a
-- timed account ban on a community DID, into a decision the 074 readers take
-- as neither, so the ban would stop being enforced.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM moderation_actions a
        WHERE a.subject_collection IS NULL AND a.action <> 'restore'
          AND a.subject_kind <> CASE
              WHEN a.expires_at IS NOT NULL THEN 'account'
              WHEN EXISTS (SELECT 1 FROM communities c WHERE c.did = substr(a.subject_uri, 6)) THEN 'community'
              ELSE 'account'
          END
    ) OR EXISTS (
        SELECT 1 FROM moderation_actions a
        WHERE a.subject_collection IS NULL AND a.subject_kind = 'account'
          AND EXISTS (SELECT 1 FROM communities c WHERE c.did = substr(a.subject_uri, 6))
    ) OR EXISTS (
        SELECT 1 FROM moderation_actions a
        JOIN moderation_actions reversed ON reversed.id = a.reverses_action_id
        WHERE a.subject_collection IS NULL AND a.action = 'restore'
          AND a.subject_kind IS DISTINCT FROM reversed.subject_kind
    ) THEN
        RAISE EXCEPTION 'moderation_actions holds an at://<did> subject kind the 074 schema cannot read back';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE users DROP COLUMN index_source;
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_community_kind_permanent;
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_subject_kind_shape;
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_subject_kind;
ALTER TABLE moderation_actions DROP COLUMN subject_kind;

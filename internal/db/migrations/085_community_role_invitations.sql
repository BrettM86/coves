-- +goose Up
-- A community role invitation is AppView state: an owner's or an instance
-- admin's invitation of an account to moderate a hosted community, or an
-- offer of its ownership. Accepting one writes the role records to the
-- community repo; inviting, declining and superseding write nothing there.
-- Lapsing (at or after expires_at) is computed at read time and never stored.
-- There is no foreign key: the community delete removes its rows.
CREATE TABLE community_role_invitations (
    id TEXT PRIMARY KEY,
    community_did TEXT NOT NULL,
    invitee_did TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('moderator', 'owner')),
    inviter_did TEXT NOT NULL CHECK (inviter_did <> ''),
    inviter_admin BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open', 'accepted', 'declined', 'superseded')),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > created_at)
);

-- At most one open invitation per (community, invitee): a newer one
-- supersedes it in the same transaction.
CREATE UNIQUE INDEX community_role_invitations_open_invitee
    ON community_role_invitations (community_did, invitee_did) WHERE state = 'open';
-- The invitee's inbox.
CREATE INDEX community_role_invitations_open_by_invitee
    ON community_role_invitations (invitee_did, created_at DESC) WHERE state = 'open';
-- The community's list.
CREATE INDEX community_role_invitations_open_by_community
    ON community_role_invitations (community_did, created_at DESC) WHERE state = 'open';

-- An accept's intent: the invitation it settles; the audit actor (the
-- inviter), whose name the action carries while actor_did stays the
-- requester (the invitee); and a transfer's paired write, the previous
-- owner's record rewritten as moderator in the same commit.
ALTER TABLE community_write_intents ADD COLUMN invitation_id TEXT;
ALTER TABLE community_write_intents ADD COLUMN audit_actor_did TEXT;
ALTER TABLE community_write_intents ADD COLUMN paired_target_uri TEXT;
ALTER TABLE community_write_intents ADD COLUMN paired_record JSONB;
ALTER TABLE community_write_intents ADD COLUMN paired_replaced_cid TEXT;
ALTER TABLE community_write_intents ADD CONSTRAINT community_write_intent_paired_write CHECK (
    (paired_target_uri IS NULL) = (paired_record IS NULL)
);
-- At most one write in flight per record, for a paired target too.
CREATE UNIQUE INDEX community_write_intents_open_paired_target ON community_write_intents (paired_target_uri)
    WHERE state = 'open' AND paired_target_uri IS NOT NULL;

-- communityInvitation: the invitee was invited to a community role. It has
-- no record and no root post; subject_uri is at://<community DID> and the
-- actor is the inviter.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_reason_check CHECK (reason IN (
    'postReply', 'commentReply', 'mention', 'upvote', 'communityInvitation'
));
ALTER TABLE notifications DROP CONSTRAINT notifications_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_check CHECK (
    (reason = 'upvote' AND record_uri IS NULL AND record_cid IS NULL AND actor_did IS NULL
        AND subject_uri IS NOT NULL)
 OR (reason IN ('postReply','commentReply') AND record_uri IS NOT NULL AND record_cid IS NOT NULL
        AND actor_did IS NOT NULL AND subject_uri IS NOT NULL)
 OR (reason = 'mention' AND record_uri IS NOT NULL AND record_cid IS NOT NULL
        AND actor_did IS NOT NULL AND subject_uri IS NULL)
 OR (reason = 'communityInvitation' AND record_uri IS NULL AND record_cid IS NULL
        AND actor_did IS NOT NULL AND subject_uri IS NOT NULL AND root_post_uri IS NULL)
);
ALTER TABLE notifications ALTER COLUMN root_post_uri DROP NOT NULL;
ALTER TABLE notifications ADD CONSTRAINT notifications_root_post_present CHECK (
    root_post_uri IS NOT NULL OR reason = 'communityInvitation'
);

-- +goose Down
-- Invitations and their notifications have no place in the 083 schema.
DELETE FROM notifications WHERE reason = 'communityInvitation';
ALTER TABLE notifications DROP CONSTRAINT notifications_root_post_present;
ALTER TABLE notifications ALTER COLUMN root_post_uri SET NOT NULL;
ALTER TABLE notifications DROP CONSTRAINT notifications_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_check CHECK (
    (reason = 'upvote' AND record_uri IS NULL AND record_cid IS NULL AND actor_did IS NULL
        AND subject_uri IS NOT NULL)
 OR (reason IN ('postReply','commentReply') AND record_uri IS NOT NULL AND record_cid IS NOT NULL
        AND actor_did IS NOT NULL AND subject_uri IS NOT NULL)
 OR (reason = 'mention' AND record_uri IS NOT NULL AND record_cid IS NOT NULL
        AND actor_did IS NOT NULL AND subject_uri IS NULL)
);
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_reason_check CHECK (reason IN (
    'postReply', 'commentReply', 'mention', 'upvote'
));
-- Accept intents name invitations and paired writes, which the 083 schema
-- cannot hold; their actions stay.
DELETE FROM community_write_intents WHERE invitation_id IS NOT NULL;
DROP INDEX IF EXISTS community_write_intents_open_paired_target;
ALTER TABLE community_write_intents DROP CONSTRAINT community_write_intent_paired_write;
ALTER TABLE community_write_intents DROP COLUMN paired_replaced_cid;
ALTER TABLE community_write_intents DROP COLUMN paired_record;
ALTER TABLE community_write_intents DROP COLUMN paired_target_uri;
ALTER TABLE community_write_intents DROP COLUMN audit_actor_did;
ALTER TABLE community_write_intents DROP COLUMN invitation_id;
DROP TABLE IF EXISTS community_role_invitations;

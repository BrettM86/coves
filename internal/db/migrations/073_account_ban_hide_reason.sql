-- +goose Up
-- 'account_ban' marks a comment or vote indexed while an instance account ban
-- on its author was in force at Coves index time, in any community. Like
-- 'community_ban' it is permanent: an unban or a lapse does not clear it.
ALTER TABLE comments DROP CONSTRAINT comments_hide_reason;
ALTER TABLE comments ADD CONSTRAINT comments_hide_reason
    CHECK (hide_reason IN ('community_ban', 'pending_ban_check', 'account_ban'));
ALTER TABLE votes DROP CONSTRAINT votes_hide_reason;
ALTER TABLE votes ADD CONSTRAINT votes_hide_reason
    CHECK (hide_reason IN ('community_ban', 'pending_ban_check', 'account_ban'));

-- +goose Down
-- Fails while any comment or vote is recorded 'account_ban': the 072 schema
-- cannot hold one, and rewriting the marker would serve hidden content.
ALTER TABLE votes DROP CONSTRAINT votes_hide_reason;
ALTER TABLE votes ADD CONSTRAINT votes_hide_reason
    CHECK (hide_reason IN ('community_ban', 'pending_ban_check'));
ALTER TABLE comments DROP CONSTRAINT comments_hide_reason;
ALTER TABLE comments ADD CONSTRAINT comments_hide_reason
    CHECK (hide_reason IN ('community_ban', 'pending_ban_check'));

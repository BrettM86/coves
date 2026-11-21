-- +goose Up
-- The communityReport group: one row per (recipient, community) whose subject
-- is at://<community DID>, with no record, actor or root post. New reports in
-- the community's queue bump it; it is served only while the queue has open
-- community-visible reports (docs/NOTIFICATIONS.md, "Community report groups").
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_reason_check CHECK (reason IN (
    'postReply', 'commentReply', 'mention', 'upvote', 'communityInvitation', 'communityReport'
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
 OR (reason = 'communityReport' AND record_uri IS NULL AND record_cid IS NULL
        AND actor_did IS NULL AND subject_uri IS NOT NULL AND root_post_uri IS NULL)
);
ALTER TABLE notifications DROP CONSTRAINT notifications_root_post_present;
ALTER TABLE notifications ADD CONSTRAINT notifications_root_post_present CHECK (
    root_post_uri IS NOT NULL OR reason IN ('communityInvitation', 'communityReport')
);
CREATE UNIQUE INDEX uq_notifications_community_report_group
    ON notifications (recipient_did, subject_uri) WHERE reason = 'communityReport';

-- +goose Down
-- The 091 schema has no communityReport reason.
DELETE FROM notifications WHERE reason = 'communityReport';
DROP INDEX IF EXISTS uq_notifications_community_report_group;
ALTER TABLE notifications DROP CONSTRAINT notifications_root_post_present;
ALTER TABLE notifications ADD CONSTRAINT notifications_root_post_present CHECK (
    root_post_uri IS NOT NULL OR reason = 'communityInvitation'
);
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
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_reason_check CHECK (reason IN (
    'postReply', 'commentReply', 'mention', 'upvote', 'communityInvitation'
));

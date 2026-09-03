-- +goose Up
-- Community profiles previously seeded subscriber_count from an undeclared,
-- self-asserted subscriberCount property. Rebuild the materialized aggregate
-- from the subscription relationships this AppView actually indexed, then
-- keep it synchronized at the database boundary.
--
-- Version coupling: the binary that ships with this migration no longer
-- adjusts subscriber_count in Go. A pre-045 binary writing after this commits
-- would apply its own +1/-1 on top of the trigger and double-count. Migrations
-- run at server startup on a single host, so the two never overlap today; if
-- the deploy ever becomes rolling, quiesce subscription writers first.
--
-- This transaction escalates to ACCESS EXCLUSIVE on communities for the ALTER
-- TABLE below. The migration connection carries no statement timeout, so
-- without a lock timeout a single idle reader on communities would stall the
-- boot silently. Fail loudly instead; goose wraps the file in one transaction,
-- so SET LOCAL scopes the timeout to it.
SET LOCAL lock_timeout = '5s';

-- Hold off subscription mutations for the duration of the recount. A writer
-- that commits before this lock is acquired is visible to the UPDATE; a writer
-- that starts afterward blocks until this transaction commits, and is then
-- maintained by the trigger.
LOCK TABLE community_subscriptions IN SHARE MODE;

-- +goose StatementBegin
CREATE FUNCTION maintain_community_subscriber_count()
RETURNS TRIGGER AS $$
DECLARE
    previous_count INTEGER;
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE communities
        SET subscriber_count = subscriber_count + 1,
            updated_at = NOW()
        WHERE did = NEW.community_did;
        -- The foreign key makes a missing community unreachable today. Keep
        -- the invariant honest if that constraint is ever relaxed or deferred.
        IF NOT FOUND THEN
            RAISE EXCEPTION 'subscriber_count trigger: community % does not exist', NEW.community_did
                USING ERRCODE = 'foreign_key_violation';
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        -- A cascade from DELETE FROM communities finds no row here, which is
        -- correct: the aggregate is gone with its owner.
        SELECT subscriber_count INTO previous_count
        FROM communities
        WHERE did = OLD.community_did
        FOR NO KEY UPDATE;
        IF FOUND THEN
            -- The floor keeps unsubscribes and account deletion working if
            -- the column has drifted, but drift is a bug, so leave a trace.
            IF previous_count <= 0 THEN
                RAISE WARNING 'subscriber_count drift: decrement from zero for community %', OLD.community_did;
            END IF;
            UPDATE communities
            SET subscriber_count = GREATEST(0, previous_count - 1),
                updated_at = NOW()
            WHERE did = OLD.community_did;
        END IF;
    -- UPDATE OF community_did fires whenever the column is in the SET list,
    -- even when the value is unchanged, hence the distinctness check.
    ELSIF OLD.community_did IS DISTINCT FROM NEW.community_did THEN
        UPDATE communities
        SET subscriber_count = GREATEST(0, subscriber_count - 1),
            updated_at = NOW()
        WHERE did = OLD.community_did;

        UPDATE communities
        SET subscriber_count = subscriber_count + 1,
            updated_at = NOW()
        WHERE did = NEW.community_did;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'subscriber_count trigger: community % does not exist', NEW.community_did
                USING ERRCODE = 'foreign_key_violation';
        END IF;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER maintain_community_subscriber_count
    AFTER INSERT OR DELETE OR UPDATE OF community_did ON community_subscriptions
    FOR EACH ROW
    EXECUTE FUNCTION maintain_community_subscriber_count();

-- Touches every row unconditionally, which also clears any NULL left by the
-- original nullable column definition before the NOT NULL below is applied.
UPDATE communities AS c
SET subscriber_count = (
    SELECT COUNT(*)::INTEGER
    FROM community_subscriptions AS s
    WHERE s.community_did = c.did
);

ALTER TABLE communities
    ALTER COLUMN subscriber_count SET NOT NULL,
    ADD CONSTRAINT communities_subscriber_count_nonnegative
        CHECK (subscriber_count >= 0);

-- +goose Down
-- Down is only safe together with a revert to the pre-045 binary. The binary
-- that ships with this migration maintains subscriber_count through the
-- trigger alone, so once the trigger is gone nothing moves the column and
-- every subsequent subscribe or unsubscribe silently freezes the count.
ALTER TABLE communities
    DROP CONSTRAINT IF EXISTS communities_subscriber_count_nonnegative,
    ALTER COLUMN subscriber_count DROP NOT NULL;

DROP TRIGGER IF EXISTS maintain_community_subscriber_count ON community_subscriptions;
DROP FUNCTION IF EXISTS maintain_community_subscriber_count();

-- The record-asserted values are not authoritative and cannot be restored
-- safely after the relationship-backed recount, so the corrected values stay.

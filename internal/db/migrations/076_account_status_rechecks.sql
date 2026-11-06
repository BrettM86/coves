-- +goose Up
-- A pending retry of an account's PDS status confirmation. The users consumer
-- writes a row when com.atproto.sync.getRepoStatus fails for a takendown,
-- suspended or restoring #account event (PDSes emit #account only on change,
-- so the event alone is never retried), or answers RepoNotFound while a
-- restriction is stored. The recheck worker retries it with backoff and
-- deletes the row once the PDS answers. A newer #account event that asks the
-- PDS again supersedes the row; the exception is an activation of an account
-- with no stored restriction, which asks no PDS and keeps a pending recheck,
-- so a forged active event cannot cancel a pending takedown check.
--
-- Only DIDs with Coves presence (a users or communities row, or an authored
-- post or comment) or a stored restriction get rows: Jetstream carries
-- #account for the whole network, and an unknown DID's event is ignored. The
-- worker takes due rows fewest attempts first, then oldest next_attempt_at, so
-- a long-failing DID cannot starve newer ones. last_error_code is the latest
-- failure's accountstatus.ConfirmationFailure.
CREATE TABLE account_status_rechecks (
    did text PRIMARY KEY,
    attempts integer NOT NULL CHECK (attempts > 0),
    next_attempt_at timestamptz NOT NULL,
    last_error_code text NOT NULL CHECK (last_error_code IN (
        'identity_unresolved',
        'pds_unreachable',
        'pds_error_status',
        'pds_invalid_response',
        'unclassified',
        'repo_not_found'
    )),
    updated_at timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX account_status_rechecks_attempts_next_attempt_at_idx
    ON account_status_rechecks (attempts, next_attempt_at);

-- +goose Down
DROP TABLE account_status_rechecks;

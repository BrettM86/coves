-- +goose Up
-- Rule-7 stub: intentionally a no-op. The RED test for the vote-drift repair
-- runs against this empty migration; the real sweep and recount SQL lands in
-- the GREEN step.

-- +goose Down
-- No-op by design. This migration repairs drifted data, it does not change
-- schema, and a repair cannot be un-repaired: the pre-migration counts and the
-- orphaned live vote rows are not recoverable once corrected. Rolling back
-- leaves the corrected data in place, which is the safe outcome.
SELECT 1;

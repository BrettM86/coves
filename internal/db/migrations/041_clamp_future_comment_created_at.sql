-- +goose Up
-- Comments indexed before the ingest clamp may carry a future created_at. The
-- hot-rank SQL now tolerates these rows, but a future timestamp still pins a
-- comment to the top of the "new" sort until wall-clock catches up.
--
-- indexed_at is set from the AppView wall clock at insert and advanced to the
-- Jetstream event time on updates, making it the closest honest value for when
-- the AppView first saw the comment. LEAST guards the updated-row case where a
-- skewed relay clock put indexed_at itself in the future.
--
-- Posts are deliberately excluded: their ingest clamp shipped earlier without
-- a backfill, and posts never crashed because their hot rank already clamps.
-- A posts repair is tracked separately.
UPDATE comments SET created_at = LEAST(indexed_at, NOW()) WHERE created_at > NOW();

-- +goose Down
-- No-op: the original future values are unrecoverable, and the repaired values
-- are valid timestamps that are safe to retain.
SELECT 1;

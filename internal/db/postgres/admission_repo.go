package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"Coves/internal/core/posts"
)

// PostgreSQL storage for per-(community, post) admission decisions
// (docs/PRD_AUTHOR_OWNED_POSTS.md §5.2, §5.5, §6.1; migration 034).
//
// THE SHAPE EVERY MUTATION TAKES. All five are one INSERT ... ON CONFLICT DO
// UPDATE ... WHERE <guard>, and the guard is the whole decision: Postgres
// evaluates it against the conflicting row while holding that row's lock, so
// two consumers draining overlapping Jetstream feeds cannot interleave a read
// and a write. There is no SELECT-then-decide anywhere in this file, which is
// what makes a duplicate delivery a genuine no-op rather than a re-stamped
// decision timestamp.
//
// updated_at is set ONLY inside the guarded SET clause. A refused event must
// leave the row byte-identical — the moderation audit trail would otherwise
// become a function of how many feeds happened to carry the event.
//
// WHAT A REFUSAL RETURNS. A skip is an outcome, never an error (migration 033's
// precedent, restated in §5.2): stale cross-feed copies, dead-letter redrives
// and author edits of removed posts are the system working, and returning them
// as errors would bury them in the dead-letter queue among genuine failures.
// The refused caller still gets the current row, because deciding whether to
// notify or re-emit needs it and fetching it separately would reintroduce the
// race the CAS exists to close.
//
// The classifying SELECT that follows a refused upsert runs in the SAME
// transaction, so it reads the row under the lock the failed ON CONFLICT DO
// UPDATE already took: the row reported back is exactly the row the guard
// refused, not whatever a concurrent consumer left a moment later.

type postgresAdmissionRepo struct {
	db *sql.DB
}

// NewAdmissionRepository creates a PostgreSQL admission repository.
func NewAdmissionRepository(db *sql.DB) posts.AdmissionRepository {
	return &postgresAdmissionRepo{db: db}
}

// admissionColumns is the ordered column list every query in this file
// returns. It MUST stay byte-aligned with the positional Scan in
// scanAdmission, so it is written once and shared.
const admissionColumns = `
	community_did, post_uri, status,
	acceptance_uri, acceptance_rkey, accepted_cid,
	decision_code, decision_at, evaluated_cid, redrivable,
	last_community_rev, last_community_op_rank,
	created_at, updated_at`

// communityWatermarkGuard is the §5.2 rule, and the only thing that decides
// whether a community-repo event applies: the event's (rev, op-rank) tuple must
// be strictly greater than the row's.
//
// Postgres compares row constructors lexicographically, which is exactly the
// intent — a put (rank 1) carrying the same rev as a delete (rank 0) outranks
// it, so the removal commit converges on `removed` and the restore commit on
// `accepted` whichever half arrives first. Revs compare as TEXT COLLATE "C",
// i.e. bytewise, which for base32-sortable TIDs IS commit order.
//
// The IS NULL branch admits the first community event about a subject. It is
// not merely a convenience: the tuple comparison would evaluate to NULL there,
// and NULL reads as "not greater", so without it a subject whose first event is
// a community event could never leave its initial state.
const communityWatermarkGuard = `
	WHERE community_post_admissions.last_community_rev IS NULL
	   OR (community_post_admissions.last_community_rev, community_post_admissions.last_community_op_rank)
	    < (excluded.last_community_rev, excluded.last_community_op_rank)`

// UpsertPending records an AUTHOR-repo observation of the post's content.
//
// It carries no watermark and touches none: author and community repos have
// unrelated revision clocks (author events are ordered by migration 033's
// per-record gate), and letting an edit advance the community watermark would
// let an author outrank a moderator.
//
// The status the observation produces is a function of the CID it carries:
//
//   - removed stays removed, evaluated_cid and updated_at aside. Removal is
//     terminal against author events (§5.5) — if an edit could leave `removed`,
//     editing would launder a removed post back through auto-acceptance. The
//     content is still recorded, because a later moderator restore has to judge
//     what the AppView holds NOW, not what was removed.
//   - accepted whose acceptance pins a different CID becomes
//     pending_reacceptance: the standing acceptance pins the pre-edit content,
//     and edited content must never render under it.
//   - pending_reacceptance whose acceptance pins THIS CID becomes accepted.
//     That is what makes an acceptance arriving before its subject's post event
//     converge when the post event lands, instead of livelocking.
//   - rejected re-opens as pending on new content, and drops the decision that
//     judged the old content along with it — including its redrivable=false, or
//     the new content would never be evaluated at all.
func (r *postgresAdmissionRepo) UpsertPending(ctx context.Context, cmd posts.UpsertPendingCommand) (posts.AdmissionResult, error) {
	// The guard is content-distinctness alone. Every transition above is a
	// function of the incoming CID, so a re-delivery carrying the CID already
	// recorded cannot change any column — and must therefore write none,
	// including updated_at.
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status, evaluated_cid, created_at, updated_at
		) VALUES ($1, $2, 'pending', $3, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			evaluated_cid = excluded.evaluated_cid,
			status = CASE
				WHEN community_post_admissions.status = 'removed'
					THEN community_post_admissions.status
				WHEN community_post_admissions.status = 'rejected'
					THEN 'pending'
				WHEN community_post_admissions.status = 'accepted'
					AND community_post_admissions.accepted_cid IS DISTINCT FROM excluded.evaluated_cid
					THEN 'pending_reacceptance'
				WHEN community_post_admissions.status = 'pending_reacceptance'
					AND community_post_admissions.accepted_cid IS NOT DISTINCT FROM excluded.evaluated_cid
					THEN 'accepted'
				ELSE community_post_admissions.status
			END,
			decision_code = CASE
				WHEN community_post_admissions.status = 'rejected' THEN NULL
				ELSE community_post_admissions.decision_code
			END,
			decision_at = CASE
				WHEN community_post_admissions.status = 'rejected' THEN NULL
				ELSE community_post_admissions.decision_at
			END,
			redrivable = CASE
				WHEN community_post_admissions.status = 'rejected' THEN true
				ELSE community_post_admissions.redrivable
			END,
			updated_at = NOW()
		WHERE community_post_admissions.evaluated_cid IS DISTINCT FROM excluded.evaluated_cid
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "UpsertPending", cmd.CommunityDID, cmd.PostURI, authorEventOutcome,
		query, cmd.CommunityDID, cmd.PostURI, cmd.EvaluatedCID)
}

// ApplyAcceptance applies a community acceptance record write under the §5.2
// watermark rule.
//
// The acceptance fields are persisted whether or not the pinned CID matches the
// indexed content; only the STATUS distinguishes the two. Persisting them on a
// mismatch is what lets an acceptance that arrives before its subject's edit
// converge later (see UpsertPending), and a mismatch is common rather than
// exceptional: the author edits, and the community's acceptance for the older
// content is still in flight.
//
// It also clears the decision columns, which is not tidiness. There is no
// distinct restore operation on the wire — only the community's key holder can
// write acceptances, so a community acceptance at a strictly greater watermark
// IS the moderator restore. In the delivery order where the removal's deletion
// arrives after the fresh acceptance, that deletion is refused as not-greater,
// so the acceptance is the only event left that can clear the removal.
func (r *postgresAdmissionRepo) ApplyAcceptance(ctx context.Context, cmd posts.ApplyAcceptanceCommand) (posts.AdmissionResult, error) {
	// On a subject this AppView has no row for, the acceptance is the first
	// thing known about it, and the CID the community pinned is the only
	// content identifier available — so it is recorded as evaluated. A post
	// event that later lands a different CID moves the row to
	// pending_reacceptance through the ordinary path.
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status,
			acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid,
			last_community_rev, last_community_op_rank, created_at, updated_at
		) VALUES ($1, $2, 'accepted', $3, $4, $5, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			status = CASE
				WHEN community_post_admissions.evaluated_cid IS NOT DISTINCT FROM excluded.accepted_cid
					THEN 'accepted'
				ELSE 'pending_reacceptance'
			END,
			acceptance_uri = excluded.acceptance_uri,
			acceptance_rkey = excluded.acceptance_rkey,
			accepted_cid = excluded.accepted_cid,
			decision_code = NULL,
			decision_at = NULL,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()` + communityWatermarkGuard + `
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "ApplyAcceptance", cmd.CommunityDID, cmd.PostURI, communityEventOutcome,
		query, cmd.CommunityDID, cmd.PostURI,
		cmd.AcceptanceURI, cmd.AcceptanceRkey, cmd.PinnedCID,
		cmd.Watermark.Rev, int16(cmd.Watermark.OpRank))
}

// ApplyAcceptanceDelete applies the deletion of the acceptance record.
//
// Deleting an acceptance withdraws it; it does not decide anything. So a
// subject that was accepted (or awaiting re-acceptance under that acceptance)
// falls back to pending, and any other status stands — deleting an acceptance
// cannot un-remove a post. Even then the watermark advances and the acceptance
// columns clear, which is the point: the row must record that this event was
// seen, or the stale acceptance-create it superseded would apply on redelivery.
func (r *postgresAdmissionRepo) ApplyAcceptanceDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	// A deletion for a subject with no row still inserts one: it is a
	// tombstone, and without it the acceptance-create this deletion supersedes
	// would apply the next time a feed replays it.
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status,
			last_community_rev, last_community_op_rank, created_at, updated_at
		) VALUES ($1, $2, 'pending', $3, $4, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			status = CASE
				WHEN community_post_admissions.status IN ('accepted', 'pending_reacceptance')
					THEN 'pending'
				ELSE community_post_admissions.status
			END,
			acceptance_uri = NULL,
			acceptance_rkey = NULL,
			accepted_cid = NULL,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()` + communityWatermarkGuard + `
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "ApplyAcceptanceDelete", cmd.CommunityDID, cmd.PostURI, communityEventOutcome,
		query, cmd.CommunityDID, cmd.PostURI, cmd.Watermark.Rev, int16(cmd.Watermark.OpRank))
}

// ApplyRemoval applies a community removal record write under the §5.2
// watermark rule.
//
// It clears the acceptance columns for the same reason ApplyAcceptance clears
// the decision columns: in the delivery order where the removal arrives before
// the acceptance-delete that shares its rev, that deletion is refused as
// not-greater, so the removal is the only event left to clear them. A removed
// post carrying a live acceptance would also be indefensible on its own terms.
//
// A removal with no prior acceptance is valid and indexes normally —
// communities may remove pre-emptively — so the absent-row case inserts.
func (r *postgresAdmissionRepo) ApplyRemoval(ctx context.Context, cmd posts.ApplyRemovalCommand) (posts.AdmissionResult, error) {
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status, decision_code, decision_at,
			last_community_rev, last_community_op_rank, created_at, updated_at
		) VALUES ($1, $2, 'removed', $3, NOW(), $4, $5, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			status = 'removed',
			decision_code = excluded.decision_code,
			decision_at = excluded.decision_at,
			acceptance_uri = NULL,
			acceptance_rkey = NULL,
			accepted_cid = NULL,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()` + communityWatermarkGuard + `
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "ApplyRemoval", cmd.CommunityDID, cmd.PostURI, communityEventOutcome,
		query, cmd.CommunityDID, cmd.PostURI, cmd.DecisionCode,
		cmd.Watermark.Rev, int16(cmd.Watermark.OpRank))
}

// ApplyRemovalDelete applies the deletion of the removal record.
//
// Withdrawing a removal returns the subject to pending and drops the decision
// with it — a row that is no longer removed must not keep the code a reader
// would render. It does not restore any acceptance: the moderator's restore
// commit carries a FRESH acceptance alongside this deletion, and that
// acceptance is what makes the post visible again.
func (r *postgresAdmissionRepo) ApplyRemovalDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status,
			last_community_rev, last_community_op_rank, created_at, updated_at
		) VALUES ($1, $2, 'pending', $3, $4, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			status = CASE
				WHEN community_post_admissions.status = 'removed' THEN 'pending'
				ELSE community_post_admissions.status
			END,
			decision_code = CASE
				WHEN community_post_admissions.status = 'removed' THEN NULL
				ELSE community_post_admissions.decision_code
			END,
			decision_at = CASE
				WHEN community_post_admissions.status = 'removed' THEN NULL
				ELSE community_post_admissions.decision_at
			END,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()` + communityWatermarkGuard + `
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "ApplyRemovalDelete", cmd.CommunityDID, cmd.PostURI, communityEventOutcome,
		query, cmd.CommunityDID, cmd.PostURI, cmd.Watermark.Rev, int16(cmd.Watermark.OpRank))
}

// ---------------------------------------------------------------------------
// COMPILE STUBS — cycle 2's contract, deliberately unimplemented.
//
// These four are declared by posts.AdmissionRepository, so the package does not
// build without them. They return zero values so that the tests written against
// them fail on their ASSERTIONS rather than on a missing symbol: a red that is a
// build error proves nothing about the specification.
// ---------------------------------------------------------------------------

func (r *postgresAdmissionRepo) RepinAcceptedCID(ctx context.Context, cmd posts.RepinAcceptanceCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) RecordRejection(ctx context.Context, cmd posts.RecordRejectionCommand) (posts.AdmissionResult, error) {
	return posts.AdmissionResult{}, nil
}

func (r *postgresAdmissionRepo) GetByPostURIs(ctx context.Context, postURIs []string) (map[string][]*posts.Admission, error) {
	return nil, nil
}

func (r *postgresAdmissionRepo) ListByStatusForCommunity(ctx context.Context, communityDID string, status posts.AdmissionStatus, limit int, cursor *string) ([]*posts.Admission, *string, error) {
	return nil, nil, nil
}

// Get returns the admission row for one subject, or posts.ErrNotFound when this
// community has never seen this post.
func (r *postgresAdmissionRepo) Get(ctx context.Context, communityDID, postURI string) (*posts.Admission, error) {
	admission, err := scanAdmission(r.db.QueryRowContext(ctx,
		`SELECT `+admissionColumns+`
		 FROM community_post_admissions
		 WHERE community_did = $1 AND post_uri = $2`,
		communityDID, postURI))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, posts.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading the admission for %s in %s: %w", postURI, communityDID, err)
	}
	return admission, nil
}

// admissionOutcome decides what a mutation DID from whether the guarded upsert
// wrote and from the row it left behind. The two event sources classify
// differently, so each mutation passes its own.
type admissionOutcome func(wrote bool, current *posts.Admission) posts.AdmissionOutcome

// communityEventOutcome classifies a community-repo event. Its guard is the
// watermark and nothing else, so a refusal always means the event's tuple was
// not strictly greater: an out-of-order copy from another feed, or an exact
// replay of the event already applied.
func communityEventOutcome(wrote bool, _ *posts.Admission) posts.AdmissionOutcome {
	if wrote {
		return posts.AdmissionApplied
	}
	return posts.AdmissionSkippedStale
}

// authorEventOutcome classifies an author-repo event, where a write and an
// applied transition are not the same thing.
//
// A removed row still records the content it now holds, so the upsert writes
// while the transition it was asking for was refused by removal terminality —
// that is skipped_terminal, audit columns notwithstanding. Anything else that
// wrote applied; anything that did not write was a re-delivery of content the
// row already carried.
func authorEventOutcome(wrote bool, current *posts.Admission) posts.AdmissionOutcome {
	if current.Status == posts.AdmissionStatusRemoved {
		return posts.AdmissionSkippedTerminal
	}
	if wrote {
		return posts.AdmissionApplied
	}
	return posts.AdmissionSkippedStale
}

// compareAndSwap runs one guarded upsert and reports the row it left behind.
//
// The transaction exists for the refusal path. A guard that fails returns no
// rows, so the current state has to be read — and reading it in the same
// transaction means reading it under the row lock the failed ON CONFLICT DO
// UPDATE already holds, which is what makes the returned row provably the row
// the guard was evaluated against.
func (r *postgresAdmissionRepo) compareAndSwap(
	ctx context.Context,
	operation, communityDID, postURI string,
	classify admissionOutcome,
	query string,
	args ...interface{},
) (posts.AdmissionResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return posts.AdmissionResult{}, fmt.Errorf("%s for %s in %s: beginning transaction: %w", operation, postURI, communityDID, err)
	}
	defer func() {
		// ErrTxDone is the ordinary case: the happy path has already committed.
		// Anything else is a connection the pool should not be reusing quietly.
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("failed to roll back admission transaction",
				slog.String("operation", operation),
				slog.String("community_did", communityDID),
				slog.String("post_uri", postURI),
				slog.String("error", rollbackErr.Error()),
			)
		}
	}()

	admission, err := scanAdmission(tx.QueryRowContext(ctx, query, args...))
	wrote := true
	if errors.Is(err, sql.ErrNoRows) {
		wrote = false
		admission, err = scanAdmission(tx.QueryRowContext(ctx,
			`SELECT `+admissionColumns+`
			 FROM community_post_admissions
			 WHERE community_did = $1 AND post_uri = $2`,
			communityDID, postURI))
		if errors.Is(err, sql.ErrNoRows) {
			// Every guard here refuses only an existing row: an absent subject
			// takes the INSERT branch, which cannot be refused. Reaching this
			// means the row vanished between the two statements, which nothing
			// in the system does.
			return posts.AdmissionResult{}, fmt.Errorf("%s for %s in %s: the compare-and-swap neither wrote nor found a row", operation, postURI, communityDID)
		}
	}
	if err != nil {
		return posts.AdmissionResult{}, fmt.Errorf("%s for %s in %s: %w", operation, postURI, communityDID, err)
	}

	if err := tx.Commit(); err != nil {
		return posts.AdmissionResult{}, fmt.Errorf("%s for %s in %s: committing: %w", operation, postURI, communityDID, err)
	}
	return posts.AdmissionResult{Outcome: classify(wrote, admission), Admission: admission}, nil
}

// admissionRow is the one-row read both compareAndSwap and Get need, so the
// scan below can serve a *sql.Row from either a transaction or the pool.
type admissionRow interface {
	Scan(dest ...interface{}) error
}

// scanAdmission reads one row in admissionColumns order.
func scanAdmission(row admissionRow) (*posts.Admission, error) {
	var (
		admission      posts.Admission
		acceptanceURI  sql.NullString
		acceptanceRkey sql.NullString
		acceptedCID    sql.NullString
		decisionCode   sql.NullString
		decisionAt     sql.NullTime
		evaluatedCID   sql.NullString
		lastRev        sql.NullString
		lastOpRank     sql.NullInt16
	)

	if err := row.Scan(
		&admission.CommunityDID, &admission.PostURI, &admission.Status,
		&acceptanceURI, &acceptanceRkey, &acceptedCID,
		&decisionCode, &decisionAt, &evaluatedCID, &admission.Redrivable,
		&lastRev, &lastOpRank,
		&admission.CreatedAt, &admission.UpdatedAt,
	); err != nil {
		return nil, err
	}

	admission.AcceptanceURI = nullableString(acceptanceURI)
	admission.AcceptanceRkey = nullableString(acceptanceRkey)
	admission.AcceptedCID = nullableString(acceptedCID)
	admission.DecisionCode = nullableString(decisionCode)
	admission.EvaluatedCID = nullableString(evaluatedCID)
	if decisionAt.Valid {
		decidedAt := decisionAt.Time
		admission.DecisionAt = &decidedAt
	}
	// Migration 034 constrains the two halves of the watermark to be set or
	// unset together, so one test answers for both.
	if lastRev.Valid {
		admission.LastCommunityEvent = &posts.CommunityWatermark{
			Rev:    lastRev.String,
			OpRank: posts.CommunityOpRank(lastOpRank.Int16),
		}
	}
	return &admission, nil
}

// nullableString converts a scanned nullable column to the pointer the domain
// uses, so that "column is NULL" and "column is the empty string" stay
// distinguishable all the way up.
func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	scanned := value.String
	return &scanned
}

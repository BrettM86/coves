package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Coves/internal/core/posts"

	"github.com/lib/pq"
)

// PostgreSQL storage for per-(community, post) admission decisions
// (docs/PRD_AUTHOR_OWNED_POSTS.md §5.2, §5.5, §6.1; migration 034).
//
// THE SHAPE EVERY MUTATION TAKES. All seven are single-statement compare-and-
// swaps whose guard is the whole decision. Five — the author-repo observation
// and the four community events — are one INSERT ... ON CONFLICT DO UPDATE ...
// WHERE <guard>, because each may legitimately meet an absent subject and must
// create the row that records the event was seen. The other two are guarded
// UPDATEs, because each must NEVER create a row: a repin moves an acceptance
// that stands, and a rejection lands on the pending row the engine read from
// its own queue. Either way, Postgres evaluates the guard against the current
// row inside the writing statement, so two consumers draining overlapping
// Jetstream feeds cannot interleave a read and a write. There is no
// SELECT-then-decide anywhere in this file, which is what makes a duplicate
// delivery — RecordRejection's included — a genuine no-op rather than a
// re-stamped decision timestamp.
//
// updated_at is set ONLY inside the guarded SET clause. A refused event must
// leave the row byte-identical — the moderation audit trail would otherwise
// become a function of how many feeds happened to carry the event.
//
// INVARIANT: redrivable is false only while the decision that set it stands;
// every transition that reopens evaluation (new content reopening a rejection,
// a removal withdrawn with nothing in its place) resets it to true.
//
// WHAT A REFUSAL RETURNS. A skip is an outcome, never an error (migration 033's
// precedent, restated in §5.2): stale cross-feed copies, dead-letter redrives
// and author edits of removed posts are the system working, and returning them
// as errors would bury them in the dead-letter queue among genuine failures.
// The refused caller still gets the current row, because deciding whether to
// notify or re-emit needs it and fetching it separately would reintroduce the
// race the CAS exists to close.
//
// The classifying SELECT that follows a refused mutation runs in the SAME
// transaction and takes the row lock itself (FOR UPDATE). The lock has to be
// explicit: a refused ON CONFLICT DO UPDATE holds the conflicting row's lock
// already, but a guarded UPDATE that matched nothing holds no lock at all, and
// without one the row reported back could be whatever a concurrent consumer
// left a moment after the guard refused rather than the row it refused.

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
	// Removal terminality needs no arm of its own: a removed row matches none
	// of the transitions below, so it keeps its status through the ELSE — the
	// matrix tests "an edit while removed records the content and nothing else"
	// and "same CID on a removed row" pin exactly that.
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status, evaluated_cid, created_at, updated_at
		) VALUES ($1, $2, 'pending', $3, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			evaluated_cid = excluded.evaluated_cid,
			status = CASE
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
		  -- A SEED MAY FILL THE COLUMN IN, NEVER CHANGE IT. $4 is false for a
		  -- firehose observation, which is always the newest thing the AppView
		  -- has seen and may advance the row freely. It is true for the write
		  -- path's seed of a record it just committed, which may be carrying
		  -- content the firehose has ALREADY superseded — the CIDs differ
		  -- because the row is newer, and writing it would move the AppView's
		  -- belief about the post backwards in time. Combined with the
		  -- distinctness guard above, a seed meeting its own CID writes nothing
		  -- (including updated_at) and a seed meeting a different one writes
		  -- nothing at all. See posts.UpsertPendingCommand.IsSeed.
		  AND (NOT $4::boolean OR community_post_admissions.evaluated_cid IS NULL)
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "UpsertPending", cmd.CommunityDID, cmd.PostURI, upsertPendingOutcome, rowRequired,
		query, cmd.CommunityDID, cmd.PostURI, cmd.EvaluatedCID, cmd.IsSeed)
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
	if err := validateWatermark("ApplyAcceptance", cmd.CommunityDID, cmd.PostURI, cmd.Watermark); err != nil {
		return posts.AdmissionResult{}, err
	}

	// On a subject this AppView holds NO CONTENT for, the CID the community
	// pinned is the only content identifier available — so it is recorded as
	// evaluated and the row lands accepted. That covers two shapes of row: the
	// absent subject (INSERT path) and the NULL-evaluated tombstone a restore
	// commit's removal-delete half leaves when IT met the absent subject first
	// (conflict path, hence the COALESCE — NULL evaluated_cid means "nothing
	// recorded yet", never "content that mismatches", and treating it as a
	// mismatch would make the two delivery orders of {removal-delete,
	// acceptance} converge on different statuses). A post event that later
	// lands a different CID moves the row to pending_reacceptance through the
	// ordinary path.
	query := `
		INSERT INTO community_post_admissions (
			community_did, post_uri, status,
			acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid,
			last_community_rev, last_community_op_rank, created_at, updated_at
		) VALUES ($1, $2, 'accepted', $3, $4, $5, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
			status = CASE
				WHEN COALESCE(community_post_admissions.evaluated_cid, excluded.accepted_cid) = excluded.accepted_cid
					THEN 'accepted'
				ELSE 'pending_reacceptance'
			END,
			evaluated_cid = COALESCE(community_post_admissions.evaluated_cid, excluded.accepted_cid),
			acceptance_uri = excluded.acceptance_uri,
			acceptance_rkey = excluded.acceptance_rkey,
			accepted_cid = excluded.accepted_cid,
			decision_code = NULL,
			decision_at = NULL,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()` + communityWatermarkGuard + `
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "ApplyAcceptance", cmd.CommunityDID, cmd.PostURI, communityEventOutcome, rowRequired,
		query, cmd.CommunityDID, cmd.PostURI,
		cmd.AcceptanceURI, cmd.AcceptanceRkey, cmd.PinnedCID,
		cmd.Watermark.Rev, int16(posts.CommunityOpPut))
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
	if err := validateWatermark("ApplyAcceptanceDelete", cmd.CommunityDID, cmd.PostURI, cmd.Watermark); err != nil {
		return posts.AdmissionResult{}, err
	}

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

	return r.compareAndSwap(ctx, "ApplyAcceptanceDelete", cmd.CommunityDID, cmd.PostURI, communityEventOutcome, rowRequired,
		query, cmd.CommunityDID, cmd.PostURI, cmd.Watermark.Rev, int16(posts.CommunityOpDelete))
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
	if err := validateWatermark("ApplyRemoval", cmd.CommunityDID, cmd.PostURI, cmd.Watermark); err != nil {
		return posts.AdmissionResult{}, err
	}

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

	return r.compareAndSwap(ctx, "ApplyRemoval", cmd.CommunityDID, cmd.PostURI, communityEventOutcome, rowRequired,
		query, cmd.CommunityDID, cmd.PostURI, cmd.DecisionCode,
		cmd.Watermark.Rev, int16(posts.CommunityOpPut))
}

// ApplyRemovalDelete applies the deletion of the removal record.
//
// Withdrawing a removal returns the subject to pending and drops the decision
// with it — a row that is no longer removed must not keep the code a reader
// would render. Redrivable resets to true with it: the standing decision is
// what justified refusing to re-evaluate, and a pre-removal terminal rejection
// leaving redrivable=false behind would make the reopened pending row a post
// the redrive pass never judges. It does not restore any acceptance: the
// moderator's restore commit carries a FRESH acceptance alongside this
// deletion, and that acceptance is what makes the post visible again.
func (r *postgresAdmissionRepo) ApplyRemovalDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	if err := validateWatermark("ApplyRemovalDelete", cmd.CommunityDID, cmd.PostURI, cmd.Watermark); err != nil {
		return posts.AdmissionResult{}, err
	}

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
			redrivable = CASE
				WHEN community_post_admissions.status = 'removed' THEN true
				ELSE community_post_admissions.redrivable
			END,
			last_community_rev = excluded.last_community_rev,
			last_community_op_rank = excluded.last_community_op_rank,
			updated_at = NOW()` + communityWatermarkGuard + `
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "ApplyRemovalDelete", cmd.CommunityDID, cmd.PostURI, communityEventOutcome, rowRequired,
		query, cmd.CommunityDID, cmd.PostURI, cmd.Watermark.Rev, int16(posts.CommunityOpDelete))
}

// RepinAcceptedCID moves a standing acceptance onto new content without
// re-deciding anything — the §5.5 bridgedStats exception.
//
// Bridges refresh their records specifically to carry the origin platform's new
// vote counts, so the CID changes while nothing a moderator would judge does.
// Through the ordinary edit path each refresh would drop the post out of every
// feed until a re-acceptance commit caught up, repeatedly, for as long as the
// origin post keeps collecting votes. So this moves accepted_cid and
// evaluated_cid together and leaves the status alone. Moving them TOGETHER is
// the point: leaving evaluated_cid behind would make the very next author event
// read as an edit and demand the re-acceptance this exists to avoid.
//
// The caller establishes that the diff touches only bridgedStats and that the
// author passes the bridge-trust gate. What this enforces is narrower and
// structural: a repin updates an acceptance that STANDS on a row that is
// `accepted` NOW. Both halves of that guard earn their place. Without the
// acceptance-columns check, a repin arriving at a removed post — whose removal
// cleared the acceptance — would write a fresh accepted_cid onto it, which is
// exactly the live-acceptance-on-a-removed-post state the removal path takes
// care to prevent. Without the status check, a repin arriving at
// pending_reacceptance — which still CARRIES the acceptance columns — would
// move both CIDs under a status that says the acceptance does not cover the
// current content, silently converting an author's real edit into a
// stats-only refresh.
//
// It is an UPDATE rather than an upsert for the same reason: a repin must never
// be able to CREATE an admission. Manufacturing an accepted row from a bridge's
// refresh would be an auto-admission the community never wrote.
func (r *postgresAdmissionRepo) RepinAcceptedCID(ctx context.Context, cmd posts.RepinAcceptanceCommand) (posts.AdmissionResult, error) {
	if err := validateWatermark("RepinAcceptedCID", cmd.CommunityDID, cmd.PostURI, cmd.Watermark); err != nil {
		return posts.AdmissionResult{}, err
	}

	query := `
		UPDATE community_post_admissions SET
			accepted_cid = $3,
			evaluated_cid = $3,
			last_community_rev = $4,
			last_community_op_rank = $5,
			updated_at = NOW()
		WHERE community_did = $1
		  AND post_uri = $2
		  AND status = 'accepted'
		  AND acceptance_uri IS NOT NULL
		  AND (last_community_rev IS NULL
		       OR (last_community_rev, last_community_op_rank) < ($4::text COLLATE "C", $5::smallint))
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "RepinAcceptedCID", cmd.CommunityDID, cmd.PostURI, repinOutcome, rowOptional,
		query, cmd.CommunityDID, cmd.PostURI, cmd.PinnedCID,
		cmd.Watermark.Rev, int16(posts.CommunityOpPut))
}

// RecordRejection records the AppView's OWN decision not to admit a post.
//
// There is no community-repo record behind a rejection, which is why it must
// not advance the community watermark: a local decision that outranked a
// genuine community event would suppress the very acceptance that overrules it,
// leaving the community no way to publish its way past us. It does not touch
// evaluated_cid either — the rejection judges the content already recorded, it
// does not change what was judged.
//
// The guard is the engine's read made safe: rejection's ONLY legal source
// state is `pending` (§5.5 — re-acceptance failure is expressed as a removal,
// and a rejection landing on accepted or pending_reacceptance would suppress a
// live community acceptance with a local decision), and the row must still
// hold the exact CID the verdict judged, or an author's edit slipped in
// between the engine's read and its write and the verdict is about content
// that no longer exists. A replay of the decision already recorded meets
// status = 'rejected' and is refused without touching a byte — no re-stamped
// decision_at, however many feeds redrive it.
//
// It is an UPDATE, never an insert: the engine rejects rows it read from its
// own queue, so a subject with NO row is a caller bug that must surface as an
// error, not a decision to be recorded against nothing.
//
// Redrivable is the caller's classification of WHY, stored verbatim: a policy
// rejection is terminal and must not be retried by the dead-letter pass, while
// a transient evaluation failure has to stay retryable.
func (r *postgresAdmissionRepo) RecordRejection(ctx context.Context, cmd posts.RecordRejectionCommand) (posts.AdmissionResult, error) {
	query := `
		UPDATE community_post_admissions SET
			status = 'rejected',
			decision_code = $3,
			decision_at = NOW(),
			redrivable = $4,
			updated_at = NOW()
		WHERE community_did = $1
		  AND post_uri = $2
		  AND status = 'pending'
		  AND evaluated_cid IS NOT DISTINCT FROM $5
		RETURNING ` + admissionColumns

	return r.compareAndSwap(ctx, "RecordRejection", cmd.CommunityDID, cmd.PostURI, rejectionOutcome, rowRequired,
		query, cmd.CommunityDID, cmd.PostURI, cmd.DecisionCode, cmd.Redrivable, cmd.JudgedCID)
}

// GetByPostURIs returns every community's decision about each of the given
// posts, keyed by post URI.
//
// The map is per-URI rather than flat because one post genuinely has several
// decisions — an author may submit the same post to several communities, and
// the author's own view has to show each answer separately. A post no community
// has an opinion about is ABSENT from the map rather than present with an empty
// slice, so a caller ranging over it cannot mistake "never seen" for
// "considered and passed over".
func (r *postgresAdmissionRepo) GetByPostURIs(ctx context.Context, postURIs []string) (map[string][]*posts.Admission, error) {
	byPostURI := make(map[string][]*posts.Admission, len(postURIs))
	if len(postURIs) == 0 {
		return byPostURI, nil
	}

	// The URI set is bound through a single array parameter rather than an
	// interpolated IN list, so the SQL is constant — one cached plan whatever
	// the batch size, and no string building anywhere near a query. This
	// mirrors GetViewsByURIs, which hydrates the posts these decisions are
	// about.
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+admissionColumns+`
		FROM community_post_admissions
		WHERE post_uri = ANY($1)
		ORDER BY post_uri, community_did
	`, pq.Array(postURIs))
	if err != nil {
		return nil, fmt.Errorf("reading admissions for %d posts: %w", len(postURIs), err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close admission rows", slog.String("error", closeErr.Error()))
		}
	}()

	for rows.Next() {
		admission, scanErr := scanAdmission(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("reading admissions for %d posts: %w", len(postURIs), scanErr)
		}
		byPostURI[admission.PostURI] = append(byPostURI[admission.PostURI], admission)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading admissions for %d posts: %w", len(postURIs), err)
	}
	return byPostURI, nil
}

// admissionQueuePageSize bounds one page of a moderation queue. A caller asking
// for nothing gets the default rather than the whole queue, and a caller asking
// for more than the maximum gets the maximum: an unbounded listing is a table
// scan a moderator's browser cannot render anyway.
const (
	admissionQueuePageSize    = 50
	admissionQueuePageMaximum = 200
)

// ListByStatusForCommunity pages one community's admissions in one status —
// the moderation queue — oldest first, matching the
// (community_did, status, created_at) index.
//
// Both columns filter. Scoping by only one would still return a plausible page
// on the happy path and be wrong in production, where a community holds rows in
// every status and a status is held by every community.
//
// Paging is keyset rather than OFFSET: a queue is being worked while it is
// being read, and OFFSET shifts under inserts, which shows up as a moderator
// re-reviewing rows they had cleared or never seeing rows at all. The key is
// (created_at, post_uri) because created_at alone is not unique — two rows
// written in one transaction share it, and a cursor that could not separate
// them would either repeat or skip the pair.
func (r *postgresAdmissionRepo) ListByStatusForCommunity(ctx context.Context, communityDID string, status posts.AdmissionStatus, limit int, cursor *string) ([]*posts.Admission, *string, error) {
	if limit <= 0 {
		limit = admissionQueuePageSize
	}
	if limit > admissionQueuePageMaximum {
		limit = admissionQueuePageMaximum
	}

	keyset, err := parseAdmissionQueueCursor(cursor)
	if err != nil {
		return nil, nil, err
	}

	// Two constant statements rather than one assembled from pieces. Both are
	// fully parameterized either way, but a query whose text never varies has
	// one cached plan and can be read as-is, and the first page keeps the index
	// range the second page's keyset clause would otherwise have to fake with a
	// nullable comparison.
	query, args := admissionQueueFirstPage, []interface{}{communityDID, string(status), limit}
	if keyset != nil {
		query = admissionQueueNextPage
		args = []interface{}{communityDID, string(status), keyset.createdAt, keyset.postURI, limit}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("listing %s admissions for %s: %w", status, communityDID, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close admission rows", slog.String("error", closeErr.Error()))
		}
	}()

	page := make([]*posts.Admission, 0, limit)
	for rows.Next() {
		admission, scanErr := scanAdmission(rows)
		if scanErr != nil {
			return nil, nil, fmt.Errorf("listing %s admissions for %s: %w", status, communityDID, scanErr)
		}
		page = append(page, admission)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing %s admissions for %s: %w", status, communityDID, err)
	}

	// A short page exhausted the queue, so there is nothing to point at. Only a
	// FULL page might have more behind it.
	if len(page) < limit {
		return page, nil, nil
	}
	next := buildAdmissionQueueCursor(page[len(page)-1])
	return page, &next, nil
}

// The moderation queue's two statements. Oldest first matches the
// (community_did, status, created_at) index, and a queue that reordered under a
// moderator would have them re-reviewing what they had cleared.
//
// The next page's key is (created_at, post_uri), strictly greater: the row the
// cursor names was the last one delivered. post_uri is in the key because
// created_at alone is not unique — rows written in one transaction share it,
// and a cursor that could not separate them would either repeat the pair or
// skip it.
const (
	admissionQueueFirstPage = `
		SELECT ` + admissionColumns + `
		FROM community_post_admissions
		WHERE community_did = $1 AND status = $2
		ORDER BY created_at ASC, post_uri ASC
		LIMIT $3`

	admissionQueueNextPage = `
		SELECT ` + admissionColumns + `
		FROM community_post_admissions
		WHERE community_did = $1 AND status = $2
		  AND (created_at, post_uri) > ($3::timestamptz, $4::text)
		ORDER BY created_at ASC, post_uri ASC
		LIMIT $5`
)

// admissionQueueKeyset is a decoded cursor: the last row of the previous page.
type admissionQueueKeyset struct {
	createdAt string
	postURI   string
}

// parseAdmissionQueueCursor decodes a queue cursor. Format:
// base64(created_at|post_uri), following the convention post_repo's
// author-posts cursor established. A nil cursor decodes to nil, meaning the
// first page.
//
// A malformed cursor is an ERROR rather than a silent reset to the first page:
// restarting the queue under a moderator would make them re-review everything
// they had already cleared, and would read as a UI bug rather than as the
// corrupted input it is.
func parseAdmissionQueueCursor(cursor *string) (*admissionQueueKeyset, error) {
	if cursor == nil || *cursor == "" {
		return nil, nil
	}

	// Bound the input before decoding it: base64 expands into memory, and a
	// cursor is attacker-supplied on any endpoint that echoes one back.
	const maxCursorSize = 512
	if len(*cursor) > maxCursorSize {
		return nil, fmt.Errorf("%w: cursor exceeds maximum length", posts.ErrInvalidCursor)
	}

	decoded, err := base64.URLEncoding.DecodeString(*cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid base64 encoding", posts.ErrInvalidCursor)
	}

	createdAt, postURI, found := strings.Cut(string(decoded), "|")
	if !found {
		return nil, fmt.Errorf("%w: malformed cursor format", posts.ErrInvalidCursor)
	}
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return nil, fmt.Errorf("%w: invalid timestamp in cursor", posts.ErrInvalidCursor)
	}
	if !strings.HasPrefix(postURI, "at://") {
		return nil, fmt.Errorf("%w: invalid URI format in cursor", posts.ErrInvalidCursor)
	}
	return &admissionQueueKeyset{createdAt: createdAt, postURI: postURI}, nil
}

// buildAdmissionQueueCursor names the last row of a page, so the next page
// starts after it.
func buildAdmissionQueueCursor(last *posts.Admission) string {
	return base64.URLEncoding.EncodeToString(
		[]byte(last.CreatedAt.Format(time.RFC3339Nano) + "|" + last.PostURI))
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

// upsertPendingOutcome classifies the author-repo content observation, where a
// write and an applied transition are not the same thing.
//
// A delivery that wrote NOTHING met a row already holding exactly this content
// — a multi-feed duplicate — and that is skipped_stale whatever the row's
// status, a removed row included: nothing was recorded, so nothing was
// refused. A delivery that wrote but met `removed` recorded audit columns
// while the transition it asked for was refused by removal terminality
// (§5.5); that is skipped_terminal. Anything else that wrote applied.
func upsertPendingOutcome(wrote bool, current *posts.Admission) posts.AdmissionOutcome {
	if !wrote {
		return posts.AdmissionSkippedStale
	}
	if current.Status == posts.AdmissionStatusRemoved {
		return posts.AdmissionSkippedTerminal
	}
	return posts.AdmissionApplied
}

// rejectionOutcome classifies the AppView's own rejection, whose guard refuses
// in two honestly different ways. A row still `pending` refused because it no
// longer holds the judged CID — the verdict is about content that has been
// edited away, which is ordering skew: skipped_stale. A row already `rejected`
// met a replay of the decision it records: skipped_stale too. Any other status
// (accepted, pending_reacceptance, removed) refuses by STATE — a community
// decision or a standing acceptance outranks a local verdict regardless of
// when it arrives: skipped_terminal.
func rejectionOutcome(wrote bool, current *posts.Admission) posts.AdmissionOutcome {
	if wrote {
		return posts.AdmissionApplied
	}
	switch current.Status {
	case posts.AdmissionStatusPending, posts.AdmissionStatusRejected:
		return posts.AdmissionSkippedStale
	default:
		return posts.AdmissionSkippedTerminal
	}
}

// repinOutcome classifies a bridge repin, whose guard has two halves and so two
// ways to refuse. A row that is not `accepted` with its acceptance standing is
// the row's state refusing the transition regardless of ordering; anything
// else is the watermark.
func repinOutcome(wrote bool, current *posts.Admission) posts.AdmissionOutcome {
	if wrote {
		return posts.AdmissionApplied
	}
	if current.Status != posts.AdmissionStatusAccepted || current.AcceptanceURI == nil {
		return posts.AdmissionSkippedTerminal
	}
	return posts.AdmissionSkippedStale
}

// validateWatermark refuses a community event whose watermark cannot have come
// off the wire. Every Jetstream commit carries a rev, so an empty one is an
// upstream decoding bug — a genuine error bound for the dead-letter queue, not
// a skip — and stamping it would write a clock value that never existed onto
// the row (see posts.ErrInvalidWatermark).
func validateWatermark(operation, communityDID, postURI string, watermark posts.CommunityWatermark) error {
	if watermark.Rev == "" {
		return fmt.Errorf("%s for %s in %s: %w: empty rev", operation, postURI, communityDID, posts.ErrInvalidWatermark)
	}
	return nil
}

// rowExpectation states whether a mutation may legitimately find NO row when
// its guard refuses. The upserts insert on an absent subject, so for them a
// missing row after a refusal is impossible and reads as corruption; the
// UPDATE-shaped mutations differ, and only the repin treats absence as an
// ordinary state (see compareAndSwap).
const (
	rowRequired = false
	rowOptional = true
)

// compareAndSwap runs one guarded mutation and reports the row it left behind.
//
// The transaction exists for the refusal path. A guard that fails returns no
// rows, so the current state has to be read — and the classifying read locks
// the row (FOR UPDATE) in the same transaction, which is what makes the
// returned row provably the row the guard was evaluated against. A refused ON
// CONFLICT DO UPDATE already holds the conflicting row's lock, but a guarded
// UPDATE that matched nothing holds none, and without the explicit lock a
// concurrent consumer could rewrite the row between the refusal and the read.
func (r *postgresAdmissionRepo) compareAndSwap(
	ctx context.Context,
	operation, communityDID, postURI string,
	classify admissionOutcome,
	mayLackRow bool,
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
			 WHERE community_did = $1 AND post_uri = $2
			 FOR UPDATE`,
			communityDID, postURI))
		if errors.Is(err, sql.ErrNoRows) {
			// Only the UPDATE-shaped mutations can reach this — the upserts
			// insert when the subject is absent, so their guard can refuse
			// nothing but an existing row. Whether absence is a state or a bug
			// is per-operation. A repin, which must never create an admission,
			// simply has no acceptance to move: the ordering skew that delivers
			// a community event early delivers a bridge refresh early too, so
			// that is an outcome. A rejection, by contrast, is the engine
			// writing a verdict for a row it read from its own queue; no row
			// means the caller judged nothing, and burying that as a skip
			// would hide a genuine bug from the dead-letter queue.
			if mayLackRow {
				return posts.AdmissionResult{Outcome: posts.AdmissionSkippedTerminal}, nil
			}
			return posts.AdmissionResult{}, fmt.Errorf(
				"%s for %s in %s: no admission row to decide against: %w", operation, postURI, communityDID, posts.ErrNotFound)
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

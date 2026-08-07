//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The outer contract for community post admissions: one post's whole life inside
// one community, driven entirely through the repository API.
//
// docs/PRD_AUTHOR_OWNED_POSTS.md §5.2 and §5.5 are the specification. The claim
// under test is not "each method writes its columns" — it is that the ONE rule
// those sections describe (apply a community event only when its (rev, op-rank)
// tuple is strictly greater than the row's watermark, rank(delete) < rank(put))
// is sufficient to make the whole state machine converge. Convergence is what
// this file measures, so the interesting steps are written twice, once in each
// delivery order, and asserted against a single shared expectation.
//
// Two properties are load-bearing everywhere below and are worth stating once:
//
//   - AUTHOR-repo events never advance the community watermark. The author's
//     repo and the community's repo have unrelated revision clocks; mixing them
//     would let an author's edit silently outrank a moderator's removal.
//   - A skipped event is an OUTCOME, never an error. Migration 033 set that
//     precedent for the per-record gate; an error return here would push healthy
//     multi-feed duplicates into the dead-letter queue.

// admissionSubject is one (community, post) pair — the thing an admission row is
// about.
type admissionSubject struct {
	CommunityDID string
	PostURI      string
	AuthorDID    string
}

// newAdmissionSubject seeds a real community and a real post row, then returns
// the pair as a subject.
//
// The rows are seeded rather than faked because the admission table's subject is
// a foreign key in spirit whether or not it is one in SQL: an admission for a
// post nothing indexed is a state the AppView never reaches, and seeding the
// real rows is what makes a future constraint (or a join in a feed query) fail
// here rather than in production.
func newAdmissionSubject(t *testing.T, db *sql.DB) admissionSubject {
	t.Helper()

	ctx := context.Background()
	name := testkit.UniqueIDWithPrefix(t, "adm")

	communityDID, err := fixtures.Community(ctx, db, name, "owner"+name)
	require.NoErrorf(t, err, "seeding community %s", name)

	authorDID := fixtures.DID(testkit.UniqueID(t))
	postURI := fixtures.Post(t, db, communityDID, authorDID, "a post seeking admission", 0, time.Now())

	return admissionSubject{CommunityDID: communityDID, PostURI: postURI, AuthorDID: authorDID}
}

// increasingRevs returns n real atProto TIDs whose lexicographic order is their
// generation order.
//
// The gate compares revs as strings, so a test using invented revs would prove
// the gate works on invented data. testkit.TID emits genuine TIDs — fixed-length
// base32-sortable, exactly what a commit carries — and the require inside the
// loop pins the property the whole ordering rule rests on, so a change to the
// TID clock fails here with a clear message instead of somewhere downstream.
func increasingRevs(t *testing.T, n int) []string {
	t.Helper()

	revs := make([]string, n)
	for i := range revs {
		revs[i] = testkit.TID()
		if i > 0 {
			require.Greaterf(t, revs[i], revs[i-1],
				"testkit.TID must emit lexicographically increasing revs; got %q after %q", revs[i], revs[i-1])
		}
	}
	return revs
}

// contentCID returns a distinct, CID-shaped content identifier.
func contentCID(t *testing.T, label string) string {
	t.Helper()
	return "bafyrei" + testkit.UniqueIDWithPrefix(t, label)
}

// acceptanceRecord returns the URI and rkey a community's acceptance record for
// this subject would have.
func acceptanceRecord(t *testing.T, communityDID string) (uri, rkey string) {
	t.Helper()
	rkey = testkit.TID()
	return "at://" + communityDID + "/social.coves.community.acceptance/" + rkey, rkey
}

// assertNullableString asserts a nullable column holds exactly want.
func assertNullableString(t *testing.T, want string, got *string, what string) {
	t.Helper()
	if !assert.NotNilf(t, got, "%s: want %q, got NULL", what, want) {
		return
	}
	assert.Equalf(t, want, *got, "%s", what)
}

// assertWatermark asserts the subject-scoped composite watermark.
func assertWatermark(t *testing.T, wantRev string, wantRank posts.CommunityOpRank, got *posts.CommunityWatermark) {
	t.Helper()
	if !assert.NotNilf(t, got, "watermark: want (%s, %d), got NULL", wantRev, wantRank) {
		return
	}
	assert.Equalf(t, posts.CommunityWatermark{Rev: wantRev, OpRank: wantRank}, *got, "watermark")
}

// assertRemovedByCommit is the SHARED expectation for the removal commit
// {delete acceptance @ (rev, 0), create removal @ (rev, 1)}.
//
// Both delivery orders are asserted against this one function, which is the
// whole point: "converges" means the two orders are indistinguishable in the
// row they leave, not merely that both end up with status = removed.
//
// The cleared acceptance columns are part of convergence rather than a separate
// nicety. In the delete-first order the acceptance deletion clears them; in the
// removal-first order the deletion is skipped as stale, so the removal itself
// has to clear them or the two orders would leave different rows. A `removed`
// post carrying a live acceptance is also just wrong.
func assertRemovedByCommit(t *testing.T, got *posts.Admission, rev, decisionCode string) {
	t.Helper()

	require.NotNil(t, got, "admission row")
	assert.Equal(t, posts.AdmissionStatusRemoved, got.Status)
	assertNullableString(t, decisionCode, got.DecisionCode, "decision_code")
	assert.NotNil(t, got.DecisionAt, "decision_at must be stamped when a removal applies")
	assertWatermark(t, rev, posts.CommunityOpPut, got.LastCommunityEvent)

	assert.Nil(t, got.AcceptanceURI, "a removed post must carry no live acceptance URI, whichever half of the commit arrived first")
	assert.Nil(t, got.AcceptanceRkey, "a removed post must carry no live acceptance rkey, whichever half of the commit arrived first")
	assert.Nil(t, got.AcceptedCID, "a removed post must carry no accepted CID, whichever half of the commit arrived first")
}

// assertRestoredByCommit is the SHARED expectation for the restore commit
// {delete removal @ (rev, 0), create acceptance @ (rev, 1)}.
//
// There is no distinct "restore" operation on the wire: a community-authored
// acceptance at a strictly greater watermark than the removal IS the restore,
// because only the community's key holder can write acceptances. The cleared
// decision columns are convergence again — in the acceptance-first order the
// removal deletion is skipped, so the acceptance must clear them itself.
func assertRestoredByCommit(t *testing.T, got *posts.Admission, rev, acceptedCID string) {
	t.Helper()

	require.NotNil(t, got, "admission row")
	assert.Equal(t, posts.AdmissionStatusAccepted, got.Status)
	assertNullableString(t, acceptedCID, got.AcceptedCID, "accepted_cid")
	assert.NotNil(t, got.AcceptanceURI, "a restored post must carry the acceptance that restored it")
	assertWatermark(t, rev, posts.CommunityOpPut, got.LastCommunityEvent)

	assert.Nil(t, got.DecisionCode, "a restored post must carry no removal code, whichever half of the commit arrived first")
	assert.Nil(t, got.DecisionAt, "a restored post must carry no removal timestamp, whichever half of the commit arrived first")
}

// acceptedSubject seeds a fresh subject and drives it to accepted at (rev, put).
func acceptedSubject(t *testing.T, db *sql.DB, repo posts.AdmissionRepository, cid, rev string) admissionSubject {
	t.Helper()

	ctx := context.Background()
	subject := newAdmissionSubject(t, db)

	_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: cid,
	})
	require.NoError(t, err, "seeding the pending admission")

	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
	result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   subject.CommunityDID,
		PostURI:        subject.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err, "seeding the acceptance")
	require.NotNil(t, result.Admission, "seeding the acceptance returned no row")
	require.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status, "seeded subject is not accepted")

	return subject
}

// removedSubject seeds a fresh subject, accepts it at (acceptRev, put), then
// removes it with the full removal commit at (removalRev, ·).
func removedSubject(t *testing.T, db *sql.DB, repo posts.AdmissionRepository, cid, acceptRev, removalRev, decisionCode string) admissionSubject {
	t.Helper()

	ctx := context.Background()
	subject := acceptedSubject(t, db, repo, cid, acceptRev)

	_, err := repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		Watermark:    posts.CommunityWatermark{Rev: removalRev, OpRank: posts.CommunityOpDelete},
	})
	require.NoError(t, err, "seeding the acceptance deletion")

	result, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		DecisionCode: decisionCode,
		Watermark:    posts.CommunityWatermark{Rev: removalRev, OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err, "seeding the removal")
	require.NotNil(t, result.Admission, "seeding the removal returned no row")
	require.Equal(t, posts.AdmissionStatusRemoved, result.Admission.Status, "seeded subject is not removed")

	return subject
}

func TestAdmissionRepo_Lifecycle(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	subject := newAdmissionSubject(t, db)

	// Four community revisions, in the order the community's repo produced them:
	// the first acceptance, the re-acceptance after the author's edit, the
	// removal commit, and the restore commit.
	revs := increasingRevs(t, 4)
	revAcceptance, revReacceptance, revRemoval, revRestore := revs[0], revs[1], revs[2], revs[3]

	// Three content CIDs: as first written, after the author's edit, and after
	// the edit made while removed.
	originalCID := contentCID(t, "orig")
	editedCID := contentCID(t, "edit")
	restoredCID := contentCID(t, "restore")

	const decisionCode = "rule_violation"

	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)

	// The subtests below are steps of one journey over one row, so they run in
	// order and share state — none of them calls t.Parallel.

	t.Run("the author's post arrives: pending, with no community watermark", func(t *testing.T) {
		result, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			EvaluatedCID: originalCID,
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)

		require.NotNil(t, result.Admission, "a mutation must return the row it left behind")
		assert.Equal(t, posts.AdmissionStatusPending, result.Admission.Status)
		assert.Equal(t, subject.CommunityDID, result.Admission.CommunityDID)
		assert.Equal(t, subject.PostURI, result.Admission.PostURI)
		assertNullableString(t, originalCID, result.Admission.EvaluatedCID, "evaluated_cid")
		assert.Nil(t, result.Admission.AcceptedCID, "nothing has accepted this post yet")
		assert.Nil(t, result.Admission.DecisionCode, "no decision has been made yet")
		assert.True(t, result.Admission.Redrivable, "a pending admission must stay redrivable")
		assert.Nil(t, result.Admission.LastCommunityEvent,
			"an author-repo event must never set the community watermark: the two repos have unrelated revision clocks")
	})

	t.Run("the community accepts it: accepted, watermark at the acceptance", func(t *testing.T) {
		result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
			CommunityDID:   subject.CommunityDID,
			PostURI:        subject.PostURI,
			AcceptanceURI:  acceptanceURI,
			AcceptanceRkey: acceptanceRkey,
			PinnedCID:      originalCID,
			Watermark:      posts.CommunityWatermark{Rev: revAcceptance, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)

		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status,
			"the acceptance pins the CID the AppView indexed, so the post is accepted outright")
		assertNullableString(t, originalCID, result.Admission.AcceptedCID, "accepted_cid")
		assertNullableString(t, acceptanceURI, result.Admission.AcceptanceURI, "acceptance_uri")
		assertNullableString(t, acceptanceRkey, result.Admission.AcceptanceRkey, "acceptance_rkey")
		assertWatermark(t, revAcceptance, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})

	t.Run("the author edits: pending_reacceptance, community watermark untouched", func(t *testing.T) {
		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		require.NotNil(t, before)

		result, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			EvaluatedCID: editedCID,
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)

		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusPendingReacceptance, result.Admission.Status,
			"the standing acceptance pins the pre-edit CID, so edited content must not render under it")
		assertNullableString(t, editedCID, result.Admission.EvaluatedCID, "evaluated_cid")
		assert.Equal(t, before.LastCommunityEvent, result.Admission.LastCommunityEvent,
			"an author edit must not advance the community watermark, or the author could outrank a moderator")
	})

	t.Run("the community re-accepts the edit: accepted at the newer revision", func(t *testing.T) {
		result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
			CommunityDID:   subject.CommunityDID,
			PostURI:        subject.PostURI,
			AcceptanceURI:  acceptanceURI,
			AcceptanceRkey: acceptanceRkey,
			PinnedCID:      editedCID,
			Watermark:      posts.CommunityWatermark{Rev: revReacceptance, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)

		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status)
		assertNullableString(t, editedCID, result.Admission.AcceptedCID, "accepted_cid")
		assertWatermark(t, revReacceptance, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})

	t.Run("the removal commit converges from either delivery order", func(t *testing.T) {
		// One commit, two events: {delete acceptance @ (revRemoval, 0),
		// create removal @ (revRemoval, 1)}. Jetstream may deliver them in
		// either order across overlapping feeds, and the journey's subject takes
		// one order while a fresh mirror at the same state takes the other.
		mirror := acceptedSubject(t, db, repo, editedCID, revReacceptance)

		deleteAcceptance := func(s admissionSubject) posts.AdmissionResult {
			result, err := repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
				CommunityDID: s.CommunityDID,
				PostURI:      s.PostURI,
				Watermark:    posts.CommunityWatermark{Rev: revRemoval, OpRank: posts.CommunityOpDelete},
			})
			require.NoError(t, err)
			return result
		}
		createRemoval := func(s admissionSubject) posts.AdmissionResult {
			result, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
				CommunityDID: s.CommunityDID,
				PostURI:      s.PostURI,
				DecisionCode: decisionCode,
				Watermark:    posts.CommunityWatermark{Rev: revRemoval, OpRank: posts.CommunityOpPut},
			})
			require.NoError(t, err)
			return result
		}

		// Deletion first. Both events outrank the standing acceptance, so both
		// apply and the put lands last.
		assert.Equal(t, posts.AdmissionApplied, deleteAcceptance(subject).Outcome)
		assert.Equal(t, posts.AdmissionApplied, createRemoval(subject).Outcome)

		// Removal first. The deletion that follows carries the SAME rev at the
		// lower rank, so the tuple is not strictly greater and it is skipped —
		// which is the mechanism, not an accident: it is why the removal wins.
		assert.Equal(t, posts.AdmissionApplied, createRemoval(mirror).Outcome)
		assert.Equal(t, posts.AdmissionSkippedStale, deleteAcceptance(mirror).Outcome,
			"the acceptance deletion shares the removal's rev at the lower rank, so it must not un-remove the post")

		got, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assertRemovedByCommit(t, got, revRemoval, decisionCode)

		gotMirror, err := repo.Get(ctx, mirror.CommunityDID, mirror.PostURI)
		require.NoError(t, err)
		assertRemovedByCommit(t, gotMirror, revRemoval, decisionCode)
	})

	t.Run("replaying the removal changes nothing at all", func(t *testing.T) {
		// Overlapping feeds and dead-letter redrives deliver the same event more
		// than once. An equal tuple is not "slightly stale" — it is the event
		// already applied, and re-stamping decision_at from it would make the
		// moderation audit trail a function of feed topology.
		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		require.NotNil(t, before)

		result, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			DecisionCode: decisionCode,
			Watermark:    posts.CommunityWatermark{Rev: revRemoval, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err, "a duplicate delivery is the system working; it must not surface as an error")
		assert.Equal(t, posts.AdmissionSkippedStale, result.Outcome)

		// The WHOLE row, timestamps included: an assertion restricted to status
		// and watermark would pass against an implementation that rewrote
		// decision_at and updated_at on every replay.
		assert.Equal(t, before, result.Admission, "the skipped replay must return the untouched row")

		after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assert.Equal(t, before, after, "the skipped replay must not have written anything")
	})

	t.Run("an author edit while removed updates audit metadata only", func(t *testing.T) {
		// Removal is terminal against author-repo events. If an edit could move
		// the row out of removed, editing would launder a removed post back
		// through auto-acceptance — so the content the AppView holds is recorded
		// (a later moderator restore has to judge the CURRENT content) while the
		// decision is left exactly as the moderator made it.
		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		require.NotNil(t, before)

		result, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			EvaluatedCID: restoredCID,
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionSkippedTerminal, result.Outcome,
			"the transition was refused by removal terminality, even though the audit columns advanced")

		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusRemoved, result.Admission.Status)
		assert.Equal(t, before.DecisionCode, result.Admission.DecisionCode, "decision_code is the moderator's, not the author's")
		assert.Equal(t, before.DecisionAt, result.Admission.DecisionAt, "decision_at is the moderator's, not the author's")
		assert.Equal(t, before.LastCommunityEvent, result.Admission.LastCommunityEvent,
			"an author-repo event must never advance the community watermark")

		assertNullableString(t, restoredCID, result.Admission.EvaluatedCID, "evaluated_cid must record the content now indexed")
		assert.True(t, result.Admission.UpdatedAt.After(before.UpdatedAt),
			"updated_at must advance: the row was audited even though the decision stood")
	})

	t.Run("the restore commit converges from either delivery order", func(t *testing.T) {
		// The moderator's restore is one commit: {delete removal @ (revRestore,
		// 0), create acceptance @ (revRestore, 1)}. Same rule, opposite result —
		// the put outranks the delete, so the acceptance is what lands.
		mirror := removedSubject(t, db, repo, restoredCID, revReacceptance, revRemoval, decisionCode)

		deleteRemoval := func(s admissionSubject) posts.AdmissionResult {
			result, err := repo.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
				CommunityDID: s.CommunityDID,
				PostURI:      s.PostURI,
				Watermark:    posts.CommunityWatermark{Rev: revRestore, OpRank: posts.CommunityOpDelete},
			})
			require.NoError(t, err)
			return result
		}
		createAcceptance := func(s admissionSubject) posts.AdmissionResult {
			uri, rkey := acceptanceRecord(t, s.CommunityDID)
			result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
				CommunityDID:   s.CommunityDID,
				PostURI:        s.PostURI,
				AcceptanceURI:  uri,
				AcceptanceRkey: rkey,
				PinnedCID:      restoredCID,
				Watermark:      posts.CommunityWatermark{Rev: revRestore, OpRank: posts.CommunityOpPut},
			})
			require.NoError(t, err)
			return result
		}

		// Deletion first.
		assert.Equal(t, posts.AdmissionApplied, deleteRemoval(subject).Outcome)
		assert.Equal(t, posts.AdmissionApplied, createAcceptance(subject).Outcome)

		// Acceptance first: it outranks the removal on its own, and the removal
		// deletion that follows is skipped as not-strictly-greater.
		assert.Equal(t, posts.AdmissionApplied, createAcceptance(mirror).Outcome,
			"a community acceptance at a strictly greater watermark IS the restore; there is no separate restore operation")
		assert.Equal(t, posts.AdmissionSkippedStale, deleteRemoval(mirror).Outcome)

		got, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assertRestoredByCommit(t, got, revRestore, restoredCID)

		gotMirror, err := repo.Get(ctx, mirror.CommunityDID, mirror.PostURI)
		require.NoError(t, err)
		assertRestoredByCommit(t, gotMirror, revRestore, restoredCID)
	})
}

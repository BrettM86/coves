//go:build integration

package postgres

import (
	"context"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE LEDGER'S FROM-STATE GUARDS, and the divergence error they exist to raise.
//
// Every state-advancing UPDATE carries `AND state = $N`. That clause is the only
// thing standing between the cutover tool and a ledger that says a post is safe
// to delete when it is not: without it, MarkMigrated on a row still at
// `discovered` succeeds, the row reads as "verified, safe to delete", and the
// next pass deletes a legacy record whose replacement was never written.
//
// It is also the only thing that makes two concurrent runs SAY SO rather than
// interleave silently. Zero rows affected is an error on purpose.
//
// Stripping `AND state = $N` from all five guarded UPDATEs used to keep the
// whole suite green. This file is what makes that mutation fail.

// ledgerFixture stages one row at a chosen state and hands back the ledger.
func ledgerFixture(t *testing.T, at posts.RematerializeState) (posts.RematerializeLedger, string) {
	t.Helper()
	db := testkit.DB(t)
	ledger := NewRematerializeLedger(db)
	ctx := context.Background()

	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/" + testkit.TID()
	communityDID := "did:plc:community2222222222222222"
	authorDID := "did:plc:author11111111111111111"

	_, err := ledger.Discover(ctx, oldURI, communityDID, authorDID)
	require.NoError(t, err)

	// Walk the machine forward to the requested state through its own transitions,
	// so the fixture cannot stage a state the machine could not reach.
	newRkey := "3kremat" + testkit.TID()
	newURI := "at://" + authorDID + "/social.coves.community.postv2/" + newRkey
	steps := []struct {
		reaches posts.RematerializeState
		do      func() error
	}{
		{posts.RematerializePostV2Written, func() error {
			return ledger.RecordPostV2Written(ctx, oldURI, "bafysource", newURI, "bafynew", newRkey)
		}},
		{posts.RematerializeVerified, func() error { return ledger.MarkVerified(ctx, oldURI) }},
		{posts.RematerializeMigrated, func() error { return ledger.MarkMigrated(ctx, oldURI) }},
		{posts.RematerializeDone, func() error { return ledger.MarkDone(ctx, oldURI) }},
	}
	if at == posts.RematerializeFallbackLeftLegacy {
		require.NoError(t, ledger.MarkFallback(ctx, oldURI, posts.RematerializeFallbackLeftLegacy, "staged"))
		return ledger, oldURI
	}
	for _, step := range steps {
		if at == posts.RematerializeDiscovered {
			break
		}
		require.NoError(t, step.do())
		if step.reaches == at {
			break
		}
	}

	row, found, err := ledger.Get(ctx, oldURI)
	require.NoError(t, err)
	require.True(t, found)
	require.Equalf(t, at, row.State, "the fixture failed to stage the row at %s", at)
	return ledger, oldURI
}

func TestRematerializeLedger_EveryTransitionIsGuardedOnItsPriorState(t *testing.T) {
	t.Parallel()

	allStates := []posts.RematerializeState{
		posts.RematerializeDiscovered,
		posts.RematerializePostV2Written,
		posts.RematerializeVerified,
		posts.RematerializeMigrated,
		posts.RematerializeDone,
		posts.RematerializeFallbackLeftLegacy,
	}

	transitions := []struct {
		name string
		from posts.RematerializeState
		fire func(ledger posts.RematerializeLedger, oldURI string) error
		// why says what a missing guard would cost.
		why string
	}{
		{
			name: "RecordPostV2Written", from: posts.RematerializeDiscovered,
			fire: func(l posts.RematerializeLedger, uri string) error {
				return l.RecordPostV2Written(context.Background(), uri, "bafysource2", "at://x/y/z", "bafynew2", "3kagain")
			},
			why: "unguarded, it would overwrite the postv2 coordinates of a row that is already past the write — pointing a later delete at a record " +
				"this run never verified",
		},
		{
			name: "MarkVerified", from: posts.RematerializePostV2Written,
			fire: func(l posts.RematerializeLedger, uri string) error { return l.MarkVerified(context.Background(), uri) },
			why:  "unguarded, it would mark a row `verified` that has no postv2 at all",
		},
		{
			name: "MarkMigrated", from: posts.RematerializeVerified,
			fire: func(l posts.RematerializeLedger, uri string) error { return l.MarkMigrated(context.Background(), uri) },
			why: "unguarded, it would write the CHECKPOINT BEFORE DELETE onto a row nothing has been verified for — and `migrated` is exactly what a " +
				"resumed run reads as 'the delete is safe, just retry it'",
		},
		{
			name: "MarkDone", from: posts.RematerializeMigrated,
			fire: func(l posts.RematerializeLedger, uri string) error { return l.MarkDone(context.Background(), uri) },
			why: "unguarded, it would mark a row done — meaning 'the legacy record has been deleted' — for a record that is still standing, so the " +
				"census reports a drain that never happened",
		},
		{
			name: "MarkFallback", from: posts.RematerializeDiscovered,
			fire: func(l posts.RematerializeLedger, uri string) error {
				return l.MarkFallback(context.Background(), uri, posts.RematerializeFallbackLeftLegacy, "second thoughts")
			},
			why: "unguarded, it would sentence a row that already has a postv2 written for it — abandoning work that succeeded, and overwriting the " +
				"reason an operator is about to read",
		},
	}

	for _, tr := range transitions {
		for _, priorState := range allStates {
			if priorState == tr.from {
				continue
			}
			t.Run(tr.name+"_from_"+string(priorState), func(t *testing.T) {
				t.Parallel()
				ledger, oldURI := ledgerFixture(t, priorState)

				err := tr.fire(ledger, oldURI)

				require.Errorf(t, err,
					"%s succeeded against a row standing at %s (it is guarded on %s). %s.\nA transition that finds no row in its expected prior state "+
						"means the ledger and the tool have diverged, and that is a fault to surface, not to swallow into a false success",
					tr.name, priorState, tr.from, tr.why)
				assert.Containsf(t, err.Error(), "diverged",
					"the error must name the divergence: a 3am operator reading it needs to know the ledger disagrees with the tool, not that some SQL failed")

				row, found, err := ledger.Get(context.Background(), oldURI)
				require.NoError(t, err)
				require.True(t, found)
				assert.Equalf(t, priorState, row.State,
					"the refused transition CHANGED the row anyway: it now stands at %s. A guard that updates and then complains is not a guard", row.State)
			})
		}
	}
}

// The happy path still has to work — a guard that refuses everything would pass
// every test above.
func TestRematerializeLedger_TheOrderedTransitionsSucceedFromTheirOwnPriorState(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := NewRematerializeLedger(db)
	ctx := context.Background()

	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/" + testkit.TID()
	_, err := ledger.Discover(ctx, oldURI, "did:plc:community2222222222222222", "did:plc:author11111111111111111")
	require.NoError(t, err)

	require.NoError(t, ledger.RecordPostV2Written(ctx, oldURI, "bafysource", "at://a/b/c", "bafynew", "3krkey"))
	require.NoError(t, ledger.MarkVerified(ctx, oldURI))
	require.NoError(t, ledger.MarkMigrated(ctx, oldURI))
	require.NoError(t, ledger.MarkDone(ctx, oldURI))

	row, found, err := ledger.Get(ctx, oldURI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, posts.RematerializeDone, row.State)
	assert.Equalf(t, "bafysource", row.SourceCID,
		"the source CID must round-trip: it is what the pre-delete re-read is compared against and what the delete's swap guard is made of")
	assert.Equal(t, "at://a/b/c", row.NewURI)
	assert.Equal(t, "bafynew", row.NewCID)
	assert.Equal(t, "3krkey", row.NewRkey)
	assert.Equalf(t, "did:plc:community2222222222222222", row.CommunityDID,
		"the community DID must round-trip: it is the scope a staged run resumes, counts and deletes within")
}

// A postv2 recorded with no source CID leaves nothing to guard the eventual
// delete on, so it is refused at the ledger rather than discovered later.
func TestRematerializeLedger_RecordPostV2Written_RequiresASourceCID(t *testing.T) {
	t.Parallel()
	ledger, oldURI := ledgerFixture(t, posts.RematerializeDiscovered)

	err := ledger.RecordPostV2Written(context.Background(), oldURI, "", "at://a/b/c", "bafynew", "3krkey")
	require.Errorf(t, err,
		"a postv2 was recorded with no source CID. The source CID is the ONLY thing a later pass can check the legacy record against before deleting it; "+
			"a row without one either blocks forever or deletes blind")

	row, _, err := ledger.Get(context.Background(), oldURI)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDiscovered, row.State)
}

// ---- scope ------------------------------------------------------------------

// ListResumable, CountByState and ReopenFallback are all scoped, because a
// staged run that resumes another community's rows will DELETE that community's
// records.
func TestRematerializeLedger_ScopedQueriesDoNotReachAnotherCommunity(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := NewRematerializeLedger(db)
	ctx := context.Background()

	mine := "did:plc:mycommunity222222222222"
	theirs := "did:plc:theircommunity33333333"
	mineURI := "at://" + mine + "/social.coves.community.post/" + testkit.TID()
	theirsURI := "at://" + theirs + "/social.coves.community.post/" + testkit.TID()

	_, err := ledger.Discover(ctx, mineURI, mine, "did:plc:author11111111111111111")
	require.NoError(t, err)
	_, err = ledger.Discover(ctx, theirsURI, theirs, "did:plc:author11111111111111111")
	require.NoError(t, err)

	resumable, err := ledger.ListResumable(ctx, mine)
	require.NoError(t, err)
	for _, row := range resumable {
		assert.Equalf(t, mine, row.CommunityDID,
			"a run scoped to %s was handed %s's row to resume. The very next thing the tool does with a resumable row is drive it toward a DELETE",
			mine, row.CommunityDID)
	}
	assert.Lenf(t, resumable, 1, "the scoped resume set must contain exactly this community's row")

	counts, err := ledger.CountByState(ctx, mine)
	require.NoError(t, err)
	assert.Equalf(t, 1, counts[posts.RematerializeDiscovered],
		"the scoped census counted %d discovered rows; a census that reaches outside its scope makes every staged run report itself incomplete",
		counts[posts.RematerializeDiscovered])

	global, err := ledger.CountByState(ctx, "")
	require.NoError(t, err)
	assert.GreaterOrEqualf(t, global[posts.RematerializeDiscovered], 2,
		"the UNSCOPED census must see both communities: it is what gates the irreversible legacy-removal step, which is global")
}

func TestRematerializeLedger_ReopenFallback_IsScopedAndOnlyTouchesFallbackRows(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := NewRematerializeLedger(db)
	ctx := context.Background()

	mine := "did:plc:mycommunity222222222222"
	theirs := "did:plc:theircommunity33333333"
	author := "did:plc:author11111111111111111"

	mineFallback := "at://" + mine + "/social.coves.community.post/" + testkit.TID()
	theirsFallback := "at://" + theirs + "/social.coves.community.post/" + testkit.TID()
	mineInFlight := "at://" + mine + "/social.coves.community.post/" + testkit.TID()

	for _, uri := range []string{mineFallback, theirsFallback, mineInFlight} {
		communityDID := mine
		if uri == theirsFallback {
			communityDID = theirs
		}
		_, err := ledger.Discover(ctx, uri, communityDID, author)
		require.NoError(t, err)
	}
	require.NoError(t, ledger.MarkFallback(ctx, mineFallback, posts.RematerializeFallbackLeftLegacy, "no creds"))
	require.NoError(t, ledger.MarkFallback(ctx, theirsFallback, posts.RematerializeFallbackLeftLegacy, "no creds"))
	require.NoError(t, ledger.RecordPostV2Written(ctx, mineInFlight, "bafysource", "at://a/b/c", "bafynew", "3krkey"))

	moved, err := ledger.ReopenFallback(ctx, mine)
	require.NoError(t, err)
	assert.Equalf(t, 1, moved, "ReopenFallback moved %d row(s); it must move only this community's fallback rows", moved)

	reopened, _, err := ledger.Get(ctx, mineFallback)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDiscovered, reopened.State)
	assert.Emptyf(t, reopened.Reason, "a reopened row's stale fallback reason must be cleared, or the operator reads a verdict that no longer applies")

	untouched, _, err := ledger.Get(ctx, theirsFallback)
	require.NoError(t, err)
	assert.Equalf(t, posts.RematerializeFallbackLeftLegacy, untouched.State,
		"ReopenFallback reached outside its scope and reopened another community's row")

	inFlight, _, err := ledger.Get(ctx, mineInFlight)
	require.NoError(t, err)
	assert.Equalf(t, posts.RematerializePostV2Written, inFlight.State,
		"ReopenFallback moved a row that was not a fallback, discarding work that had already succeeded")
}

// Zero rows reopened is NOT an error: "there was nothing to reopen" is a normal,
// common answer, and this is not one of the ordered transitions whose no-op
// means divergence.
func TestRematerializeLedger_ReopenFallback_ZeroRowsIsNotAnError(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := NewRematerializeLedger(db)

	moved, err := ledger.ReopenFallback(context.Background(), "did:plc:nothingtoreopen1111111")
	require.NoErrorf(t, err, "reopening a scope with no fallback rows must be a quiet success, not a divergence error")
	assert.Equal(t, 0, moved)
}

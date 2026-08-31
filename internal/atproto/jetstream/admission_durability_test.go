//go:build integration

package jetstream

import (
	"context"

	"errors"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Six ways the admission row and the events that move it can fall out of step.
//
// They share a shape worth naming once, because it is the shape of almost every
// bug in this consumer: TWO WRITES, ONE EVENT. Indexing a post writes the posts
// row AND the admission row; a tombstone writes the tombstone AND withdraws the
// acceptance; converging writes the fetched post AND its pending admission. The
// rev gate exists to make the FIRST of each pair happen exactly once — and every
// case below is the second one being skipped, reverted or never retried because
// the gate already counted the event as done.
//
// The gate is right to be a gate. What is wrong is treating "this event has been
// seen" as "everything this event should have caused has happened", which is
// only true when the effects are atomic with the gate advance. Where they are
// not, the second write needs to be idempotent and unconditional rather than
// gated along with the first.

// flakyAdmissions wraps the real repository and fails a chosen method a fixed
// number of times before letting it through — a transient database fault, which
// is the ordinary way the second write of a pair goes missing.
type flakyAdmissions struct {
	posts.AdmissionRepository

	upsertFailures int
	upsertCalls    int
	err            error
}

func (a *flakyAdmissions) UpsertPending(ctx context.Context, cmd posts.UpsertPendingCommand) (posts.AdmissionResult, error) {
	a.upsertCalls++
	if a.upsertFailures > 0 {
		a.upsertFailures--
		return posts.AdmissionResult{}, a.err
	}
	return a.AdmissionRepository.UpsertPending(ctx, cmd)
}

// flakyDeleter fails the acceptance withdrawal a fixed number of times.
type flakyDeleter struct {
	failures int
	calls    []posts.CommunityAcceptanceDeleteCommand
	err      error
}

func (d *flakyDeleter) DeleteAcceptance(
	_ context.Context, cmd posts.CommunityAcceptanceDeleteCommand,
) (posts.CommunityWriteResult, error) {
	d.calls = append(d.calls, cmd)
	if d.failures > 0 {
		d.failures--
		return posts.CommunityWriteResult{}, d.err
	}
	return posts.CommunityWriteResult{Rev: testkit.TID()}, nil
}

// removalDeleteEvent builds the delete half of a community's removal record,
// arriving on its own — no paired acceptance-create in the same commit.
func removalDeleteEvent(communityDID, postURI, rev string, timeUS int64) *JetstreamEvent {
	return revCommitEvent(communityDID, posts.RemovalCollection, "delete",
		posts.SubjectRkey(postURI), rev, "", timeUS, nil)
}

func TestAdmission_LoneRemovalDeleteExitsRemoved(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newAccFixture(t, db)
	base := time.Now().UnixMicro()

	const cid = "bafyreiloneremoval"
	uri := f.indexPV2(t, "loneremoval", cid, base)

	revs := increasingTIDs(t, 3)
	require.NoError(t, f.consumer.HandleEvent(ctx, removalEvent(
		accCommunity, uri, cid, string(posts.DecisionRuleViolation), revs[0], base+1_000_000)))

	row, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusRemoved, row.Status, "fixture: the removal must stand")

	// A removal delete with NO paired acceptance-create. The restore commit of
	// §5.2 is {removal-delete, acceptance-create} together, and treating the
	// delete half as a no-op is safe THERE because the create outranks it. But a
	// moderator can also simply withdraw a removal — deleting the record and
	// writing nothing — and that commit carries only this event. Ignoring it
	// leaves the post `removed` forever, with the community's own repo no longer
	// saying so: the AppView and the signed record disagree, and only the
	// AppView is consulted when the post is served.
	require.NoError(t, f.consumer.HandleEvent(ctx, removalDeleteEvent(accCommunity, uri, revs[1], base+2_000_000)))

	row, err = f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	assert.Equalf(t, posts.AdmissionStatusPending, row.Status,
		"a lone removal delete left the post %q. The removal record is gone from the community's repo, so nothing "+
			"published says this post is removed — but the AppView still refuses to show it, and no later event will "+
			"change that, because `removed` is terminal against everything except a community event at a greater watermark", row.Status)

	// The audit fields go with it. A row that says `pending` while still
	// carrying a decision code reads, to getStatus and to a moderator, as a post
	// that was refused for a reason — which is the state the withdrawal undid.
	assert.Nil(t, row.DecisionCode, "the withdrawn removal's code must be cleared with it")
	assert.Nil(t, row.DecisionAt, "and its decision time")
	assert.Truef(t, row.Redrivable,
		"redrivable must be reset: it was set false by a terminal decision that no longer stands, and leaving it false "+
			"means the dead-letter redrive will never revisit this subject")
}

// racingFetcher runs one action the first time a fetch is made, then delegates.
//
// It is how the interleaving below is made deterministic without weakening what
// the fetch itself does: the REAL fetcher still reads the real repo and still
// recomputes the CID. All this decides is WHEN the competing event lands, which
// in production is decided by two repos Jetstream carries in parallel.
type racingFetcher struct {
	inner  PostRecordFetcher
	before func()
	once   sync.Once
}

func (f *racingFetcher) FetchPost(ctx context.Context, postURI string) (*FetchedPost, error) {
	f.once.Do(f.before)
	return f.inner.FetchPost(ctx, postURI)
}

func TestAdmission_ConvergeMustNotRegressTheEvaluatedCID(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newRealRepoFixture(t, db)

	// The repo holds ONE version, and the acceptance pins it. That version is
	// what the fetch will legitimately verify and return.
	record := f.publish(t, accCommunity, "the version the acceptance pinned")
	const newerCID = "bafyreiconvergenewer"

	// The author edits. The AppView learns about the edit from the firehose
	// while the fetch — started earlier, against the pre-edit repo — is still in
	// flight. That is not an exotic interleaving: the acceptance and the post
	// live in different repos, Jetstream parallelises across repos, and the
	// fetch exists precisely because the post's own event had not arrived yet.
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db),
		newMockUserService(), db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostRecordFetcher(&racingFetcher{
			inner: NewDirectPostFetcher(pinnedResolver(f.author.DID, f.pds.URL()),
				PrivatePostFetcherOptions(true)...),
			before: func() {
				require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
					f.author.DID, "create", record.RKey, testkit.TID(), newerCID, time.Now().UnixMicro(),
					pv2Record(accCommunity, "the version that actually arrived", "newer body"),
				)), "the racing post event must index cleanly")
			},
		}),
	)

	_ = f.consumer.HandleEvent(ctx,
		acceptanceEvent(accCommunity, record.URI, record.CID, testkit.TID(), time.Now().UnixMicro()))

	row, err := f.admissions.Get(ctx, accCommunity, record.URI)
	require.NoError(t, err)
	require.NotNil(t, row)

	// evaluated_cid is what the NEXT decision judges. Regressed to the fetched
	// version, the engine evaluates content the author has already replaced —
	// and an acceptance written from that verdict pins a version the AppView is
	// no longer serving, so the row reports `accepted` for content nobody sees.
	assertNullableStringPV2(t, newerCID, row.EvaluatedCID,
		"the converge path wrote its fetched CID over a NEWER one a real event had already recorded. UpsertPending is "+
			"last-write-wins, so a fetch that lost the race must not apply — the post event is the authority on what "+
			"content stands, and the fetch is only a catch-up")

	_, _, storedCID, _, _ := readPV2Post(t, db, record.URI)
	assert.Equal(t, newerCID, storedCID, "the indexed post must hold the version its own event carried")
}

func TestAdmission_SurvivesAFailedUpsertAcrossRedelivery(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	flaky := &flakyAdmissions{
		AdmissionRepository: postgres.NewAdmissionRepository(db),
		upsertFailures:      1,
		err:                 errors.New("community_post_admissions is briefly unreachable"),
	}
	f := newAccFixture(t, db)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db),
		newMockUserService(), db,
		WithAdmissions(flaky),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
	)

	rkey := "upsertflake"
	uri := accPostURI(rkey)
	rev := testkit.TID()
	event := pv2Event(accAuthor, "create", rkey, rev, "bafyreiupsertflake", time.Now().UnixMicro(),
		pv2Record(accCommunity, "indexed while the admissions table blipped", "body"))

	// First delivery: the post indexes, the admission does not. The event is
	// dead-lettered, which is correct — but the rev gate has already advanced.
	require.Error(t, f.consumer.HandleEvent(ctx, event),
		"fixture: the first delivery must fail on the admission write")

	// THE REDELIVERY, which is guaranteed: the connector rewinds its cursor
	// after every reconnect, the redriver replays dead letters, and overlapping
	// feeds carry the same commit. It arrives with the SAME rev, so the gate
	// rejects it — and if the admission upsert is gated along with the post
	// insert, the row is never created and the post is invisible in its
	// community forever, with no error anywhere and nothing left to retry.
	require.NoError(t, f.consumer.HandleEvent(ctx, event),
		"a redelivery of an already-gated event must not error")

	row, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoErrorf(t, err,
		"the admission row was never created. The rev gate makes the POST insert happen once; it must not also suppress "+
			"the admission upsert, which is idempotent and is the only thing that makes the post visible in its "+
			"community. Orphaned this way, nothing revisits it: the gate rejects every future delivery (upsert calls: %d)",
		flaky.upsertCalls)
	require.NotNil(t, row)
	assert.Equal(t, posts.AdmissionStatusPending, row.Status)
	assertNullableStringPV2(t, "bafyreiupsertflake", row.EvaluatedCID, "evaluated_cid")

	_, _, _, _, deletedAt := readPV2Post(t, db, uri)
	assert.Nil(t, deletedAt, "the post itself indexed on the first delivery and must be untouched")
}

func TestAdmission_AcceptanceForATombstonedPostIsNotApplied(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newAccFixture(t, db)
	base := time.Now().UnixMicro()

	const cid = "bafyreitombaccept"
	rkey := "tombaccept"
	uri := f.indexPV2(t, rkey, cid, base)

	revs := increasingTIDs(t, 2)
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		accAuthor, "delete", rkey, revs[0], "", base+1_000_000, nil)))

	_, _, _, _, deletedAt := readPV2Post(t, db, uri)
	require.NotNil(t, deletedAt, "fixture: the post must be tombstoned")

	before, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)

	// An acceptance can legitimately arrive after the author deleted the post:
	// the community decided before it saw the tombstone, and the two events are
	// in different repos with no ordering between them. Applying it makes the
	// AppView report `accepted` for content it will never serve — and the
	// host-side sweep, which already ran with the tombstone, will not run again,
	// so the community's repo keeps an acceptance pointing at nothing.
	require.NoError(t, f.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, uri, cid, revs[1], base+2_000_000)),
		"an acceptance for a deleted post is a skip, not a failure: refusing it would dead-letter an event that will "+
			"be replayed and refused identically forever")

	after, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	assert.NotEqualf(t, posts.AdmissionStatusAccepted, after.Status,
		"a tombstoned post was accepted. getStatus would report `accepted` for a post no read path will ever serve, "+
			"and the acceptance record in the community's repo now cites a record nobody can fetch")
	assert.Equalf(t, before.Status, after.Status,
		"the admission of a deleted post must be left exactly as it was")
}

func TestAdmission_RetargetToAnUnknownCommunityIsAWholeEventSkip(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newAccFixture(t, db)
	base := time.Now().UnixMicro()

	const cid = "bafyreiretargetghost"
	rkey := "retargetghost"
	uri := f.indexPV2(t, rkey, cid, base)

	// An update that changes `community` to a DID nobody has indexed. TWO rules
	// meet here and only one of them can be right:
	//
	//   - community immutability (§3.1): discard the WHOLE event, silently, and
	//     never look at anything else it says;
	//   - unknown community (§5.3): an unresolved reference, because the community's
	//     own profile event may simply not have arrived yet.
	//
	// Immutability has to outrank it, and the reason is not aesthetic. The
	// unknown-community branch is UNRESOLVED, so a retarget naming a nonexistent
	// DID would still consume its bounded redrive budget — and it can never
	// succeed, because even once that
	// community exists the event is still an illegal retarget. An author can
	// mint that load at will by editing one field.
	require.NoErrorf(t, f.consumer.HandleEvent(ctx, pv2Event(
		accAuthor, "update", rkey, testkit.TID(), "bafyreiretargeted", base+1_000_000,
		pv2Record("did:plc:accnevercommunity", "retargeted at a ghost", "body"),
	)), "a retarget must be discarded on its own terms, before the community is ever looked up: checking the community "+
		"first turns an invalid event into a retryable one that can never succeed")

	_, communityDID, storedCID, _, _ := readPV2Post(t, db, uri)
	assert.Equal(t, accCommunity, communityDID, "the community must not move")
	assert.Equal(t, cid, storedCID, "the whole event is invalid, so its content must not be applied either")

	assert.Zero(t, countRows(t, db,
		`SELECT count(*) FROM community_post_admissions WHERE post_uri = $1 AND community_did = $2`,
		uri, "did:plc:accnevercommunity"),
		"no admission may be opened for the community an ignored retarget named")
}

func TestAdmission_FailedWithdrawalIsRetriedOnRedelivery(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	sweep := &flakyDeleter{failures: 1, err: errors.New("the community's PDS is briefly unreachable")}
	f := newAccFixture(t, db)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db),
		newMockUserService(), db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithAcceptanceCleanup(sweep),
	)

	base := time.Now().UnixMicro()
	const cid = "bafyreiwithdrawflake"
	rkey := "withdrawflake"
	uri := f.indexPV2(t, rkey, cid, base)

	acceptanceRkey := posts.SubjectRkey(uri)
	_, err := f.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   accCommunity,
		PostURI:        uri,
		AcceptanceURI:  "at://" + accCommunity + "/" + posts.AcceptanceCollection + "/" + acceptanceRkey,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)

	rev := testkit.TID()
	tombstone := pv2Event(accAuthor, "delete", rkey, rev, "", base+1_000_000, nil)

	// First delivery: the tombstone lands (correctly — the author's deletion is
	// the local truth and must not be held hostage by a remote PDS) and the
	// withdrawal fails. The failure is swallowed so the event is not
	// dead-lettered, which is also right, because the redrive would be rejected
	// by the rev gate anyway.
	require.NoError(t, f.consumer.HandleEvent(ctx, tombstone))
	_, _, _, _, deletedAt := readPV2Post(t, db, uri)
	require.NotNil(t, deletedAt, "fixture: the tombstone must land regardless of the sweep")
	require.Len(t, sweep.calls, 1, "fixture: the first withdrawal must have been attempted")

	// The redelivery, with the same rev. Today the gate skip returns before the
	// sweep is reconsidered, so the acceptance is left standing in the
	// community's repo pointing at a record nobody can fetch — permanently,
	// because nothing else revisits it. The community's CAR, the thing its
	// portability argument rests on, now cites content the author withdrew.
	require.NoError(t, f.consumer.HandleEvent(ctx, tombstone))

	assert.Greaterf(t, len(sweep.calls), 1,
		"a withdrawal that failed was never retried (%d attempts). The tombstone is gated so it happens once; the "+
			"withdrawal is idempotent and must be reconsidered on every delivery, or a single unreachable PDS leaves "+
			"the acceptance standing forever with nothing scheduled to fix it", len(sweep.calls))
	assert.Equal(t, uri, sweep.calls[len(sweep.calls)-1].PostURI, "the retry must name the same subject")
}

//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ingesting social.coves.community.postv2 — the post record that now lives in
// the AUTHOR's repo (docs/PRD_AUTHOR_OWNED_POSTS.md §3.1, §5.3).
//
// The collection is new, and so is almost everything about how an event from it
// is read. Three inversions drive every case below:
//
//   - AUTHORSHIP COMES FROM THE REPO, not from the record. There is no `author`
//     field to read and none to trust; event.Did IS the author. The old
//     consumer's central security check — repo DID must equal the record's
//     community — inverts into its opposite: the repo DID must NOT be the
//     community, and the community is a claim the record makes.
//   - AN UNKNOWN AUTHOR IS NORMAL. Open federated posting means the author of a
//     post may be someone this AppView has never indexed, so "no users row" can
//     no longer refuse the event (§5.3, migration 034 dropped the FK).
//   - THE POST ROW IS NO LONGER THE DECISION. Whether a community shows the post
//     lives in community_post_admissions, and a postv2 event's job is to record
//     content plus a pending admission — never to decide anything.
//
// A REFUSAL AND A SKIP ARE DIFFERENT ANSWERS, and several cases turn on which
// one is being asserted. The connector dead-letters exactly what a handler
// returns as an error: nil is "handled, nothing more to do" and never reaches
// the queue; an error wrapped in ErrPermanentEvent is dead-lettered with its
// redrive budget already spent; any other error is dead-lettered retryable.
// "No dead letter" below therefore means a nil return, and it is asserted
// wherever an event must be dropped without the queue growing.

const (
	pv2Prefix    = "did:plc:pv2"
	pv2Community = pv2Prefix + "community"
	pv2Author    = pv2Prefix + "author"
	pv2Other     = pv2Prefix + "otherauthor"
)

// pv2Fixture is a wired consumer plus the stores the assertions read.
type pv2Fixture struct {
	consumer   *PostEventConsumer
	admissions posts.AdmissionRepository
	db         *sql.DB
	users      *mockUserService
}

// newPV2Fixture indexes a community and returns a consumer wired with the three
// collaborators author-owned ingestion needs, all real: the admissions store and
// the deleted-account lookup both run against this test's Postgres clone, so a
// gate that reads the wrong table fails here rather than passing against a map.
func newPV2Fixture(t *testing.T, db *sql.DB) pv2Fixture {
	t.Helper()

	insertBridgedUser(t, db, pv2Author, "pv2author.test")
	insertBridgedCommunity(t, db, pv2Community, "pv2community.test", pv2Author)

	us := newMockUserService()
	us.users[pv2Author] = &users.User{DID: pv2Author, Handle: "pv2author.test"}

	admissions := postgres.NewAdmissionRepository(db)
	consumer := NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		us,
		db,
		WithAdmissions(admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
	)

	return pv2Fixture{consumer: consumer, admissions: admissions, db: db, users: us}
}

// pv2URI is the AT-URI of an author-repo post: the author's DID is the
// authority, which is the whole point of the flip.
func pv2URI(authorDID, rkey string) string {
	return "at://" + authorDID + "/" + PostV2Collection + "/" + rkey
}

// pv2Record builds a postv2 record body. It carries NO author field, by
// construction — the lexicon has none (§3.1), and a consumer that still read one
// would be reading a field only a forger would bother to send.
func pv2Record(communityDID, title, content string) map[string]interface{} {
	return map[string]interface{}{
		"$type":     PostV2Collection,
		"community": communityDID,
		"title":     title,
		"content":   content,
		"createdAt": "2026-03-01T00:00:00Z",
	}
}

// pv2Event builds a commit event in the AUTHOR's repo.
func pv2Event(authorDID, op, rkey, rev, cid string, timeUS int64, record map[string]interface{}) *JetstreamEvent {
	return revCommitEvent(authorDID, PostV2Collection, op, rkey, rev, cid, timeUS, record)
}

// readPV2Post returns the indexed row's identity columns, or fails naming the
// URI that is missing.
func readPV2Post(t *testing.T, db *sql.DB, uri string) (authorDID, communityDID, cid, title string, deletedAt *time.Time) {
	t.Helper()
	err := db.QueryRow(
		`SELECT author_did, community_did, cid, title, deleted_at FROM posts WHERE uri = $1`, uri,
	).Scan(&authorDID, &communityDID, &cid, &title, &deletedAt)
	require.NoErrorf(t, err, "no post row for %s", uri)
	return authorDID, communityDID, cid, title, deletedAt
}

func countRows(t *testing.T, db *sql.DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// markAccountDeleted writes the migration-036 erasure marker directly, which is
// what userRepo.Delete leaves behind.
func markAccountDeleted(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW()) ON CONFLICT (did) DO NOTHING`, did)
	require.NoErrorf(t, err, "marking %s deleted", did)
}

func TestPostV2Consumer_Create_IndexesTheAuthorsPostAndOpensAPendingAdmission(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	const cid = "bafyreipv2create"
	rkey := "pv2create"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, testkit.TID(), cid, time.Now().UnixMicro(),
		pv2Record(pv2Community, "an author-signed post", "words the author is accountable for"),
	)))

	authorDID, communityDID, storedCID, title, deletedAt := readPV2Post(t, db, uri)

	// The author is the repo, not a field. If this ever reads from the record
	// instead, any repo can claim any author — which is exactly the
	// impersonation power the flip removed (§1).
	assert.Equal(t, pv2Author, authorDID,
		"author_did must come from event.Did: the record has no author field, and deriving one from anywhere else restores the forgery the flip removed")
	assert.Equal(t, pv2Community, communityDID,
		"community_did comes from the record — it is the author's submission target, a claim the community has not yet agreed to")
	assert.Equal(t, cid, storedCID)
	assert.Equal(t, "an author-signed post", title)
	assert.Nil(t, deletedAt)

	admission, err := f.admissions.Get(ctx, pv2Community, uri)
	require.NoErrorf(t, err, "indexing a postv2 must open the admission row the community will decide against")
	require.NotNil(t, admission)

	// PENDING, not accepted. The post claims the community; the community has
	// said nothing. §2 is explicit that a post lacking an acceptance is never
	// shown in that community, and an indexer that opened the row as anything
	// else would publish speech the community never agreed to carry.
	assert.Equal(t, posts.AdmissionStatusPending, admission.Status)
	assertNullableStringPV2(t, cid, admission.EvaluatedCID,
		"evaluated_cid must be the CID this event carried: it is what the next decision judges and what an acceptance's pinned CID is compared against")

	// An author-repo event orders by the per-record rev gate, never by the
	// community watermark. Stamping one here would let an author's edit outrank
	// a moderator's removal — two repos, two unrelated revision clocks (§5.2).
	assert.Nilf(t, admission.LastCommunityEvent,
		"an author-repo event must not advance the community watermark; got %+v", admission.LastCommunityEvent)
}

func TestPostV2Consumer_DeletedAuthor_IsDroppedWithoutADeadLetter(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	// The account was erased. Migration 036's marker is the only thing that says
	// so: the users row is gone, and under §5.3 a missing users row means
	// "federated author we have not indexed", which indexes normally. Without
	// the marker this event silently re-creates the content the deletion swept —
	// and it WILL arrive, because dead-letter redrives and overlapping feeds
	// replay events long after the account is gone.
	markAccountDeleted(t, db, pv2Other)

	rkey := "pv2deleted"
	uri := pv2URI(pv2Other, rkey)

	err := f.consumer.HandleEvent(ctx, pv2Event(
		pv2Other, "create", rkey, testkit.TID(), "bafyreipv2deleted", time.Now().UnixMicro(),
		pv2Record(pv2Community, "a post from an erased account", "content the AppView was asked to forget"),
	))

	// Nil, not an error. The connector dead-letters whatever a handler returns,
	// so refusing this event with an error would fill the queue with rows that
	// redrive, fail identically, and retire — turning every erased account into
	// a permanent stream of operational noise. The event is not a failure; it is
	// an event with nothing to do.
	require.NoError(t, err,
		"an event from an erased account must be dropped as a no-op: returning an error dead-letters it, and the queue exists for failures, not for events the AppView correctly ignores")

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"the post of an erased account must not be indexed")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, uri),
		"no admission row either: an admission for an erased account's post is the row migration 036 exists to stop being recreated")
}

func TestPostV2Consumer_UnknownAuthorIndexesAnyway(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	// The §5.3 flip, and the case that separates "erased" from "never seen".
	// pv2Other has no users row and no erasure marker — the ordinary state of an
	// author on someone else's server. The old consumer refused this event, which
	// the test architecture recorded as "federated authors cannot currently be
	// indexed"; open federated posting makes that refusal a bug.
	rkey := "pv2unknown"
	uri := pv2URI(pv2Other, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Other, "create", rkey, testkit.TID(), "bafyreipv2unknown", time.Now().UnixMicro(),
		pv2Record(pv2Community, "a post from a federated stranger", "posted from a PDS we have never met"),
	)), "an author this AppView has never indexed must not block ingestion: that refusal is what made cross-server posting impossible")

	authorDID, _, _, _, _ := readPV2Post(t, db, uri)
	assert.Equal(t, pv2Other, authorDID)

	admission, err := f.admissions.Get(ctx, pv2Community, uri)
	require.NoError(t, err)
	require.NotNil(t, admission)
	assert.Equal(t, posts.AdmissionStatusPending, admission.Status)
}

func TestPostV2Consumer_UnknownCommunity_IsUnresolvedAndRedrivable(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	const ghostCommunity = "did:plc:pv2ghostcommunity"
	rkey := "pv2ghost"

	err := f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2ghost", time.Now().UnixMicro(),
		pv2Record(ghostCommunity, "aimed at a community we have not indexed", "body"),
	))

	require.Error(t, err, "a post naming a community this AppView has never indexed cannot open an admission row against it")

	// The classification is the assertion. BigSky preserves order within a repo,
	// not across repos, so a post can genuinely arrive before the community's own
	// profile event — this is an ORDERING failure, and marking it permanent would
	// discard every post that merely arrived early, with the redrive that would
	// have fixed it already spent.
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"community-not-found is an ordering failure and must stay redrivable once the community arrives")
	assert.ErrorIs(t, err, ErrUnresolvedReference,
		"the ordering failure must bypass in-line retries and move directly to the redriver")

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, pv2URI(pv2Author, rkey)),
		"the refused post must not have been indexed")
}

func TestPostV2Consumer_UpdateChangingCommunity_IgnoresTheWholeEvent(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	otherName := "pv2secondcommunity"
	const secondCommunity = pv2Prefix + "community2"
	insertBridgedCommunity(t, db, secondCommunity, otherName+".test", pv2Author)

	rkey := "pv2retarget"
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2original", base,
		pv2Record(pv2Community, "original title", "original body"),
	)))

	// The retarget attempt: same record, new community, and new content riding
	// along. §3.1 says the ENTIRE event is invalid — discard it, do not merely
	// keep the old community value. Applying the content while ignoring the
	// community would leave the first community's admission holding a CID it
	// never evaluated, silently publishing content nobody judged under a
	// standing acceptance.
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2retargeted", base+1_000_000,
		pv2Record(secondCommunity, "retargeted title", "retargeted body"),
	)), "an update that changes community is invalid, not an infrastructure failure: it must be skipped, not dead-lettered")

	_, communityDID, cid, title, _ := readPV2Post(t, db, uri)
	assert.Equal(t, pv2Community, communityDID, "the community must not move; retargeting a post means writing a new record")
	assert.Equalf(t, "bafyreipv2original", cid,
		"the whole event is invalid, so the CID must not move either — a moved CID under an unmoved community is content the community never evaluated")
	assert.Equal(t, "original title", title, "the content half of a rejected event must be rejected with it")

	assert.Zero(t, countRows(t, db,
		`SELECT count(*) FROM community_post_admissions WHERE post_uri = $1 AND community_did = $2`, uri, secondCommunity),
		"an ignored retarget must not open an admission in the community it named: that row would be a dangling decision about a post that never claimed this community")
}

func TestPostV2Consumer_UpdateWithNewContent_ReopensAnAcceptedAdmission(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	rkey := "pv2edit"
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	const originalCID = "bafyreipv2editv1"
	const editedCID = "bafyreipv2editv2"

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], originalCID, base,
		pv2Record(pv2Community, "before the edit", "the version the community judged"),
	)))

	// The community accepts the original, pinning that exact CID.
	acceptanceRkey := testkit.TID()
	accepted, err := f.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   pv2Community,
		PostURI:        uri,
		AcceptanceURI:  "at://" + pv2Community + "/social.coves.community.acceptance/" + acceptanceRkey,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      originalCID,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome, "fixture: the acceptance must stand before the edit arrives")

	// The author edits. The standing acceptance now pins content that is no
	// longer current, and §5.5 forbids rendering the new CID under it.
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], editedCID, base+1_000_000,
		pv2Record(pv2Community, "after the edit", "words the community has not seen"),
	)))

	_, _, storedCID, title, _ := readPV2Post(t, db, uri)
	assert.Equal(t, editedCID, storedCID, "the edit's content must be indexed — it is what the community will re-judge")
	assert.Equal(t, "after the edit", title)

	admission, err := f.admissions.Get(ctx, pv2Community, uri)
	require.NoError(t, err)
	require.NotNil(t, admission)

	assert.Equal(t, posts.AdmissionStatusPendingReacceptance, admission.Status,
		"an edit under a standing acceptance must reopen the decision: auto-rendering the new CID under the old acceptance would let an author swap content past moderation after approval")
	assertNullableStringPV2(t, editedCID, admission.EvaluatedCID,
		"evaluated_cid must follow the content, or the re-decision judges the version the author replaced")
	assertNullableStringPV2(t, originalCID, admission.AcceptedCID,
		"the acceptance still pins the CID the community actually agreed to; moving it here would forge agreement to the edit")
}

func TestPostV2Consumer_EditOfARemovedPost_IsSkippedNotDeadLettered(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	rkey := "pv2removededit"
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2removedv1", base,
		pv2Record(pv2Community, "will be removed", "body"),
	)))

	// The precondition is asserted, not assumed. ApplyRemoval below creates the
	// admission row when the subject is absent, so without this the whole test
	// would pass against a consumer that ignores postv2 entirely — the edit
	// would be a no-op for the wrong reason and the final assertion would read
	// back a row nothing had ever contested.
	_, _, _, _, deletedAt := readPV2Post(t, db, uri)
	require.Nil(t, deletedAt, "fixture: the post must be indexed and live before the removal lands")

	removed, err := f.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: pv2Community,
		PostURI:      uri,
		DecisionCode: string(posts.DecisionRuleViolation),
		Watermark:    posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, removed.Outcome, "fixture: the removal must stand")

	// §5.5: removal is terminal against author-repo events. The repository
	// answers this edit with skipped_terminal — a value, not an error — and the
	// consumer must pass that through as success. Mapping a CAS skip onto an
	// error return is the mistake migration 033's precedent exists to prevent:
	// it routes the system WORKING into the dead-letter queue, where every
	// redrive re-runs a decision that will refuse identically forever.
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2removedv2", base+1_000_000,
		pv2Record(pv2Community, "edited while removed", "laundering attempt"),
	)), "a terminal admission skip is an outcome, not a failure: returning an error would dead-letter healthy skips")

	admission, err := f.admissions.Get(ctx, pv2Community, uri)
	require.NoError(t, err)
	require.NotNil(t, admission)
	assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status,
		"editing a removed post must not reopen it; that is how a removed post gets laundered back through auto-acceptance")
}

func TestPostV2Consumer_Delete_TombstonesTheRow(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	rkey := "pv2delete"
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2delete", base,
		pv2Record(pv2Community, "to be deleted by its author", "body that must survive the tombstone"),
	)))

	// A delete carries no record, exactly as Jetstream delivers it.
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil,
	)))

	_, _, _, title, deletedAt := readPV2Post(t, db, uri)
	require.NotNil(t, deletedAt,
		"an author delete must SOFT-delete: the row is the rev gate's tombstone, the comment thread's parent, and what moderation still reads")
	assert.Equal(t, "to be deleted by its author", title,
		"a soft delete must not blank the content")

	// The host-side half — the community observing the tombstone and deleting
	// its acceptance (§5.3) — is asserted separately below, since it only runs
	// on the instance that HOSTS the community.
}

// recordingAcceptanceDeleter is the host-side sweep, observed rather than
// performed.
//
// A fake here and not a real community repo, deliberately: what the CONSUMER
// owes is that it asks for the right subject, exactly once, and only when it
// should. Whether the ask reaches the PDS correctly — the shaped delete, the
// skip on a missing record, the committed rev — is the writer's own contract
// and is proven against a real PDS in
// internal/core/posts/acceptance_delete_test.go. Wiring a credentialed
// community in here would re-prove that and make this test unable to say
// anything about the case that matters most: the sweep NOT firing.
type recordingAcceptanceDeleter struct {
	calls []posts.CommunityAcceptanceDeleteCommand
	err   error
}

func (d *recordingAcceptanceDeleter) DeleteAcceptance(
	_ context.Context, cmd posts.CommunityAcceptanceDeleteCommand,
) (posts.CommunityWriteResult, error) {
	d.calls = append(d.calls, cmd)
	if d.err != nil {
		return posts.CommunityWriteResult{}, d.err
	}
	return posts.CommunityWriteResult{Rev: testkit.TID()}, nil
}

func TestPostV2Consumer_Delete_WithdrawsTheHostedCommunitysAcceptance(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	sweep := &recordingAcceptanceDeleter{}
	f := newPV2Fixture(t, db)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		f.users,
		db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithAcceptanceCleanup(sweep),
	)

	rkey := "pv2sweep"
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	const cid = "bafyreipv2sweep"
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], cid, base,
		pv2Record(pv2Community, "accepted, then withdrawn by its author", "body"),
	)))

	acceptanceRkey := testkit.TID()
	accepted, err := f.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   pv2Community,
		PostURI:        uri,
		AcceptanceURI:  "at://" + pv2Community + "/social.coves.community.acceptance/" + acceptanceRkey,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome, "fixture: an acceptance must stand for the sweep to withdraw")

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil,
	)))

	// The acceptance points at a record nobody can fetch now. The community repo
	// is the curated index the whole portability argument rests on, so leaving
	// it standing means the CAR permanently cites content the author withdrew,
	// and a peer replaying it shows a post that no longer exists.
	require.Lenf(t, sweep.calls, 1,
		"the tombstone must trigger exactly one acceptance withdrawal; got %d", len(sweep.calls))
	assert.Equal(t, pv2Community, sweep.calls[0].CommunityDID)
	assert.Equal(t, uri, sweep.calls[0].PostURI,
		"the sweep must name the tombstoned post; the acceptance rkey is derived from this URI, so a wrong subject deletes a different post's acceptance")
}

func TestPostV2Consumer_Delete_DoesNotSweepWhenNoAcceptanceStands(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	sweep := &recordingAcceptanceDeleter{}
	f := newPV2Fixture(t, db)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		f.users,
		db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithAcceptanceCleanup(sweep),
	)

	rkey := "pv2nosweep"
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	// Indexed and pending: the community never accepted it, so there is nothing
	// in its repo to withdraw. This is the COMMON case — most posts a community
	// sees were never accepted by it — and a sweep that fired anyway would put
	// one pointless authenticated PDS round trip behind every delete event on
	// the network.
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2nosweep", base,
		pv2Record(pv2Community, "never accepted", "body"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil,
	)))

	assert.Emptyf(t, sweep.calls,
		"the sweep fired for a post that was never accepted (%d calls): the admission row is the AppView's own record of whether an acceptance stands, "+
			"and consulting it is what keeps this from being a PDS round trip per delete event", len(sweep.calls))
}

func TestPostV2Consumer_Delete_SurvivesAFailedSweep(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	sweep := &recordingAcceptanceDeleter{err: errors.New("the community's PDS is unreachable")}
	f := newPV2Fixture(t, db)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		f.users,
		db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithAcceptanceCleanup(sweep),
	)

	rkey := "pv2sweepfail"
	uri := pv2URI(pv2Author, rkey)
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)

	const cid = "bafyreipv2sweepfail"
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], cid, base,
		pv2Record(pv2Community, "the sweep will fail", "body"),
	)))
	acceptanceRkey := testkit.TID()
	_, err := f.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   pv2Community,
		PostURI:        uri,
		AcceptanceURI:  "at://" + pv2Community + "/social.coves.community.acceptance/" + acceptanceRkey,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)

	deleteErr := f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil,
	))

	// THE TOMBSTONE IS THE LOCAL TRUTH AND MUST LAND REGARDLESS. The author
	// asked for their post to be gone; a community PDS that cannot be reached
	// must not keep this AppView serving it. The acceptance withdrawal is
	// best-effort cleanup of a REMOTE repo, and the engine's own passes revisit
	// it — so the only question here is whether a failed sweep can hold the
	// deletion hostage.
	_, _, _, _, deletedAt := readPV2Post(t, db, uri)
	require.NotNilf(t, deletedAt,
		"the post was not tombstoned because the acceptance sweep failed: an unreachable community PDS would keep this AppView serving content its author deleted (sweep error: %v)", deleteErr)
}

// assertNullableStringPV2 asserts a nullable column holds exactly want.
//
// Named apart from the postgres package's helper of the same shape because this
// package has its own; the duplication is two lines against an import cycle.
func assertNullableStringPV2(t *testing.T, want string, got *string, what string) {
	t.Helper()
	if !assert.NotNilf(t, got, "%s: want %q, got NULL", what, want) {
		return
	}
	assert.Equalf(t, want, *got, "%s", what)
}

// increasingTIDs returns n real atProto TIDs whose lexicographic order is their
// generation order — what a repo's successive commits actually carry, and what
// the rev gate compares. Invented revs would prove the gate works on invented
// data.
func increasingTIDs(t *testing.T, n int) []string {
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

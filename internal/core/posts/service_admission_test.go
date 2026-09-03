//go:build integration

package posts_test

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The outer contract for admission: what CreatePost does when the admission
// policy of docs/PRD_AUTHOR_OWNED_POSTS.md §4.1/§8 refuses a submission.
//
// The decision itself is covered at width in admit_matrix_test.go against
// fakes. What is unproven without these is the WIRING, and the wiring is where
// this kind of check historically goes wrong:
//
//   - that CreatePost consults the policy AT ALL. §4.1 corrects rev 1 of the
//     spec on exactly this point — the service docstring has claimed
//     "membership/ban validation" since the beginning and no ban lookup has
//     ever existed on the write path.
//   - that it consults it BEFORE the PDS write, so a refusal leaves no record
//     in a community that refused it.
//   - that the ledger rows the quota is counted against are the ones CreatePost
//     itself writes. Like the aggregator quota (service_aggregator_test.go),
//     the producer and the consumer of that counter are the same code path, and
//     a service that never wrote them would pass every unit test and never rate
//     limit anything.
//   - that a failed PDS write RELEASES the row it reserved. This is the one
//     behaviour no fake can prove, because it is about what survives in the
//     database after a write that did not happen.
//
// ON SEEDING is_banned DIRECTLY. Nothing in production writes
// community_memberships at all today — not CreateMembership, not
// UpdateMembership; both are reachable only from tests. There is no ban
// endpoint, no moderation consumer, and no firehose path that sets the column.
// So these tests seed it through the repository, and that is an honest
// admission of an incomplete feature rather than a shortcut: §4.1 specifies the
// ban LOOKUP, and the record type that will eventually write it
// (social.coves.moderation.ban) is not this task's work. When it lands, this
// seeding becomes the moderation path and these assertions do not change.

// admissionFixture is the post service wired with the §8 admission policy over
// real Postgres, a real PDS, and a clock the test controls.
type admissionFixture struct {
	base    *postFixture
	service posts.Service
	repo    communities.Repository
	clock   *testClock
	limits  posts.SubmissionLimits
}

// testClock is the injected Clock. Time moves only when a test moves it, which
// is what lets a rolling window be crossed without a sleep — docs/
// TEST_ARCHITECTURE.md §3.3 bans the alternative outright.
//
// It is mutex-guarded because CreatePost may read it from more than one
// goroutine, and the race detector is on in CI.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newAdmissionFixture provisions a community on the test PDS and points a
// policy-wired post service at it.
//
// The limit is deliberately small. Each admitted submission costs a real
// createRecord against a real PDS, and the assertion that matters is the
// BOUNDARY — N admitted, N+1 refused — which three proves exactly as well as
// three hundred would.
func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()

	base := newPostFixture(t)
	limits := posts.SubmissionLimits{
		MaxPerAuthorPerCommunity: 3,
		Window:                   time.Hour,
		DedupeWindow:             time.Hour,
	}

	// Anchored at the real present rather than at a fixed date: post_submissions
	// stamps created_at server-side (NOW()), while the rolling-window query is
	// computed from THIS clock, so the two have to agree about roughly when
	// "now" is. Advancing forwards is always safe — it ages real rows out of the
	// window, which is the direction every test here moves.
	clock := &testClock{now: time.Now().UTC()}

	return &admissionFixture{
		base: base,
		service: posts.NewPostService(
			postgres.NewPostRepository(base.db), base.communityService,
			nil, nil, nil, nil, base.pds.URL(),
			// The write path needs the author's own credentials now (§4.2): a
			// post is written to the AUTHOR's repo, so a service wired without
			// the factory could not write one at all and every refusal here
			// would pass for the wrong reason.
			append(base.writePathOptions(),
				posts.WithAdmissionPolicy(posts.AdmissionPolicy{
					Ledger: postgres.NewSubmissionLedger(base.db),
					Bans:   base.communityService,
					Limits: limits,
					Now:    clock.Now,
				}))...),
		repo:   postgres.NewCommunityRepository(base.db, credentialciphertest.Fixed()),
		clock:  clock,
		limits: limits,
	}
}

// submit posts as the fixture's author. Unlike postFixture.createPost it
// returns the error, because every test here is about a refusal.
func (f *admissionFixture) submit(t *testing.T, communityDID, title string) (*posts.CreatePostResponse, error) {
	t.Helper()

	content := "a body that makes this a complete post"
	return f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), f.base.author.DID),
		sessionFor(t, f.base.author, f.base.pds.URL()),
		posts.CreatePostRequest{
			Community: communityDID,
			Title:     &title,
			Content:   &content,
			AuthorDID: f.base.author.DID,
		})
}

// setBanned seeds (or updates) the author's membership of the fixture's
// community with the given ban state.
func (f *admissionFixture) setBanned(t *testing.T, banned bool) {
	t.Helper()

	ctx := context.Background()
	membership := &communities.Membership{
		UserDID:      f.base.author.DID,
		CommunityDID: f.base.community.DID,
		JoinedAt:     time.Now().UTC(),
		LastActiveAt: time.Now().UTC(),
		IsBanned:     banned,
	}

	if _, err := f.repo.GetMembership(ctx, f.base.author.DID, f.base.community.DID); err != nil {
		require.ErrorIs(t, err, communities.ErrMembershipNotFound)
		_, createErr := f.repo.CreateMembership(ctx, membership)
		require.NoError(t, createErr)
		return
	}

	_, err := f.repo.UpdateMembership(ctx, membership)
	require.NoError(t, err)
}

// ledgerRows counts what the submission ledger holds for the author in one
// community — the number the quota is enforced against.
//
// Read with raw SQL rather than through the repository on purpose: the
// repository is the thing under test here, and a test that asked it to report
// its own state would pass against an implementation that recorded nothing.
func (f *admissionFixture) ledgerRows(t *testing.T, communityDID string) int {
	t.Helper()
	return countSubmissions(t, f.base.db, f.base.author.DID, communityDID)
}

func countSubmissions(t *testing.T, db *sql.DB, authorDID, communityDID string) int {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM post_submissions WHERE author_did = $1 AND community_did = $2
	`, authorDID, communityDID).Scan(&count),
		"the post_submissions ledger (migration 035) must exist for the quota to be countable")
	return count
}

// anotherCommunity provisions a second community on the same PDS, so that a
// per-community rule can be shown to be per-community.
func (f *admissionFixture) anotherCommunity(t *testing.T) *communities.Community {
	t.Helper()

	name := testkit.UniqueIDWithPrefix(t, "ad")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := f.base.communityService.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:         name,
		DisplayName:  "Somewhere else",
		Description:  "a second community, to prove the quota is scoped to one",
		Visibility:   "public",
		CreatedByDID: f.base.author.DID,
	})
	require.NoError(t, err)
	return community
}

// ---------------------------------------------------------------------------

// A ban stops the next post, and lifting it lets the author back in.
//
// Both halves matter. A ban that could not be lifted would be a data-loss bug
// dressed as a moderation feature, and it is the kind that only shows up when a
// moderator tries to undo a mistake.
func TestService_ABannedMemberIsRefusedAndAnUnbanRestoresThem(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	// Before the ban, the author is an ordinary poster in a public community.
	_, err := f.submit(t, f.base.community.DID, "posted while in good standing")
	require.NoError(t, err)

	f.setBanned(t, true)

	_, err = f.submit(t, f.base.community.DID, "posted after the ban")
	require.Error(t, err)
	assert.ErrorIsf(t, err, posts.ErrBanned,
		"the handler maps this sentinel to a 403 Banned, so the sentinel identity is the contract; got: %v", err)

	// Refused means nothing was written and nothing was billed. The check runs
	// ahead of the PDS write; an implementation that reordered them would leave
	// a banned author's post in the community's repository, from which the
	// firehose would index it before anything noticed.
	assert.Equal(t, 1, f.ledgerRows(t, f.base.community.DID),
		"the refused submission consumed quota it was never granted")

	f.setBanned(t, false)

	_, err = f.submit(t, f.base.community.DID, "posted after the unban")
	assert.NoError(t, err, "lifting a ban must take effect on the next post, not on the next restart")
}

// The per-author quota: N admitted, N+1 refused, and the limit is per
// community.
//
// Asserting only that "the fourth fails" would pass against an implementation
// that refused the third too, which is why every submission inside the quota is
// individually required to succeed.
func TestService_TheAuthorQuotaStopsTheNextSubmission(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	for i := 0; i < f.limits.MaxPerAuthorPerCommunity; i++ {
		_, err := f.submit(t, f.base.community.DID, fmt.Sprintf("submission %d", i))
		require.NoErrorf(t, err, "submission %d of %d was refused inside the quota",
			i+1, f.limits.MaxPerAuthorPerCommunity)
	}
	require.Equal(t, f.limits.MaxPerAuthorPerCommunity, f.ledgerRows(t, f.base.community.DID))

	_, err := f.submit(t, f.base.community.DID, "one submission too many")
	require.Error(t, err)
	assert.ErrorIsf(t, err, posts.ErrRateLimitExceeded,
		"the handler maps this to a 429; got: %v", err)

	// A refused submission is not billed, so an author cannot spend past their
	// quota by ignoring the error — and, more to the point, cannot extend their
	// own lockout by retrying.
	assert.Equal(t, f.limits.MaxPerAuthorPerCommunity, f.ledgerRows(t, f.base.community.DID))

	// The same author, at their limit here, is unaffected there. A quota that
	// leaked across communities would let one busy community silence its author
	// everywhere on the instance.
	elsewhere := f.anotherCommunity(t)
	_, err = f.submit(t, elsewhere.DID, "a submission somewhere else")
	assert.NoError(t, err, "the quota is per (author, community); being at the limit in one must not close the others")
}

// An identical resubmission is a repeat, not a new post — even when the retry
// names the community by DID and the original named it by handle.
//
// The canonical case is a client that retried after a lost response, and the
// answer has to be distinguishable from a quota breach: 409 tells the client its
// post already exists, 429 tells it to wait. A submission refused as a duplicate
// must also not be billed, or a flaky connection would rate-limit a user who
// posted once. Submitting first by HANDLE and retrying by DID is the identifier
// dodge the fingerprint must not fall for: the ledger scopes dedupe by the
// RESOLVED community DID, so the client-typed spelling must not enter the key.
func TestService_AnIdenticalResubmissionIsRefusedAsADuplicate(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	_, err := f.submit(t, f.base.community.Handle, "the very same post")
	require.NoError(t, err, "submitting by handle must resolve and admit like submitting by DID")

	_, err = f.submit(t, f.base.community.DID, "the very same post")
	require.Error(t, err)
	assert.ErrorIsf(t, err, posts.ErrDuplicateSubmission,
		"the handler maps this to a 409 DuplicateSubmission; got: %v", err)

	assert.Equal(t, 1, f.ledgerRows(t, f.base.community.DID),
		"the duplicate must be refused without adding a second ledger row")

	// And a genuinely different post from the same author still goes through:
	// the refusal is about identical content, not about having posted recently.
	_, err = f.submit(t, f.base.community.DID, "a different post entirely")
	assert.NoError(t, err)
}

// A PDS write that fails must give the reservation back.
//
// The ledger row goes in BEFORE the record is written — that ordering is what
// closes the concurrent double-tap, since the unique constraint is the only
// arbiter that both racing requests can agree on. The cost of that choice is
// that the failure path owes the author their slot back. If it does not pay,
// every PDS hiccup permanently consumes one submission from the author's quota
// AND blocks them from retrying the same content at all, which turns a
// transient outage into a per-user lockout that outlives it.
func TestService_AFailedPDSWriteReleasesTheReservation(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	// A PDS that refuses every write. Pointing the AUTHOR's repo client at it is
	// how the failure is injected, because the write goes to the author's repo
	// now (§4.2 step 3) — the community's stored pds_url, which this test used to
	// break, is no longer on the create path at all. It is still the real write
	// failing for a real reason rather than a stubbed-out client.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"InternalServerError"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	f.base.authorRepos.pointAt(f.base.author.DID, broken.URL)

	const repeatable = "a post whose write will fail the first time"
	_, err := f.submit(t, f.base.community.DID, repeatable)
	require.Error(t, err, "the PDS refused the write, so CreatePost must report a failure")

	assert.Zerof(t, f.ledgerRows(t, f.base.community.DID),
		"the reservation for a post that was never written is still on the ledger: it has burned a quota slot and will refuse the retry as a duplicate")

	f.base.authorRepos.pointAt(f.base.author.DID, "")

	// The retry a client would actually send: byte-identical content. It must be
	// admitted, which is only possible if the reservation was released.
	resp, err := f.submit(t, f.base.community.DID, repeatable)
	require.NoErrorf(t, err, "the identical retry after a failed write was refused, so the failure path leaked its reservation")
	require.NotEmpty(t, resp.URI)

	assert.Equal(t, 1, f.ledgerRows(t, f.base.community.DID),
		"exactly one submission survived: the failed attempt released its row and the retry took a fresh one")

	// The record really is in the AUTHOR's repo, so "admitted" here means a post
	// exists rather than merely that no error came back. The repo flipped in
	// task 6 (§3.1); what this assertion is for did not.
	record := f.base.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	assert.Equal(t, f.base.community.DID, record.Value["community"])
}

// A client that goes away MID-WRITE must still get its reservation back.
//
// The failure path runs on the same context the request came in on, and by the
// time the release runs that context is already dead — the canceled write is
// exactly why the path was taken. A release issued on the caller's context
// would be refused by Postgres as canceled too, and the leak would be
// invisible: the request already failed, the log line is a warning, and the
// author discovers it as a duplicate refusal of a post that does not exist.
// The release must therefore run detached from the caller's cancellation
// (precedent: adminreports raiseAlert), bounded by its own timeout.
func TestService_ACancellationDuringThePDSWriteStillReleasesTheReservation(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	// The client's context, canceled by the "PDS" at the exact moment the
	// write is in flight — the request-scoped context is dead by the time
	// CreatePost's failure path runs, which is the shape of a client
	// disconnecting mid-request.
	ctx, cancel := context.WithCancel(middleware.SetTestUserDID(context.Background(), f.base.author.DID))
	defer cancel()

	canceling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Kill the caller's context while its write is in flight, then refuse
		// the write. Whether CreatePost's failure surfaces as the canceled
		// context or as the 500 is an interleaving detail; cancel() happens
		// before the response is written, so by the time the failure path runs
		// the request's context is dead either way.
		cancel()
		http.Error(w, `{"error":"InternalServerError"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(canceling.Close)

	f.base.authorRepos.pointAt(f.base.author.DID, canceling.URL)

	const repeatable = "a post whose client disconnects mid-write"
	content := "a body that makes this a complete post"
	_, err := f.service.CreatePost(ctx, sessionFor(t, f.base.author, f.base.pds.URL()), posts.CreatePostRequest{
		Community: f.base.community.DID,
		Title:     func() *string { s := repeatable; return &s }(),
		Content:   &content,
		AuthorDID: f.base.author.DID,
	})
	require.Error(t, err, "the write ran against a dead context and must fail")

	assert.Zerof(t, f.ledgerRows(t, f.base.community.DID),
		"the release ran on the caller's canceled context and was refused with it: the reservation leaked, burning a quota slot and blocking the retry as a duplicate")

	f.base.authorRepos.pointAt(f.base.author.DID, "")

	// The retry a reconnected client sends: byte-identical content on a live
	// context. Admissible only if the canceled attempt released its row.
	resp, err := f.submit(t, f.base.community.DID, repeatable)
	require.NoError(t, err, "the identical retry was refused, so the canceled attempt leaked its reservation")
	require.NotEmpty(t, resp.URI)
	assert.Equal(t, 1, f.ledgerRows(t, f.base.community.DID))
}

// RETIRED: TestService_ATokenRefreshFailureReleasesTheReservation.
//
// It pinned that a failed community-token refresh releases the ledger
// reservation, which was a correct property of the PRE-FLIP write path — a post
// went into the COMMUNITY's repo, so that token was the credential the post
// could not be written without.
//
// It is not a correct property any more, and keeping it was actively harmful.
// A post is written to the AUTHOR's repo now (§4.2 step 3); the community's
// token is read only for a link preview's thumbnail, which has since moved to
// the author's repo as well, leaving the step with nothing to buy. Worse, the
// step does not merely idle: communities.NeedsRefresh cannot parse the EMPTY
// access token that every firehose-indexed community carries, so it ERRORS for
// exactly the remote communities §4.2 step 5 is about — turning the pending
// flow the flip exists to deliver into a 500. This test was the only thing
// requiring that step to exist, so the test had to go before the step could.
//
// THE PROPERTY IT GUARDED IS NOT LOST. "A failure after admission and before the
// record exists releases the reservation" is pinned three other ways, each on a
// step that is still real: TestService_AFailedPDSWriteReleasesTheReservation
// (the author-repo write), TestService_ACancellationDuringThePDSWriteStill-
// ReleasesTheReservation (a dead caller context), and the embed-guard release
// case below. Retiring this one removes a redundant assertion about a step that
// should not exist, not a unique one about a step that should.
//
// Its expiredJWT helper went with it: nothing else in this package forces a
// token refresh, and a helper kept "in case" is how a retired path grows a
// second test later.

// An embed-enhancement failure (step 7) also runs with the reservation held.
// The thumb-must-be-a-blob guard is the reachable failure in that step without
// a network: it refuses the submission after admission, so the refusal must
// hand the slot back or the author's corrected retry meets a quota they never
// spent.
func TestService_AnEmbedEnhancementFailureReleasesTheReservation(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	title := "a link post with a malformed thumbnail"
	content := "a body that makes this a complete post"
	submitWithThumb := func(thumb interface{}) (*posts.CreatePostResponse, error) {
		external := map[string]interface{}{
			"uri":         "https://example.com/article",
			"title":       "An article",
			"description": "worth reading",
		}
		if thumb != nil {
			external["thumb"] = thumb
		}
		return f.service.CreatePost(
			middleware.SetTestUserDID(context.Background(), f.base.author.DID),
			sessionFor(t, f.base.author, f.base.pds.URL()),
			posts.CreatePostRequest{
				Community: f.base.community.DID,
				Title:     &title,
				Content:   &content,
				AuthorDID: f.base.author.DID,
				Embed: map[string]interface{}{
					"$type":    "social.coves.embed.external",
					"external": external,
				},
			})
	}

	// A thumb sent as a URL string passes the lexicon-shape validation of step
	// 1 and is refused by the blob guard in step 7 — after admission.
	_, err := submitWithThumb("https://example.com/thumb.jpg")
	require.Error(t, err)
	require.True(t, posts.IsValidationError(err), "the thumb guard reports a validation error, got: %v", err)

	assert.Zerof(t, f.ledgerRows(t, f.base.community.DID),
		"the reservation for a submission refused by the embed guard is still on the ledger")

	// The corrected retry — same post, thumb omitted — must be admitted.
	resp, err := submitWithThumb(nil)
	require.NoError(t, err, "the corrected retry was refused, so the embed-guard path leaked its reservation")
	require.NotEmpty(t, resp.URI)
	assert.Equal(t, 1, f.ledgerRows(t, f.base.community.DID))
}

// The concurrent double-tap the reserve-then-confirm ordering exists to stop:
// two byte-identical submissions racing through CreatePost. The ledger's
// unique key is the only arbiter both goroutines share, so exactly one may be
// admitted; the loser must hear the DUPLICATE sentinel (its post exists — a
// 409), not a generic failure, and must not leave a second row behind.
func TestService_ConcurrentIdenticalSubmissionsAdmitExactlyOne(t *testing.T) {
	t.Parallel()

	f := newAdmissionFixture(t)

	const doubleTap = "the same post, submitted twice at once"
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, errs[slot] = f.submit(t, f.base.community.DID, doubleTap)
		}(i)
	}
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range errs {
		if err == nil {
			winners++
			continue
		}
		losers++
		assert.ErrorIsf(t, err, posts.ErrDuplicateSubmission,
			"the racing loser must hear the duplicate sentinel — its post exists — not %v", err)
	}
	assert.Equal(t, 1, winners, "exactly one of two identical concurrent submissions may be admitted")
	assert.Equal(t, 1, losers)

	assert.Equal(t, 1, f.ledgerRows(t, f.base.community.DID),
		"the race must leave exactly the winner's row: the unique key is the arbiter, not a second insert")
}

// communityPDSURL reads a community's stored PDS, so a test that repoints it
// can put back what was actually there rather than what it assumed.
func communityPDSURL(t *testing.T, db *sql.DB, communityDID string) string {
	t.Helper()

	var pdsURL string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT pds_url FROM communities WHERE did = $1`, communityDID).Scan(&pdsURL))
	require.NotEmpty(t, pdsURL)
	return pdsURL
}

// setCommunityPDSURL repoints a community at a different PDS.
//
// Written with SQL because there is no service method for it: a community's PDS
// is chosen when it is provisioned and never moves. That is exactly why it is a
// usable seam for an unreachable-PDS test — the value is read fresh on every
// write (EnsureFreshToken re-fetches the row), and nothing caches it.
func setCommunityPDSURL(t *testing.T, db *sql.DB, communityDID, pdsURL string) {
	t.Helper()

	result, err := db.ExecContext(context.Background(),
		`UPDATE communities SET pds_url = $1 WHERE did = $2`, pdsURL, communityDID)
	require.NoError(t, err)

	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "no community row was repointed, so the test would prove nothing")
}

package posts

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The BRANCHES of the delete-path compensation (§5.3), pinned one at a time.
//
// service_delete_compensation_test.go proves the whole thing end to end against
// a real PDS and a real admissions table, and that is the test that says the
// feature works. It can only walk the happy path, though: it cannot make the
// community's host answer ErrCommunityNotHosted, cannot make the stamp fail
// after the record has already left the repo, and cannot arrange for the
// author's record to be gone before the delete arrives. Every one of those is
// a branch that decides whether an author's deletion finishes or silently
// half-finishes, and each is one `if` away from being wrong.
//
// So they are pinned here, in-process, with fakes — the tier that can construct
// a failure at an exact step (docs/TEST_ARCHITECTURE.md: behavioural breadth
// belongs at T0, not T2).
//
// WHAT THESE ASSERT IS AN ORDER AND A SET OF CALLS, deliberately. There is no
// database and no repo here, so "the row is soft-deleted" is not observable;
// what IS observable — and what the production bug was — is whether the service
// ISSUES the commands at all, and with what. Every assertion below is about a
// command that was or was not sent.

// ---------------------------------------------------------------------------
// Fakes
//
// Each one extends an existing package fake with the single capability this
// file needs: the ability to fail on demand, and to remember what it was asked
// to do. The originals return nil and record nothing, which is exactly right
// for their own tests and useless for these.
// ---------------------------------------------------------------------------

// withdrawalOutcome is what one community's host answers.
type withdrawalOutcome struct {
	result CommunityWriteResult
	err    error
}

// recordingWithdrawer is the AcceptanceWithdrawer seam, observed. It is the
// whole point of the fake set: the community-repo write is the step no unit
// test can perform and the one the compensation exists to make.
//
// It answers PER COMMUNITY when byCommunity holds an entry, and falls back to
// the flat result/err otherwise. The per-community layer exists because the
// interesting failures are asymmetric: one community's host being down while
// another's is healthy is precisely the case a loop that aborts on the first
// error gets wrong, and a single shared answer cannot express it.
type recordingWithdrawer struct {
	cmds   []CommunityAcceptanceDeleteCommand
	result CommunityWriteResult
	err    error

	byCommunity map[string]withdrawalOutcome
}

func (w *recordingWithdrawer) DeleteAcceptance(_ context.Context, cmd CommunityAcceptanceDeleteCommand) (CommunityWriteResult, error) {
	w.cmds = append(w.cmds, cmd)
	if outcome, ok := w.byCommunity[cmd.CommunityDID]; ok {
		return outcome.result, outcome.err
	}
	return w.result, w.err
}

// answer scripts one community's host.
func (w *recordingWithdrawer) answer(communityDID string, result CommunityWriteResult, err error) {
	if w.byCommunity == nil {
		w.byCommunity = map[string]withdrawalOutcome{}
	}
	w.byCommunity[communityDID] = withdrawalOutcome{result: result, err: err}
}

// callsFor counts the withdrawals aimed at one community.
func (w *recordingWithdrawer) callsFor(communityDID string) int {
	n := 0
	for _, cmd := range w.cmds {
		if cmd.CommunityDID == communityDID {
			n++
		}
	}
	return n
}

// tombstoningRepo is mockRepository with a SoftDelete that remembers and can
// fail.
type tombstoningRepo struct {
	mockRepository

	rawIndexedRow *Post
	softDeleted   []string
	softDeleteErr error
}

func (r *tombstoningRepo) GetRawIndexedRow(_ context.Context, _ string) (*Post, error) {
	if r.rawIndexedRow == nil {
		return nil, ErrNotFound
	}
	return r.rawIndexedRow, nil
}

func (r *tombstoningRepo) SoftDelete(_ context.Context, uri string) error {
	r.softDeleted = append(r.softDeleted, uri)
	return r.softDeleteErr
}

// stampingAdmissions is fakeAdmissions with an ApplyAcceptanceDelete that keeps
// the command — the watermark it carries is the assertion in behaviour 1 — and
// can fail.
type stampingAdmissions struct {
	*fakeAdmissions

	stamps   []CommunityDeleteCommand
	stampErr error
}

func (a *stampingAdmissions) ApplyAcceptanceDelete(_ context.Context, cmd CommunityDeleteCommand) (AdmissionResult, error) {
	a.stamps = append(a.stamps, cmd)
	return AdmissionResult{}, a.stampErr
}

// stampsFor returns the stamps aimed at one community.
func (a *stampingAdmissions) stampsFor(communityDID string) []CommunityDeleteCommand {
	var out []CommunityDeleteCommand
	for _, cmd := range a.stamps {
		if cmd.CommunityDID == communityDID {
			out = append(out, cmd)
		}
	}
	return out
}

// deletingAuthorRepo is memAuthorRepo with a DeleteRecord that can answer
// pds.ErrNotFound — the shape a retry after a half-finished delete arrives in.
type deletingAuthorRepo struct {
	*memAuthorRepo

	deleted   []string
	deleteErr error
}

func (r *deletingAuthorRepo) DeleteRecord(_ context.Context, collection, rkey string) error {
	r.deleted = append(r.deleted, collection+"/"+rkey)
	return r.deleteErr
}

// absentCommunities answers every community lookup with "not indexed".
//
// The interface is EMBEDDED rather than implemented: communities.Service has
// twenty methods and this file needs one, and an embedded nil interface panics
// on any other — which is itself the assertion that the delete path reaches for
// nothing else.
type absentCommunities struct{ communities.Service }

func (absentCommunities) GetByDID(_ context.Context, did string) (*communities.Community, error) {
	return nil, communities.ErrCommunityNotFound
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

const (
	compensationAuthorDID    = "did:plc:deletecompensationauthorxx"
	compensationCommunityDID = "did:plc:deletecompensationcommunity"
	compensationRkey         = "3lzdeletecompensation"
	compensationAcceptance   = "at://" + compensationCommunityDID + "/social.coves.community.acceptance/aaaa"

	// withdrawalRev is what the withdrawer reports having committed in. It is
	// the value behaviour 1 traces all the way through to the admission stamp.
	withdrawalRev = "3lzwithdrawalrev0"
)

// deleteHarness is the post service over the four fakes, plus the author's
// session and the URI of the post they are deleting.
type deleteHarness struct {
	service    Service
	repo       *tombstoningRepo
	admissions *stampingAdmissions
	withdrawer *recordingWithdrawer
	authorRepo *deletingAuthorRepo

	uri     string
	session *oauth.ClientSessionData
}

// newDeleteHarness builds the DEFAULT world: one accepted admission holding a
// standing acceptance URI, an author repo that deletes cleanly, and a
// withdrawer that withdraws successfully. Every test below changes exactly one
// thing about it, so the thing it changes is the thing it is testing.
func newDeleteHarness(t *testing.T) *deleteHarness {
	t.Helper()

	uri := "at://" + compensationAuthorDID + "/" + PostV2Collection + "/" + compensationRkey
	acceptance := compensationAcceptance

	h := &deleteHarness{
		uri:  uri,
		repo: &tombstoningRepo{},
		admissions: &stampingAdmissions{fakeAdmissions: &fakeAdmissions{
			rec: &engineRecorder{},
			byPostURIs: map[string][]*Admission{
				uri: {{
					CommunityDID:  compensationCommunityDID,
					PostURI:       uri,
					Status:        AdmissionStatusAccepted,
					AcceptanceURI: &acceptance,
				}},
			},
		}},
		withdrawer: &recordingWithdrawer{result: CommunityWriteResult{Rev: withdrawalRev}},
		authorRepo: &deletingAuthorRepo{memAuthorRepo: newMemAuthorRepo(compensationAuthorDID)},
		session:    sessionForDIDString(t, compensationAuthorDID),
	}

	h.service = NewPostService(
		h.repo, nil, nil, nil, nil, nil, "https://pds.invalid",
		WithAuthorRepoFactory(func(context.Context, string, *oauth.ClientSessionData) (AuthorRepo, error) {
			return h.authorRepo, nil
		}),
		// The acceptor half is unused by the delete path; the admissions half is
		// what says WHICH community accepted the post being deleted.
		WithSyncAcceptance(h.admissions, nil),
		WithAcceptanceWithdrawal(h.withdrawer),
		WithAdmissionPolicy(NewAllowAllAdmissionPolicyForTests()),
	)
	return h
}

// withCommunityService rebuilds the service over a community service, for the
// legacy-collection route. Everything else is unchanged, so a call that reached
// the new withdrawer would still be recorded.
func (h *deleteHarness) withCommunityService(communityService communities.Service, pdsOptions ...pds.ClientOption) {
	h.service = NewPostService(
		h.repo, communityService, nil, nil, nil, nil, "https://pds.invalid",
		WithAuthorRepoFactory(func(context.Context, string, *oauth.ClientSessionData) (AuthorRepo, error) {
			return h.authorRepo, nil
		}),
		WithSyncAcceptance(h.admissions, nil),
		WithAcceptanceWithdrawal(h.withdrawer),
		WithAdmissionPolicy(NewAllowAllAdmissionPolicyForTests()),
		WithPDSClientOptions(pdsOptions...),
	)
}

// delete runs the deletion under test.
func (h *deleteHarness) delete(uri string) error {
	return h.service.DeletePost(context.Background(), h.session, DeletePostRequest{URI: uri})
}

// acceptedIn replaces the post's admissions with one accepted row per
// community, IN THE ORDER GIVEN — the order the compensation will walk them, so
// a test can put the sick community first and ask what happened to the healthy
// one behind it.
//
// A post carries more than one admission whenever a community FORKED it: the
// fork's acceptance is a real record in a real repo, standing on its own, and
// every one of them now cites a postv2 that no longer exists.
func (h *deleteHarness) acceptedIn(communityDIDs ...string) {
	rows := make([]*Admission, 0, len(communityDIDs))
	for _, did := range communityDIDs {
		acceptance := "at://" + did + "/" + AcceptanceCollection + "/" + SubjectRkey(h.uri)
		rows = append(rows, &Admission{
			CommunityDID:  did,
			PostURI:       h.uri,
			Status:        AdmissionStatusAccepted,
			AcceptanceURI: &acceptance,
		})
	}
	h.admissions.byPostURIs[h.uri] = rows
}

func sessionForDIDString(t *testing.T, did string) *oauth.ClientSessionData {
	t.Helper()

	parsed, err := syntax.ParseDID(did)
	require.NoErrorf(t, err, "the test's own author DID %q is malformed", did)
	return &oauth.ClientSessionData{AccountDID: parsed, SessionID: "delete-compensation-unit-test"}
}

// ---------------------------------------------------------------------------
// 1. The happy path issues both halves
// ---------------------------------------------------------------------------

func TestDeletePost_WithdrawsTheAcceptanceAndStampsItAtTheWithdrawalsRev(t *testing.T) {
	h := newDeleteHarness(t)

	require.NoError(t, h.delete(h.uri))

	// THE LOCAL HALF. Without it this AppView keeps serving a post its author
	// withdrew, whatever the community's repo says.
	assert.Equal(t, []string{h.uri}, h.repo.softDeleted,
		"the index row was never soft-deleted, so post.get keeps serving the deleted post")

	// THE REMOTE HALF, aimed at the community the ADMISSION names — a deletion
	// carries no record to read the community from, so the row is the only
	// thing that knows whose acceptance this is.
	require.Lenf(t, h.withdrawer.cmds, 1,
		"expected exactly one withdrawal for one standing acceptance, got %d", len(h.withdrawer.cmds))
	assert.Equal(t, CommunityAcceptanceDeleteCommand{
		CommunityDID: compensationCommunityDID,
		PostURI:      h.uri,
	}, h.withdrawer.cmds[0],
		"the withdrawal must name the community and post the admission row names")

	// AND THE STAMP CARRIES THE WITHDRAWAL'S OWN REV. This is the §5.2 watermark
	// that makes the firehose copy of this same deletion a no-op rather than a
	// second decision: a stamp at any other rev either loses the CAS (and leaves
	// the row claiming an acceptance that is gone) or outranks an event it
	// should not.
	require.Lenf(t, h.admissions.stamps, 1,
		"the admission row was never stamped, so getStatus still answers `accepted` for a deleted post")
	assert.Equal(t, CommunityDeleteCommand{
		CommunityDID: compensationCommunityDID,
		PostURI:      h.uri,
		Watermark:    CommunityWatermark{Rev: withdrawalRev},
	}, h.admissions.stamps[0],
		"the stamp must carry the rev the WITHDRAWAL committed in")
}

// ---------------------------------------------------------------------------
// 2. The guard is the acceptance URI, not the status
// ---------------------------------------------------------------------------

func TestDeletePost_WithdrawsNothingWhenNoAcceptanceStands(t *testing.T) {
	h := newDeleteHarness(t)

	// A row that was never accepted — pending, no acceptance record anywhere.
	// Reaching the community's host for it would be a PDS round trip per delete
	// for the majority of posts, since most posts a community sees it never
	// accepted.
	h.admissions.byPostURIs[h.uri] = []*Admission{{
		CommunityDID:  compensationCommunityDID,
		PostURI:       h.uri,
		Status:        AdmissionStatusPending,
		AcceptanceURI: nil,
	}}

	require.NoError(t, h.delete(h.uri))

	assert.Emptyf(t, h.withdrawer.cmds,
		"a row holding no acceptance URI has nothing standing to withdraw; the service asked the "+
			"community's host to delete a record that was never written: %+v", h.withdrawer.cmds)
	assert.Empty(t, h.admissions.stamps,
		"nothing was withdrawn, so there is no withdrawal rev to stamp — a stamp here would advance the "+
			"§5.2 watermark past a community event that never happened")

	// THE LOCAL HALF STILL RUNS. It is not conditional on any community: the
	// author deleted their post, so this AppView stops serving it regardless of
	// what any community's repo does or does not hold.
	assert.Equal(t, []string{h.uri}, h.repo.softDeleted,
		"the local tombstone must not be gated on there being an acceptance to withdraw")
}

func TestDeletePost_WithdrawsAnAcceptanceThatIsAwaitingReacceptance(t *testing.T) {
	h := newDeleteHarness(t)

	// pending_reacceptance means the author EDITED the post after it was
	// accepted: the acceptance record is still standing in the community's repo,
	// it merely pins content that has since changed. The subject is being
	// deleted, so that record has to go — and a guard written against the STATUS
	// rather than the URI would leave it dangling, which is the exact bug the
	// firehose sweep's own comment calls out (authorpost.go withdrawAcceptance).
	acceptance := compensationAcceptance
	h.admissions.byPostURIs[h.uri] = []*Admission{{
		CommunityDID:  compensationCommunityDID,
		PostURI:       h.uri,
		Status:        AdmissionStatusPendingReacceptance,
		AcceptanceURI: &acceptance,
	}}

	require.NoError(t, h.delete(h.uri))

	require.Lenf(t, h.withdrawer.cmds, 1,
		"a pending_reacceptance row HAS a live acceptance record standing in the community's repo, and "+
			"deleting the subject must withdraw it; the guard is the acceptance URI, not the status")
	assert.Equal(t, compensationCommunityDID, h.withdrawer.cmds[0].CommunityDID)
	assert.Len(t, h.admissions.stamps, 1, "the withdrawal committed, so the row must be stamped")
}

// ---------------------------------------------------------------------------
// 3. What the admission lookup answers
// ---------------------------------------------------------------------------

func TestDeletePost_FailsWhenItCannotTellWhichCommunityAcceptedThePost(t *testing.T) {
	h := newDeleteHarness(t)
	h.admissions.byPostURIsErr = errors.New("the admissions table is unreachable")

	err := h.delete(h.uri)

	require.Errorf(t, err, "a lookup that FAILED says nothing about whether an acceptance stands, and "+
		"treating `I could not look` as `there is nothing there` reintroduces the silent version of this "+
		"bug: the author is told their post is gone while the community's repo still attests to it")
	assert.Empty(t, h.withdrawer.cmds,
		"nothing is known about which community to ask, so nothing may be withdrawn")
}

func TestDeletePost_SucceedsForAPostNoCommunityEverDecidedAbout(t *testing.T) {
	h := newDeleteHarness(t)

	// No admission row at all. This is an ordinary state, not a corrupt one: a
	// post written while the community was unreachable, or one whose row has
	// been reaped. There is nothing to withdraw and nothing to stamp, and the
	// author's deletion must still succeed.
	delete(h.admissions.byPostURIs, h.uri)

	require.NoError(t, h.delete(h.uri),
		"a post with no admission row has no acceptance anywhere; its deletion has nothing to compensate "+
			"beyond the local tombstone, and must not fail")
	assert.Empty(t, h.withdrawer.cmds)
	assert.Empty(t, h.admissions.stamps)
	assert.Equal(t, []string{h.uri}, h.repo.softDeleted,
		"the local tombstone runs for every deleted post, admission row or not")
}

// ---------------------------------------------------------------------------
// 4. A community hosted elsewhere
// ---------------------------------------------------------------------------

func TestDeletePost_SucceedsWhenTheAcceptanceBelongsToACommunityHostedElsewhere(t *testing.T) {
	h := newDeleteHarness(t)
	h.withdrawer.err = ErrCommunityNotHosted

	require.NoError(t, h.delete(h.uri),
		"the acceptance lives in the community's repo and needs its keys, which this instance does not "+
			"hold — and no retry ever will. The community's own AppView performs this cleanup when the "+
			"deletion reaches it over the firehose, so failing the author's delete here would fail it "+
			"forever on work this instance cannot do")

	assert.Equal(t, []string{h.uri}, h.repo.softDeleted,
		"not hosting the community is no reason to keep serving the post locally")
	assert.Len(t, h.withdrawer.cmds, 1, "the skip is discovered BY asking; the attempt still happens")

	// NOT STAMPED. The row belongs to a community whose repo this instance
	// cannot read or write, so it has no rev to stamp and no standing to claim
	// the acceptance was withdrawn — that community's own AppView will say so.
	assert.Empty(t, h.admissions.stamps,
		"a withdrawal that did not happen must not be recorded as one; stamping here would advance the "+
			"§5.2 watermark past a community event that was never written, and the real deletion arriving "+
			"later over the firehose would lose the CAS")
}

// ---------------------------------------------------------------------------
// 5. The retry, arriving after the record is already gone
// ---------------------------------------------------------------------------

func TestDeletePost_CompensatesOnRetryWhenTheRecordIsAlreadyGone(t *testing.T) {
	h := newDeleteHarness(t)

	// THE SHAPE A RETRY ARRIVES IN. The first attempt removed the record from
	// the author's repo and then failed part way through the compensation — a
	// crash, a lost connection to the community's host, a restart. The client
	// retries, and the PDS answers "no such record".
	//
	// Before the fix this branch returned a bare nil, which made the retry the
	// client is explicitly offered the ONE path that could never finish the
	// work: the post stays served and the acceptance stays standing, no matter
	// how many times the author presses delete.
	h.authorRepo.deleteErr = pds.ErrNotFound

	require.NoError(t, h.delete(h.uri),
		"a record that is already gone is the outcome the caller asked for, so the delete succeeds")

	assert.Equal(t, []string{h.uri}, h.repo.softDeleted,
		"the retry must still tombstone the index row — the first attempt may be exactly why it is "+
			"still standing")
	require.Lenf(t, h.withdrawer.cmds, 1,
		"the retry must still withdraw the acceptance: the record is gone from the author's repo, and "+
			"the community's attestation to it is precisely the thing left to clean up")
	assert.Len(t, h.admissions.stamps, 1,
		"the retry must still stamp the row back to pending")
}

// ---------------------------------------------------------------------------
// 6. Every failure after the record is gone is surfaced
//
// The consumer logs and swallows these, and must: an error there dead-letters
// an event whose local half already committed, and the rev gate refuses the
// redrive, so the retry could never reach the sweep again. HERE THE CLIENT IS
// THE RETRY LOOP — every step is idempotent, and reporting success over a
// half-finished compensation is the exact silence this path exists to end.
// ---------------------------------------------------------------------------

func TestDeletePost_SurfacesAFailureToTombstoneTheIndexRow(t *testing.T) {
	h := newDeleteHarness(t)
	h.repo.softDeleteErr = errors.New("the database is unreachable")

	err := h.delete(h.uri)

	require.Error(t, err,
		"the record is out of the author's repo and this AppView is still serving the post: answering "+
			"success would leave the author looking at a post they were told was deleted, with nothing "+
			"anywhere scheduled to notice")
	assert.Empty(t, h.withdrawer.cmds,
		"the local truth lands FIRST, in the consumer's order — a compensation that raced ahead to the "+
			"remote half would withdraw the community's acceptance of a post this AppView still serves")
}

func TestDeletePost_SurfacesAFailureToWithdrawTheAcceptance(t *testing.T) {
	h := newDeleteHarness(t)
	h.withdrawer.err = errors.New("the community's PDS is unreachable")

	err := h.delete(h.uri)

	require.Error(t, err,
		"the community's repo still holds a signed acceptance citing a record nobody can fetch, and the "+
			"client is the only retry loop this path has")
	assert.Empty(t, h.admissions.stamps,
		"a withdrawal that failed must not be stamped as done — the row's claim to an acceptance is "+
			"TRUE while the record still stands")
}

func TestDeletePost_SurfacesAFailureToStampTheWithdrawal(t *testing.T) {
	h := newDeleteHarness(t)
	h.admissions.stampErr = errors.New("the admissions table is unreachable")

	err := h.delete(h.uri)

	require.Error(t, err,
		"the acceptance record is OUT of the community's repo and the admission row still names it: "+
			"getStatus answers `accepted` over a post that no longer exists anywhere. The firehose copy "+
			"would reconcile it eventually, and `eventually` is the assumption this path refuses to make")
	assert.Len(t, h.withdrawer.cmds, 1, "the withdrawal itself succeeded; it is the stamp that failed")
}

// ---------------------------------------------------------------------------
// 7. The deprecated collection is not this path's business
// ---------------------------------------------------------------------------

func TestDeletePost_DoesNotWithdrawAcceptancesForALegacyCommunityRepoPost(t *testing.T) {
	h := newDeleteHarness(t)
	h.withCommunityService(absentCommunities{})

	// A pre-flip social.coves.community.post lives in the COMMUNITY's repo and
	// has no acceptance record at all — the acceptance/admission machinery came
	// in with the ownership flip. Routing one of these into the compensation
	// would send a withdrawal for a record that was never written, against a
	// subject rkey derived from a URI the acceptance scheme does not cover.
	legacyURI := "at://" + compensationCommunityDID + "/" + LegacyPostCollection + "/" + compensationRkey

	err := h.delete(legacyURI)

	assert.ErrorIsf(t, err, ErrCommunityNotFound,
		"the legacy delete must route to deleteCommunityPost, which resolves the community first; got: %v", err)
	assert.Emptyf(t, h.withdrawer.cmds,
		"a legacy community-repo post has no acceptance to withdraw, and the author-post compensation "+
			"must never see it: %+v", h.withdrawer.cmds)
	assert.Empty(t, h.admissions.stamps,
		"nothing about a legacy post's deletion stamps an admission row")
	assert.Empty(t, h.repo.softDeleted,
		"the legacy path's own tombstone is the firehose consumer's, unchanged by this work")
}

// ---------------------------------------------------------------------------
// 8. ONE SICK COMMUNITY MUST NOT STARVE THE OTHERS
//
// A post can hold an admission from more than one community: the one it was
// written into, and any that FORKED it. Each has its own acceptance record in
// its own repo, on its own host, and those hosts fail independently.
//
// The compensation walks them in a loop, and a loop that returns on the first
// error makes the set only as available as its least-available member. The
// author is told their delete failed, which is true; what is NOT true is that
// the work was attempted — every community behind the failing one is skipped,
// and it is skipped again on every retry, because the retry re-enters the same
// loop and stops in the same place. A single community whose host is gone for
// good therefore strands the acceptance of every other community, permanently,
// with no signal that anything but the first is even involved.
//
// Processing all of them and joining the failures converges instead: each pass
// completes the ones it can, and the retry has strictly less to do.
// ---------------------------------------------------------------------------

const (
	compensationCommunityA = "did:plc:deletecompensationcommunitya"
	compensationCommunityB = "did:plc:deletecompensationcommunityb"
)

func TestDeletePost_WithdrawsFromEveryCommunityEvenWhenOneOfThemFails(t *testing.T) {
	h := newDeleteHarness(t)
	h.acceptedIn(compensationCommunityA, compensationCommunityB)

	// A's host is down — a real failure, not the ErrCommunityNotHosted skip.
	// B's is healthy, and B is behind A in the walk.
	hostDown := errors.New("community A's PDS is unreachable")
	h.withdrawer.answer(compensationCommunityA, CommunityWriteResult{}, hostDown)
	h.withdrawer.answer(compensationCommunityB, CommunityWriteResult{Rev: withdrawalRev}, nil)

	err := h.delete(h.uri)

	// THE FAILURE STILL SURFACES. The compensation did not finish, and the
	// client is the retry loop.
	require.Error(t, err, "A's acceptance is still standing, so the delete has not finished")

	// AND B WAS STILL PROCESSED. This is the finding: today the loop returns on
	// A and B is never reached, so B's community keeps a signed acceptance of a
	// post that no longer exists — and no retry ever gets to it, because every
	// retry stops on A first.
	assert.Equalf(t, 1, h.withdrawer.callsFor(compensationCommunityB),
		"community B was never asked to withdraw: the loop aborted on A. One unreachable host must not "+
			"strand every community behind it — each pass must attempt them all and join what failed, so "+
			"a retry has strictly less left to do rather than stopping in the same place forever")
	assert.Lenf(t, h.admissions.stampsFor(compensationCommunityB), 1,
		"B's withdrawal committed, so B's admission row must be stamped back to pending; leaving it "+
			"accepted tells the author their deleted post is still live in B")

	// A's row is NOT stamped: nothing was withdrawn there, so there is no
	// withdrawal rev and no withdrawal to record.
	assert.Empty(t, h.admissions.stampsFor(compensationCommunityA),
		"A's withdrawal failed, so A's row must keep claiming the acceptance that is genuinely still there")

	// AND THE OPERATOR CAN SEE WHICH COMMUNITY FAILED. A joined error that
	// named neither would leave "one of this post's communities failed" as the
	// entire diagnosis.
	assert.ErrorContainsf(t, err, compensationCommunityA,
		"the error must name the community whose withdrawal failed; got: %v", err)
	assert.ErrorIsf(t, err, hostDown,
		"a generic (non-auth) failure must stay in the error chain — only the pds AUTH sentinels are "+
			"severed, and for a specific reason that does not apply here; got: %v", err)
}

func TestDeletePost_ContinuesPastACommunityHostedElsewhereToOneWeDoHost(t *testing.T) {
	h := newDeleteHarness(t)
	h.acceptedIn(compensationCommunityA, compensationCommunityB)

	// A is somebody else's community — a permanent, expected skip — and it is
	// FIRST. B is ours.
	h.withdrawer.answer(compensationCommunityA, CommunityWriteResult{}, ErrCommunityNotHosted)
	h.withdrawer.answer(compensationCommunityB, CommunityWriteResult{Rev: withdrawalRev}, nil)

	require.NoError(t, h.delete(h.uri),
		"a community hosted elsewhere is not a failure — its own AppView performs the cleanup when the "+
			"deletion reaches it over the firehose — so it must not fail the author's delete")

	assert.Equal(t, 1, h.withdrawer.callsFor(compensationCommunityA))
	assert.Empty(t, h.admissions.stampsFor(compensationCommunityA),
		"a withdrawal this instance cannot perform must not be recorded as one")

	assert.Equalf(t, 1, h.withdrawer.callsFor(compensationCommunityB),
		"the skip must not end the walk: B's acceptance is ours to withdraw and is the whole reason "+
			"this instance is doing any of this")
	assert.Len(t, h.admissions.stampsFor(compensationCommunityB), 1)
}

// ---------------------------------------------------------------------------
// 9. THE COMMUNITY'S CREDENTIALS ARE NOT THE AUTHOR'S SESSION
//
// The acceptance being withdrawn lives in the COMMUNITY's repo and goes out on
// the community's stored service token. When that token is rejected, the pds
// package answers ErrUnauthorized/ErrForbidden — the same sentinels the author's
// own session produces when IT dies.
//
// Letting them travel up the chain means the XRPC boundary reads a community's
// expired token as the caller's session being dead and answers 401 "sign in
// again": a user with a perfectly healthy session is told to re-authenticate
// over a server-side credential problem they cannot fix, they do, and it fails
// identically. It also hides a real outage from 5xx alerting, because a 401 is
// a client error by every dashboard's reckoning.
//
// The legacy delete path already severs this (communityCredentialFailure, %v
// not %w). The compensation is the same class of failure and needs the same cut.
// ---------------------------------------------------------------------------

func TestDeletePost_DoesNotBlameTheAuthorsSessionForTheCommunitysCredentials(t *testing.T) {
	for _, sentinel := range []error{pds.ErrUnauthorized, pds.ErrForbidden} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			h := newDeleteHarness(t)

			// Exactly the shape pds.wrapAPIError produces for a 401/403 from the
			// community's host.
			h.withdrawer.err = fmt.Errorf("applyWrites: %w: token has expired", sentinel)

			err := h.delete(h.uri)

			// IT STILL FAILS. The acceptance is standing and the compensation did
			// not finish; swallowing it would be the silence this whole path exists
			// to end.
			require.Error(t, err, "the withdrawal failed, so the delete has not finished")

			// BUT NOT AS AN AUTH FAILURE. This is the finding: the boundary maps
			// these sentinels to a 401 aimed at the CALLER.
			assert.Falsef(t, errors.Is(err, sentinel),
				"the community's rejected credentials reached the API boundary carrying %v, which the "+
					"mapper reads as the AUTHOR's session being dead — answering 401 `sign in again` to a "+
					"user whose session is fine, over a token only the server can fix. Sever it with %%v "+
					"the way communityCredentialFailure does, so it lands as an unclassified 500 and "+
					"shows up in outage alerting; got: %v", sentinel, err)
			assert.Falsef(t, pds.IsAuthError(err),
				"pds.IsAuthError is the predicate the boundary actually consults; got: %v", err)

			// The diagnosis survives the cut — the operator still gets the cause
			// and the community it came from, just not as a typed auth error.
			assert.ErrorContains(t, err, compensationCommunityDID,
				"severing the sentinel must not sever the operator's ability to tell which community's "+
					"credentials were rejected")
			assert.ErrorContains(t, err, "token has expired",
				"the underlying message must survive; a bare `credentials rejected` names no cause")
		})
	}
}

// ---------------------------------------------------------------------------
// 10. AN EMPTY REV MEANS TWO DIFFERENT THINGS
//
// The stamp is guarded on a non-empty rev, because the admission repository
// refuses a fabricated watermark — correctly. But the guard currently answers
// the same way to two states that are not remotely the same:
//
//   - a SKIP with no rev: nothing stood in the community's repo, so there is
//     nothing to record. Returning nil is right.
//   - a COMMITTED withdrawal with no rev: the acceptance record is GONE and the
//     writer failed to report where. The row still names it, and returning nil
//     reports success over exactly the half-finished state — record gone, row
//     stranded, nothing scheduled to notice — that this whole change exists to
//     make impossible.
//
// The second is a writer-contract violation (CommunityWriteResult documents a
// rev on every path), and a contract violation that produces silence is the
// worst available outcome. It has to surface.
// ---------------------------------------------------------------------------

func TestDeletePost_SucceedsWhenTheSkippedWithdrawalHasNoRevToStamp(t *testing.T) {
	h := newDeleteHarness(t)
	h.withdrawer.result = CommunityWriteResult{Skipped: true, Rev: ""}

	require.NoError(t, h.delete(h.uri),
		"nothing stood in the community's repo, so there is nothing to withdraw and nothing to record; "+
			"this is the ordinary redelivery case and must not fail the author's delete")
	assert.Empty(t, h.admissions.stamps,
		"there is no rev, and a stamp without one would be a fabricated watermark the repository "+
			"correctly refuses")
}

func TestDeletePost_FailsWhenACommittedWithdrawalReportsNoRev(t *testing.T) {
	h := newDeleteHarness(t)

	// Skipped false: the writer says it COMMITTED the deletion. And no rev,
	// which its own contract forbids.
	h.withdrawer.result = CommunityWriteResult{Skipped: false, Rev: ""}

	err := h.delete(h.uri)

	require.Error(t, err,
		"the acceptance record is OUT of the community's repo and the admission row still names it, "+
			"because there was no rev to stamp with. Answering success here strands the row silently — "+
			"getStatus keeps saying `accepted` for a post whose acceptance no longer exists, and nothing "+
			"anywhere is scheduled to reconcile it. A writer that committed without reporting a rev has "+
			"broken its contract, and a broken contract must not be absorbed into a success")

	assert.Empty(t, h.admissions.stamps,
		"an empty rev must never be stamped — the repository refuses it as a fabricated watermark, and "+
			"surfacing the writer's failure is the answer, not laundering it into a bad write")
}

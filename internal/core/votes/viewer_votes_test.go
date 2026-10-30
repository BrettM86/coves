package votes_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"

	"github.com/stretchr/testify/require"
)

// The viewer's own votes as the read path resolves them (PRD §5.1, §5.2): the
// votes index, overlaid by this AppView's own recent writes until the index
// shows positive evidence that it has caught up with each one.
//
// "Indexed" means the test wrote or soft-deleted the row in the fake index
// itself; no consumer runs. Overlay entries come only from real CreateVote
// calls through the fake PDS.

const (
	controlSubjectURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kcontrol"
	secondSubjectURI  = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3ksecond"
	thirdSubjectURI   = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kthird"
	unvotedSubjectURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kunvoted"

	otherVoterDID = "did:plc:dddddddddddddddddddddddd"

	indexedUpVoteURI      = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.feed.vote/3kindexedup"
	indexedDownVoteURI    = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.feed.vote/3kindexeddown"
	indexedDeletedVoteURI = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.feed.vote/3kindexedgone"
	otherVoterVoteURI     = "at://did:plc:dddddddddddddddddddddddd/social.coves.feed.vote/3kothervoter"
)

func controlSubject() votes.StrongRef {
	return votes.StrongRef{URI: controlSubjectURI, CID: "bafyreicontrol"}
}

// castVote casts a vote through the service and returns its URI.
func castVote(t *testing.T, service votes.Service, subject votes.StrongRef, direction string) string {
	t.Helper()
	return castVoteAs(t, service, testVoterDID, subject, direction)
}

// castVoteAs casts a vote as the given voter and returns its URI.
func castVoteAs(t *testing.T, service votes.Service, voterDID string, subject votes.StrongRef, direction string) string {
	t.Helper()
	response, err := service.CreateVote(context.Background(), voter(t, voterDID),
		votes.CreateVoteRequest{Subject: subject, Direction: direction})
	require.NoError(t, err, "test setup: casting a vote")
	require.NotEmpty(t, response.URI, "test setup: the vote must be cast, not toggled off")
	return response.URI
}

// withdrawVote taps the same arrow again, which toggles the vote off.
func withdrawVote(t *testing.T, service votes.Service, subject votes.StrongRef, direction string) {
	t.Helper()
	response, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject, Direction: direction})
	require.NoError(t, err, "test setup: withdrawing a vote")
	require.Empty(t, response.URI, "test setup: the tap must toggle the vote off")
}

// requireViewerVotes compares a resolved map; nil and empty both mean "no votes".
func requireViewerVotes(t *testing.T, want, got map[string]votes.ViewerVote, message string) {
	t.Helper()
	if len(want) == 0 {
		require.Empty(t, got, message)
		return
	}
	require.Equal(t, want, got, message)
}

func TestViewerVotes_IndexOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := newFakeVoteRepository(t)
	repository.indexActive(t, indexedUpVoteURI, testSubject, "up")
	repository.indexActive(t, indexedDownVoteURI, secondSubjectURI, "down")
	repository.indexActive(t, indexedDeletedVoteURI, thirdSubjectURI, "up")
	repository.softDelete(t, indexedDeletedVoteURI)
	require.NoError(t, repository.Create(ctx, &votes.Vote{
		URI: otherVoterVoteURI, VoterDID: otherVoterDID, SubjectURI: unvotedSubjectURI, Direction: "up",
	}))
	fake := newFakePDS(t, testVoterDID)
	service := newService(t, fake, repository)

	got, err := service.ViewerVotes(ctx, testVoterDID,
		[]string{testSubject, secondSubjectURI, thirdSubjectURI, unvotedSubjectURI})
	require.NoError(t, err)

	require.Equal(t, map[string]votes.ViewerVote{
		testSubject:      {Direction: "up", URI: indexedUpVoteURI},
		secondSubjectURI: {Direction: "down", URI: indexedDownVoteURI},
	}, got, "active indexed rows are the viewer's votes; a soft-deleted row and another voter's row are not")
	require.Equal(t, 1, repository.callCount(repositoryGetActiveVotesForSubjects),
		"every subject in the request is resolved by one index query")
	require.Zero(t, repository.callCount(repositoryGetIndexedStatesByURIs),
		"with no recent writes there is nothing to confirm")
	require.Zero(t, fake.listCalls, "the read path never scans the PDS")
}

func TestViewerVotes_VotedEntryRetiresOnIndexedRow(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name            string
		index           func(t *testing.T, repository *fakeVoteRepository, voteURI string)
		wantIndexedVote bool
	}{
		{
			name: "create indexed",
			index: func(t *testing.T, repository *fakeVoteRepository, voteURI string) {
				repository.indexActive(t, voteURI, testSubject, "up")
			},
			wantIndexedVote: true,
		},
		{
			name: "create and later delete indexed",
			index: func(t *testing.T, repository *fakeVoteRepository, voteURI string) {
				repository.indexActive(t, voteURI, testSubject, "up")
				repository.softDelete(t, voteURI)
			},
			wantIndexedVote: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repository := newFakeVoteRepository(t)
			service := newService(t, newFakePDS(t, testVoterDID), repository)
			voted := castVote(t, service, subject(), "up")
			control := castVote(t, service, controlSubject(), "down")

			got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
			require.NoError(t, err)
			require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: voted}}, got,
				"a vote cast here shows before Jetstream indexes it")
			require.Equal(t, 2, votes.OverlaySizeForTest(service))

			testCase.index(t, repository, voted)
			want := map[string]votes.ViewerVote{}
			if testCase.wantIndexedVote {
				want = map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: voted}}
			}

			got, err = service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, want, got, "a confirmed entry yields the index value")
			require.Equal(t, 1, votes.OverlaySizeForTest(service),
				"the confirmed entry is retired; the entry for a subject not in the request stays")

			confirmationsBefore := repository.callCount(repositoryGetIndexedStatesByURIs)
			got, err = service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, want, got, "after retirement the index value stands")
			require.Equal(t, confirmationsBefore, repository.callCount(repositoryGetIndexedStatesByURIs),
				"a retired entry needs no confirmation query")

			got, err = service.ViewerVotes(ctx, testVoterDID, []string{controlSubjectURI})
			require.NoError(t, err)
			require.Equal(t, map[string]votes.ViewerVote{controlSubjectURI: {Direction: "down", URI: control}}, got)
		})
	}
}

func TestViewerVotes_RemovedEntryRetiresOnlyOnSoftDeletedRow(t *testing.T) {
	t.Parallel()

	// likeThenUnlike removes a vote cast here before Jetstream indexed it.
	likeThenUnlike := func(t *testing.T, service votes.Service, _ *fakePDS, _ *fakeVoteRepository) string {
		voteURI := castVote(t, service, subject(), "up")
		withdrawVote(t, service, subject(), "up")
		return voteURI
	}
	// toggleOffIndexedVote removes a vote that is already in the index (and on
	// the PDS, where it was written).
	toggleOffIndexedVote := func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository) string {
		repository.indexActive(t, indexedUpVoteURI, testSubject, "up")
		fake.records["3kindexedup"] = votes.VoteRecord{
			Type: "social.coves.feed.vote", Direction: "up",
			Subject: subject(), CreatedAt: "2026-10-28T00:00:00Z",
		}
		withdrawVote(t, service, subject(), "up")
		require.Equal(t, []string{"3kindexedup"}, fake.deletes(), "test setup: the toggle-off deletes the indexed vote")
		return indexedUpVoteURI
	}
	indexCreate := func(t *testing.T, repository *fakeVoteRepository, voteURI string) {
		repository.indexActive(t, voteURI, testSubject, "up")
	}
	indexCreateAndDelete := func(t *testing.T, repository *fakeVoteRepository, voteURI string) {
		repository.indexActive(t, voteURI, testSubject, "up")
		repository.softDelete(t, voteURI)
	}
	indexDelete := func(t *testing.T, repository *fakeVoteRepository, voteURI string) {
		repository.softDelete(t, voteURI)
	}
	indexNothing := func(*testing.T, *fakeVoteRepository, string) {}

	for _, testCase := range []struct {
		name        string
		remove      func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository) string
		index       func(t *testing.T, repository *fakeVoteRepository, voteURI string)
		wantRetired bool
	}{
		{"like then unlike, row absent", likeThenUnlike, indexNothing, false},
		{"like then unlike, row active", likeThenUnlike, indexCreate, false},
		{"like then unlike, row soft-deleted", likeThenUnlike, indexCreateAndDelete, true},
		{"indexed vote toggled off, row active", toggleOffIndexedVote, indexNothing, false},
		{"indexed vote toggled off, row soft-deleted", toggleOffIndexedVote, indexDelete, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			service := newService(t, fake, repository)
			removed := testCase.remove(t, service, fake, repository)
			control := castVote(t, service, controlSubject(), "down")
			testCase.index(t, repository, removed)

			got, err := service.ViewerVotes(context.Background(), testVoterDID,
				[]string{testSubject, controlSubjectURI})
			require.NoError(t, err)
			require.Equal(t, map[string]votes.ViewerVote{
				controlSubjectURI: {Direction: "down", URI: control},
			}, got, "a vote removed here shows as no vote, even while the index still holds it")

			wantSize := 2
			if testCase.wantRetired {
				wantSize = 1
			}
			require.Equal(t, wantSize, votes.OverlaySizeForTest(service),
				"a removal retires only when its row is indexed as soft-deleted; absence confirms nothing")
		})
	}
}

func TestViewerVotes_RowIndexedBetweenTheReadsQueriesKeepsTheEntry(t *testing.T) {
	t.Parallel()
	repository := newFakeVoteRepository(t)
	service := newService(t, newFakePDS(t, testVoterDID), repository)
	voted := castVote(t, service, subject(), "up")

	// The row lands after the read's first query has computed its result and
	// before its second query runs. The confirmation must not be newer than
	// the index result it is paired with.
	queries := 0
	repository.setAfterQuery(func(string) {
		queries++
		if queries == 1 {
			repository.indexActive(t, voted, testSubject, "up")
		}
	})

	got, err := service.ViewerVotes(context.Background(), testVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: voted}}, got,
		"the entry was confirmed against an index result that predates its row")
	require.Equal(t, 1, votes.OverlaySizeForTest(service),
		"an entry confirmed only after the index query ran must stay until a later read sees its row")
}

func TestViewerVotes_FailedQueryConfirmsNothing(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		method string
	}{
		{"index query", repositoryGetActiveVotesForSubjects},
		{"confirmation query", repositoryGetIndexedStatesByURIs},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repository := newFakeVoteRepository(t)
			service := newService(t, newFakePDS(t, testVoterDID), repository)
			confirmable := castVote(t, service, subject(), "up")
			repository.indexActive(t, confirmable, testSubject, "up")
			unconfirmed := castVote(t, service, controlSubject(), "down")

			injected := errors.New("votes index unavailable")
			repository.failMethod(testCase.method, injected)
			_, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject, controlSubjectURI})
			require.ErrorIs(t, err, injected, "a failed lookup must not render as \"not voted\"")
			require.Equal(t, 2, votes.OverlaySizeForTest(service), "a failed query confirms nothing")

			repository.failMethod(testCase.method, nil)
			got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject, controlSubjectURI})
			require.NoError(t, err)
			require.Equal(t, map[string]votes.ViewerVote{
				testSubject:       {Direction: "up", URI: confirmable},
				controlSubjectURI: {Direction: "down", URI: unconfirmed},
			}, got, "both entries survived the failed read")
			require.Equal(t, 1, votes.OverlaySizeForTest(service), "the confirmable entry retires once a read succeeds")
		})
	}
}

func TestViewerVotes_ReadDoesNotRetireANewerWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := newFakeVoteRepository(t)
	service := newService(t, newFakePDS(t, testVoterDID), repository)
	first := castVote(t, service, subject(), "up")
	repository.indexActive(t, first, testSubject, "up")

	// While the read's confirmation query reports the first vote as indexed,
	// the viewer switches direction. The read evaluated the older entry and
	// must not retire the newer one.
	replacement := ""
	hookRan := false
	repository.setAfterQuery(func(method string) {
		if method != repositoryGetIndexedStatesByURIs || hookRan {
			return
		}
		hookRan = true
		replacement = castVote(t, service, subject(), "down")
	})
	_, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err)
	repository.setAfterQuery(nil)

	got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "down", URI: replacement}}, got,
		"the read that confirmed the older vote retired the newer write")
	require.Equal(t, 1, votes.OverlaySizeForTest(service), "the newer write's entry is still held")
	require.True(t, hookRan, "test setup: the write ran during the read's confirmation query")
	require.NotEqual(t, first, replacement)
}

const (
	retention = 24 * time.Hour

	abandoningVoterOneDID   = "did:plc:eeeeeeeeeeeeeeeeeeeeeeee"
	abandoningVoterTwoDID   = "did:plc:ffffffffffffffffffffffff"
	abandoningVoterThreeDID = "did:plc:gggggggggggggggggggggggg"
)

// recentWriteStart is when each clock-controlled test makes its first write.
var recentWriteStart = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// testClock is the service's clock in tests about overlay expiry.
type testClock struct {
	mutex   sync.Mutex
	current time.Time
}

// installClock replaces the service's clock with one stopped at
// recentWriteStart. Install it before the first write: an entry's recorded-at
// time comes from the clock at the moment of the write.
func installClock(t *testing.T, service votes.Service) *testClock {
	t.Helper()
	clock := &testClock{current: recentWriteStart}
	votes.SetNowForTest(service, clock.now)
	return clock
}

func (c *testClock) now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.current
}

// setSinceStart moves the clock to recentWriteStart plus elapsed.
func (c *testClock) setSinceStart(elapsed time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.current = recentWriteStart.Add(elapsed)
}

// serverError is how the PDS client reports a 5xx: the write may or may not
// have committed.
func serverError(operation string) error {
	return fmt.Errorf("%s: %w: upstream unavailable", operation, pds.ErrServerError)
}

func TestViewerVotes_EntryWinsOverAStaleIndexOnlyWithinRetention(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		// write makes this AppView's write against a stale index and returns
		// the viewer's vote as the entry says it is (nil for no vote).
		write func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository) map[string]votes.ViewerVote
		// wantAfterRetention is the stale index's value.
		wantAfterRetention map[string]votes.ViewerVote
	}{
		{
			name: "voted, no indexed row",
			write: func(t *testing.T, service votes.Service, _ *fakePDS, _ *fakeVoteRepository) map[string]votes.ViewerVote {
				voted := castVote(t, service, subject(), "up")
				return map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: voted}}
			},
			wantAfterRetention: nil,
		},
		{
			name: "voted, an older active row with a different URI",
			write: func(t *testing.T, service votes.Service, _ *fakePDS, repository *fakeVoteRepository) map[string]votes.ViewerVote {
				repository.indexActive(t, indexedDownVoteURI, testSubject, "down")
				voted := castVote(t, service, subject(), "up")
				require.NotEqual(t, indexedDownVoteURI, voted, "test setup: the switch writes a new record")
				return map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: voted}}
			},
			wantAfterRetention: map[string]votes.ViewerVote{
				testSubject: {Direction: "down", URI: indexedDownVoteURI},
			},
		},
		{
			name: "removed, the row still active",
			write: func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository) map[string]votes.ViewerVote {
				repository.indexActive(t, indexedUpVoteURI, testSubject, "up")
				fake.records["3kindexedup"] = votes.VoteRecord{
					Type: "social.coves.feed.vote", Direction: "up",
					Subject: subject(), CreatedAt: "2026-10-28T00:00:00Z",
				}
				withdrawVote(t, service, subject(), "up")
				return nil
			},
			wantAfterRetention: map[string]votes.ViewerVote{
				testSubject: {Direction: "up", URI: indexedUpVoteURI},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			service := newService(t, fake, repository)
			clock := installClock(t, service)
			wantWithinRetention := testCase.write(t, service, fake, repository)

			clock.setSinceStart(retention - time.Second)
			got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, wantWithinRetention, got,
				"one second before retention ends, this AppView's write still wins over the stale index")

			clock.setSinceStart(retention + time.Second)
			got, err = service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, testCase.wantAfterRetention, got,
				"one second after retention ends, the index is authoritative")
		})
	}
}

func TestCreateVote_ExpiredUnverifiedEntryNoLongerScansThePDS(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name          string
		elapsed       time.Duration
		wantListCalls int
	}{
		{"within retention", retention - time.Second, 1},
		{"after retention", retention + time.Second, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newFakePDS(t, testVoterDID)
			service := newService(t, fake, newFakeVoteRepository(t))
			clock := installClock(t, service)

			fake.createErr = serverError("createRecord")
			_, err := service.CreateVote(ctx, voter(t, testVoterDID),
				votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
			require.ErrorIs(t, err, pds.ErrServerError, "test setup: the create's outcome is unknown")
			fake.createErr = nil

			clock.setSinceStart(testCase.elapsed)
			response, err := service.CreateVote(ctx, voter(t, testVoterDID),
				votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
			require.NoError(t, err)
			require.NotEmpty(t, response.URI, "the PDS holds no vote, so the tap casts one")
			require.Len(t, fake.creates(), 1)
			require.Equal(t, testCase.wantListCalls, fake.listCalls,
				"an Unverified entry sends the next write to the PDS only until it expires")

			got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
			require.NoError(t, err)
			require.Equal(t, map[string]votes.ViewerVote{
				testSubject: {Direction: "up", URI: "at://" + testVoterDID + "/social.coves.feed.vote/" + fake.creates()[0]},
			}, got, "the new vote displays: pruning the expired entry does not remove its replacement")
		})
	}
}

func TestViewerVotes_EntryForASubjectNotInTheRequestIsUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := newFakeVoteRepository(t)
	service := newService(t, newFakePDS(t, testVoterDID), repository)

	// In the request, confirmable: its row is indexed.
	requested := castVote(t, service, subject(), "up")
	repository.indexActive(t, requested, testSubject, "up")
	// Not in the request, confirmable: a read of other subjects must not
	// retire it.
	confirmable := castVote(t, service, controlSubject(), "down")
	repository.indexActive(t, confirmable, controlSubjectURI, "down")
	// Not in the request, against a stale index: only the entry knows the
	// viewer switched to up.
	repository.indexActive(t, indexedDownVoteURI, secondSubjectURI, "down")
	switched := castVote(t, service, votes.StrongRef{URI: secondSubjectURI, CID: "bafyreisecond"}, "up")
	require.Equal(t, 3, votes.OverlaySizeForTest(service), "each write records an entry for its key")

	got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: requested}}, got)
	require.Equal(t, 2, votes.OverlaySizeForTest(service),
		"only the requested subject's entry retires; entries for other subjects are untouched")

	got, err = service.ViewerVotes(ctx, testVoterDID, []string{controlSubjectURI, secondSubjectURI})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{
		controlSubjectURI: {Direction: "down", URI: confirmable},
		secondSubjectURI:  {Direction: "up", URI: switched},
	}, got, "a later read of the untouched entries still honours them over the stale index")
}

func TestViewerVotes_ViewersSeeOnlyTheirOwnRecentWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	firstViewerPDS := newFakePDS(t, testVoterDID)
	secondViewerPDS := newFakePDS(t, otherVoterDID)
	service := newServiceForVoters(t, newFakeVoteRepository(t), firstViewerPDS, secondViewerPDS)

	firstViewerVote := castVoteAs(t, service, testVoterDID, subject(), "up")
	secondViewerVote := castVoteAs(t, service, otherVoterDID, subject(), "down")
	require.Len(t, secondViewerPDS.creates(), 1, "test setup: the second viewer's vote is a plain create")
	secondViewerRKey := secondViewerPDS.creates()[0]

	got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: firstViewerVote}}, got,
		"the first viewer sees only their own vote")
	got, err = service.ViewerVotes(ctx, otherVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "down", URI: secondViewerVote}}, got,
		"the second viewer sees only their own vote")

	// The second viewer taps the same arrow again.
	response, err := service.CreateVote(ctx, voter(t, otherVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
	require.NoError(t, err)
	require.Empty(t, response.URI, "the second viewer's tap toggles their own vote off")
	require.Equal(t, []string{secondViewerRKey}, secondViewerPDS.deletes(),
		"the toggle-off deletes the second viewer's record")
	require.Empty(t, firstViewerPDS.deletes(), "the first viewer's repo is not written")
	require.Len(t, firstViewerPDS.records, 1, "the first viewer's vote is still on their PDS")

	got, err = service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "up", URI: firstViewerVote}}, got,
		"the second viewer's toggle-off does not change the first viewer's vote")
	got, err = service.ViewerVotes(ctx, otherVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Empty(t, got, "the second viewer's vote is removed")
}

func TestCreateVote_PrunesExpiredEntriesOfViewersWhoNeverReturn(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		elapsed     time.Duration
		wantEntries int
	}{
		{"within retention", retention - time.Second, 4},
		{"after retention", retention + time.Second, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			abandoningVoters := []string{abandoningVoterOneDID, abandoningVoterTwoDID, abandoningVoterThreeDID}
			fakes := []*fakePDS{newFakePDS(t, testVoterDID)}
			for _, abandoningVoter := range abandoningVoters {
				fakes = append(fakes, newFakePDS(t, abandoningVoter))
			}
			service := newServiceForVoters(t, newFakeVoteRepository(t), fakes...)
			clock := installClock(t, service)
			for _, abandoningVoter := range abandoningVoters {
				castVoteAs(t, service, abandoningVoter, controlSubject(), "up")
			}

			clock.setSinceStart(testCase.elapsed)
			castVote(t, service, subject(), "up")
			require.Equal(t, testCase.wantEntries, votes.OverlaySizeForTest(service),
				"a write removes expired entries, even those of viewers who never write again")
		})
	}
}

func TestCreateVote_PruningAnExpiredWriteKeepsANewerWriteForTheSameKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakePDS(t, testVoterDID)
	service := newService(t, fake, newFakeVoteRepository(t))
	clock := installClock(t, service)

	castVote(t, service, subject(), "up")
	clock.setSinceStart(23 * time.Hour)
	switched := castVote(t, service, subject(), "down")
	// Another key's write after the first write expired prunes it.
	clock.setSinceStart(retention + time.Second)
	castVote(t, service, controlSubject(), "up")

	got, err := service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err)
	require.Equal(t, map[string]votes.ViewerVote{testSubject: {Direction: "down", URI: switched}}, got,
		"the 23-hour write is within retention; pruning the key's expired first write does not remove it")
}

func TestViewerVotes_QueriesPerRead(t *testing.T) {
	t.Parallel()
	readSubjects := []string{testSubject, secondSubjectURI, thirdSubjectURI, unvotedSubjectURI}
	for _, testCase := range []struct {
		name string
		// write makes this AppView's writes and returns what the read resolves.
		write             func(t *testing.T, service votes.Service, fake *fakePDS) map[string]votes.ViewerVote
		wantConfirmations int
	}{
		{
			name: "voted and removed entries",
			write: func(t *testing.T, service votes.Service, _ *fakePDS) map[string]votes.ViewerVote {
				up := castVote(t, service, subject(), "up")
				down := castVote(t, service, votes.StrongRef{URI: secondSubjectURI, CID: "bafyreisecond"}, "down")
				third := votes.StrongRef{URI: thirdSubjectURI, CID: "bafyreithird"}
				castVote(t, service, third, "up")
				withdrawVote(t, service, third, "up")
				return map[string]votes.ViewerVote{
					testSubject:      {Direction: "up", URI: up},
					secondSubjectURI: {Direction: "down", URI: down},
				}
			},
			wantConfirmations: 1,
		},
		{
			name: "only unverified entries",
			write: func(t *testing.T, service votes.Service, fake *fakePDS) map[string]votes.ViewerVote {
				fake.createErr = serverError("createRecord")
				for _, subjectURI := range []string{testSubject, secondSubjectURI} {
					_, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{
						Subject: votes.StrongRef{URI: subjectURI, CID: "bafyreiunverified"}, Direction: "up",
					})
					require.ErrorIs(t, err, pds.ErrServerError, "test setup: the create's outcome is unknown")
				}
				fake.createErr = nil
				return nil
			},
			wantConfirmations: 0,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			service := newService(t, fake, repository)
			want := testCase.write(t, service, fake)
			confirmationsBefore := repository.callCount(repositoryGetIndexedStatesByURIs)
			indexQueriesBefore := repository.callCount(repositoryGetActiveVotesForSubjects)

			got, err := service.ViewerVotes(context.Background(), testVoterDID, readSubjects)
			require.NoError(t, err)
			requireViewerVotes(t, want, got, "the read resolves every requested subject")
			require.Equal(t, 1, repository.callCount(repositoryGetActiveVotesForSubjects)-indexQueriesBefore,
				"every subject in the request is resolved by one index query")
			require.Equal(t, testCase.wantConfirmations,
				repository.callCount(repositoryGetIndexedStatesByURIs)-confirmationsBefore,
				"one confirmation query covers every Voted and Removed entry; Unverified entries need none")
		})
	}
}

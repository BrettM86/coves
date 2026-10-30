package votes_test

import (
	"context"
	"errors"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"

	"github.com/stretchr/testify/require"
)

// Where a write finds "the existing vote" (PRD §5.3): the votes index, through
// the same resolver as the read path. The PDS is scanned only after an
// ambiguous write, and a database failure fails the write before anything
// reaches the PDS.

// indexVoteWithRecord indexes the viewer's vote on testSubject and puts its
// record on the viewer's PDS, where it was written.
func indexVoteWithRecord(t *testing.T, fake *fakePDS, repository *fakeVoteRepository, uri, rkey, direction string) {
	t.Helper()
	repository.indexActive(t, uri, testSubject, direction)
	fake.records[rkey] = votes.VoteRecord{
		Type: "social.coves.feed.vote", Direction: direction,
		Subject: subject(), CreatedAt: "2026-10-28T00:00:00Z",
	}
}

func TestCreateVote_LooksUpTheExistingVoteInTheIndex(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		// The viewer's indexed vote on the subject; an empty URI means none.
		indexedURI       string
		indexedRKey      string
		indexedDirection string
		wantVoteCast     bool
		wantCreates      int
		wantDeletes      []string
		wantBatchCalls   int
	}{
		{
			name:         "no indexed row: a first vote",
			wantVoteCast: true,
			wantCreates:  1,
		},
		{
			name:       "indexed row in the same direction: toggle off",
			indexedURI: indexedUpVoteURI, indexedRKey: "3kindexedup", indexedDirection: "up",
			wantVoteCast: false,
			wantCreates:  0,
			wantDeletes:  []string{"3kindexedup"},
		},
		{
			name:       "indexed row in the opposite direction: replace",
			indexedURI: indexedDownVoteURI, indexedRKey: "3kindexeddown", indexedDirection: "down",
			wantVoteCast:   true,
			wantCreates:    1,
			wantDeletes:    []string{"3kindexeddown"},
			wantBatchCalls: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			if testCase.indexedURI != "" {
				indexVoteWithRecord(t, fake, repository, testCase.indexedURI, testCase.indexedRKey, testCase.indexedDirection)
			}
			service := newService(t, fake, repository)

			response, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
				votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
			require.NoError(t, err)
			require.Equal(t, testCase.wantVoteCast, response.URI != "", "cast, or toggled off")
			require.Len(t, fake.creates(), testCase.wantCreates)
			require.Equal(t, testCase.wantDeletes, fake.deletes(),
				"the write acts on the indexed row's record key")
			require.Equal(t, testCase.wantBatchCalls, fake.batchCalls)
			require.Zero(t, fake.listCalls,
				"an ordinary write finds the existing vote in the index, not by scanning the PDS")
		})
	}
}

func TestVoteWrites_DatabaseFailureFailsTheWriteWithNoPDSEffect(t *testing.T) {
	t.Parallel()

	// indexedVote: a vote the index and the PDS both hold, and no overlay
	// entry. A write that fell back to "no vote" would create; one that fell
	// back to the PDS scan would find the record and act on it.
	indexedVote := func(t *testing.T, _ votes.Service, fake *fakePDS, repository *fakeVoteRepository) {
		indexVoteWithRecord(t, fake, repository, indexedUpVoteURI, "3kindexedup", "up")
	}
	// votedEntry: a Voted entry on the key, so the lookup runs the
	// confirmation query.
	votedEntry := func(t *testing.T, service votes.Service, _ *fakePDS, _ *fakeVoteRepository) {
		castVote(t, service, subject(), "up")
	}
	// unverifiedEntry: a plain create whose outcome is unknown, so the next
	// write would scan the PDS after its index lookup.
	unverifiedEntry := func(t *testing.T, service votes.Service, fake *fakePDS, _ *fakeVoteRepository) {
		fake.createErr = serverError("createRecord")
		_, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorIs(t, err, pds.ErrServerError, "test setup: the create's outcome is unknown")
		fake.createErr = nil
	}

	createVote := func(t *testing.T, service votes.Service) error {
		_, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		return err
	}
	deleteVote := func(t *testing.T, service votes.Service) error {
		return service.DeleteVote(context.Background(), voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: subject()})
	}

	for _, testCase := range []struct {
		name         string
		setup        func(t *testing.T, service votes.Service, fake *fakePDS, repository *fakeVoteRepository)
		failedMethod string
		write        func(t *testing.T, service votes.Service) error
	}{
		{"index query fails, CreateVote", indexedVote, repositoryGetActiveVotesForSubjects, createVote},
		{"index query fails, DeleteVote", indexedVote, repositoryGetActiveVotesForSubjects, deleteVote},
		{"confirmation query fails, CreateVote", votedEntry, repositoryGetIndexedStatesByURIs, createVote},
		{"confirmation query fails, DeleteVote", votedEntry, repositoryGetIndexedStatesByURIs, deleteVote},
		{"Unverified entry, index query fails, CreateVote", unverifiedEntry, repositoryGetActiveVotesForSubjects, createVote},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := newFakeVoteRepository(t)
			service := newService(t, fake, repository)
			testCase.setup(t, service, fake, repository)
			writesBefore := len(fake.ops)
			batchesBefore := fake.batchCalls
			listsBefore := fake.listCalls

			injected := errors.New("votes index unavailable")
			repository.failMethod(testCase.failedMethod, injected)
			err := testCase.write(t, service)
			require.ErrorIs(t, err, injected, "a database error fails the write; it is never read as \"no vote\"")
			require.Len(t, fake.ops, writesBefore, "no CreateRecord or DeleteRecord after a failed lookup")
			require.Equal(t, batchesBefore, fake.batchCalls, "no ApplyWrites after a failed lookup")
			require.Equal(t, listsBefore, fake.listCalls, "a database error never falls back to the PDS scan")
		})
	}
}

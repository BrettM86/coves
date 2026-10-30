package votes_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/require"
)

// The service's PDS scan and the error taxonomy of the write path.
//
// The votes index answers "has this user already voted on this?" for an
// ordinary write. The PDS is scanned only when this AppView's last write for
// the key had an unknown outcome (PRD §5.4), so the scan tests first leave
// the key in that state.
//
// The distinction that runs through the whole file is transient versus
// terminal. A vote is a write to somebody else's repository over a token that
// can expire mid-request, so "the PDS said no" has to be classified before it
// is acted on: an authorization failure means stop, anything else means the
// outcome is unknown and the next write must look at the PDS.

func TestCreateVote_FailedDirectionChangePreservesOriginalVote(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, newFakeVoteRepository(t))
		ctx := context.Background()
		up, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		<-time.After(time.Microsecond) // Distinct TIDs under the virtual clock.
		oldKey := fake.creates()[0]
		fake.createErr = errors.New("pds refused the create")
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.Error(t, err)
		require.Len(t, fake.records, 1, "a rejected replacement must leave the original PDS vote intact")
		require.Equal(t, "up", fake.records[oldKey].Direction)
		listed := fake.listCalls
		fake.createErr = nil
		down, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.NoError(t, err)
		require.NotEmpty(t, down.URI)
		require.NotEqual(t, up.URI, down.URI)
		require.Greater(t, fake.listCalls, listed, "retry must reload authoritative state")
		require.Len(t, fake.records, 1)
	})
}

func TestCreateVote_DirectionChangeReconcilesLostResponse(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	svc := newService(t, fake, newFakeVoteRepository(t))
	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.batchErr = context.DeadlineExceeded
	fake.commitBeforeError = true
	fake.afterBatch = cancel
	down, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
	require.NoError(t, err, "a replacement found on the PDS committed despite the lost response")
	require.Positive(t, fake.getCalls)
	require.NotEmpty(t, down.CID)
	require.Len(t, fake.records, 1)
}

func TestCreateVote_DirectionChangeUncertainReconciliationRereadsThePDS(t *testing.T) {
	t.Parallel()
	for _, mismatch := range []string{"unavailable", "subject", "direction", "uri", "empty cid", "subject cid", "type", "created at"} {
		t.Run(mismatch, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fake := newFakePDS(t, testVoterDID)
				svc := newService(t, fake, newFakeVoteRepository(t))
				_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
				require.NoError(t, err)
				<-time.After(time.Microsecond) // Distinct TIDs under the virtual clock.
				fake.batchErr = context.DeadlineExceeded
				fake.commitBeforeError = true
				switch mismatch {
				case "unavailable":
					fake.getErr = errors.New("PDS read unavailable")
				case "subject":
					fake.getTransform = func(r *pds.RecordResponse) {
						r.Value["subject"] = map[string]any{"uri": "at://did:plc:other/social.coves.community.post/other", "cid": testSubjectCI}
					}
				case "direction":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["direction"] = "up" }
				case "uri":
					fake.getTransform = func(r *pds.RecordResponse) { r.URI = "at://" + testVoterDID + "/social.coves.feed.vote/another" }
				case "empty cid":
					fake.getTransform = func(r *pds.RecordResponse) { r.CID = "" }
				case "subject cid":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["subject"].(map[string]any)["cid"] = "different" }
				case "type":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["$type"] = "social.coves.feed.other" }
				case "created at":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["createdAt"] = "different" }

				}
				_, err = svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
				require.Error(t, err, "uncertain or mismatched state must not be reported as success")
				listed := fake.listCalls
				fake.batchErr = nil
				fake.getErr = nil
				fake.getTransform = nil
				batches := fake.batchCalls
				response, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
				require.NoError(t, err)
				require.Greater(t, fake.listCalls, listed, "the next tap must reload authoritative PDS state")
				// D6: the retry repeats the ambiguous switch, whose result already holds.
				require.Len(t, fake.records, 1, "the committed downvote stays")
				require.Equal(t, "at://"+testVoterDID+"/social.coves.feed.vote/"+fake.creates()[1], response.URI,
					"the retry answers with the committed downvote")
				require.Len(t, fake.ops, 3, "the retry makes no PDS write")
				require.Equal(t, batches, fake.batchCalls, "the retry makes no PDS write")
			})
		})
	}
}

func TestFindExistingVote_PaginatesPastTheFirstPage(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	fake.pageSize = 2
	subjects := seedVotes(fake, 5)

	svc := newService(t, fake, newFakeVoteRepository(t))

	// The last-seeded vote is on the final page. A reader that stops after one
	// page never sees it.
	last := subjects[len(subjects)-1]
	leaveUnverified(t, svc, fake, votes.StrongRef{URI: last, CID: "bafyreilast"})
	// A DeleteVote, not a retry of the ambiguous CreateVote (D6), so finding
	// the vote means deleting it.
	err := svc.DeleteVote(context.Background(), voter(t, testVoterDID),
		votes.DeleteVoteRequest{Subject: votes.StrongRef{URI: last, CID: "bafyreilast"}})
	require.NoError(t, err)

	require.Equal(t, []string{"3ktide"}, fake.deletes(),
		"the existing vote is on the last page of the repo and the service did not find it. A vote repo "+
			"passes 100 records for any regular user, so this is the common case, not the edge")
}

func TestFindExistingVote_IgnoresRecordsItCannotRead(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	fake.rawRecords = []pds.RecordEntry{
		{
			URI:   "at://" + testVoterDID + "/social.coves.feed.vote/3kbad1",
			Value: map[string]any{"direction": "up", "subject": "not-an-object"},
		},
		{
			URI:   "at://" + testVoterDID + "/social.coves.feed.vote/3kbad2",
			Value: map[string]any{"direction": "up", "subject": map[string]any{"uri": 42}},
		},
		{
			URI:   "short-uri",
			Value: map[string]any{"direction": "up", "subject": map[string]any{"uri": testSubject}},
		},
	}
	svc := newService(t, fake, newFakeVoteRepository(t))
	leaveUnverified(t, svc, fake, subject())

	// The third raw record DOES name the subject under test, but its URI has
	// too few segments to yield an rkey. Acting on it would mean issuing a
	// delete with an empty record key.
	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	require.Empty(t, fake.deletes(),
		"the service tried to delete a record whose URI it could not parse an rkey out of — which "+
			"deletes nothing at best and the wrong key at worst")
	require.Len(t, fake.creates(), 1, "with no readable existing vote, this is a first vote")
}

func TestVoteService_ErrorTaxonomy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("an expired session on the create path is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		fake.createErr = pds.ErrSessionExpired
		svc := newService(t, fake, newFakeVoteRepository(t))

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized,
			"the handler maps this to a 401 so the client can refresh and retry; a generic error would "+
				"be a 500 and the client would give up on a vote that a new token would have accepted")
	})

	t.Run("an expired session on the toggle-off path is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, newFakeVoteRepository(t))
		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		fake.deleteErr = pds.ErrUnauthorized
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized)
	})

	t.Run("an expired session on a direction change is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, newFakeVoteRepository(t))
		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		fake.batchErr = pds.ErrForbidden
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized)
		require.Equal(t, 1, fake.batchCalls)
		require.Len(t, fake.creates(), 1,
			"a direction change whose delete was refused must not create the replacement: the repo would "+
				"then hold both directions for one subject")
	})

	t.Run("an expired session on the delete path is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, newFakeVoteRepository(t))
		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		fake.deleteErr = pds.ErrSessionExpired
		require.ErrorIs(t, svc.DeleteVote(ctx, voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: subject()}), votes.ErrNotAuthorized)
	})

	t.Run("a PDS client that cannot be built fails the request", func(t *testing.T) {
		t.Parallel()
		svc := votes.NewServiceWithPDSFactory(newFakeVoteRepository(t), nil,
			func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) {
				return nil, errors.New("dpop key rotated")
			})

		_, createErr := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorContains(t, createErr, "dpop key rotated",
			"the reason the session could not be used must survive into the error, or every auth "+
				"misconfiguration looks identical in the logs")

		deleteErr := svc.DeleteVote(ctx, voter(t, testVoterDID), votes.DeleteVoteRequest{Subject: subject()})
		require.ErrorContains(t, deleteErr, "dpop key rotated")
	})

	t.Run("the production constructor refuses to act without an OAuth client", func(t *testing.T) {
		t.Parallel()
		// NewService is what cmd/server calls. With no OAuth client wired it
		// cannot build a DPoP session for anybody, and the failure has to be an
		// error rather than a nil-pointer panic taking the process down on the
		// first vote after a bad deploy.
		svc := votes.NewService(newFakeVoteRepository(t), nil, nil, nil)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorContains(t, err, "OAuth client not configured")
	})
}

func TestDeleteVote_RejectsMalformedSubjects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		req  votes.DeleteVoteRequest
	}{
		{"empty subject URI", votes.DeleteVoteRequest{Subject: votes.StrongRef{CID: testSubjectCI}}},
		{"subject URI that is not an AT-URI", votes.DeleteVoteRequest{
			Subject: votes.StrongRef{URI: "https://example.com/post/1", CID: testSubjectCI},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			svc := newService(t, fake, newFakeVoteRepository(t))

			err := svc.DeleteVote(context.Background(), voter(t, testVoterDID), tc.req)
			require.ErrorIs(t, err, votes.ErrInvalidSubject)
			require.Zero(t, fake.listCalls,
				"a subject the service cannot parse must be rejected before the repo is read: a "+
					"pagination over every vote a user has ever cast is not the way to answer a 400")
		})
	}

	// Unlike CreateVote, a withdrawal does not need a CID — it names a record
	// that already exists rather than making a strong reference to a subject.
	t.Run("a missing CID is accepted", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, newFakeVoteRepository(t))
		_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		require.NoError(t, svc.DeleteVote(context.Background(), voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: votes.StrongRef{URI: testSubject}}))
	})
}

// leaveUnverified makes a vote on the subject fail with an ambiguous PDS
// error, so the outcome of the key's last write is unknown and the next write
// scans the PDS (PRD §5.4). Both plain writes fail, whichever one the service
// attempts, and neither commits.
func leaveUnverified(t *testing.T, service votes.Service, fake *fakePDS, subject votes.StrongRef) {
	t.Helper()
	fake.createErr = errors.New("pds timed out")
	fake.deleteErr = errors.New("pds timed out")
	_, err := service.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject, Direction: "up"})
	require.Error(t, err, "test setup: the ambiguous write must fail")
	require.Empty(t, fake.ops, "test setup: the ambiguous write must not commit")
	fake.createErr = nil
	fake.deleteErr = nil
}

// seedVotes writes n "up" votes straight into the fake PDS, as another client
// would, and returns their subjects in rkey order.
func seedVotes(fake *fakePDS, n int) []string {
	subjects := make([]string, 0, n)
	for i := 0; i < n; i++ {
		subjectURI := "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kseed" + string(rune('a'+i))
		rkey := "3ktid" + string(rune('a'+i))
		fake.records[rkey] = votes.VoteRecord{
			Type:      "social.coves.feed.vote",
			Direction: "up",
			Subject:   votes.StrongRef{URI: subjectURI, CID: "bafyseed" + string(rune('a'+i))},
			CreatedAt: "2026-10-28T00:00:00Z",
		}
		subjects = append(subjects, subjectURI)
	}
	return subjects
}

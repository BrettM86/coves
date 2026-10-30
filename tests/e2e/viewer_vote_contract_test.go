//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// viewerVoteState is the vote half of the viewer object a signed-in reader gets
// on a post view and on a comment view. Both fields are omitted when the viewer
// has no vote, so "no vote" is the zero value. Pointers keep an omitted field
// distinct from an empty string.
type viewerVoteState struct {
	Vote    *string `json:"vote"`
	VoteURI *string `json:"voteUri"`
}

// votedState is the viewer state of a subject the viewer has voted on.
func votedState(direction, voteURI string) viewerVoteState {
	return viewerVoteState{Vote: &direction, VoteURI: &voteURI}
}

// observedViewerVote reads the vote half of a view's viewer object. A view with
// no viewer object at all has no vote.
func observedViewerVote(viewer *viewerVoteState) viewerVoteState {
	if viewer == nil {
		return viewerVoteState{}
	}
	return *viewer
}

// voteCounts are the indexed totals the vote consumer maintains on a subject.
// They are this contract's indexing signal because nothing else writes them:
// they move only when Jetstream has delivered the vote commits.
type voteCounts struct {
	Upvotes   int
	Downvotes int
	Score     int
}

// voteSubjectObservation is one subject as a signed-in viewer reads it.
type voteSubjectObservation struct {
	Counts voteCounts
	Viewer viewerVoteState
}

// observeVoteSubjects reads every subject of one surface in a single request,
// as the client reader carries. A subject missing from the response is a
// terminal error: every subject was indexed before the first vote, and a vote
// cannot remove its subject.
type observeVoteSubjects func(ctx context.Context, reader *pipeline) (map[string]voteSubjectObservation, error)

// pdsVote is a vote record as it sits in the voter's repo.
type pdsVote struct {
	URI       string
	Direction string
}

// voteRecordsBySubject lists every social.coves.feed.vote record in account's
// repo, straight from its PDS, grouped by subject URI.
func voteRecordsBySubject(t *testing.T, account *testkit.Account) map[string][]pdsVote {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	records := map[string][]pdsVote{}
	cursor := ""
	for {
		params := url.Values{"repo": {account.DID}, "collection": {voteCollection}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var page struct {
			Records []struct {
				URI   string `json:"uri"`
				Value struct {
					Subject   strongRef `json:"subject"`
					Direction string    `json:"direction"`
				} `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}
		require.NoError(t, account.XRPC().Query(ctx, "com.atproto.repo.listRecords", params, &page),
			"listing the viewer's vote records on its PDS")
		for _, record := range page.Records {
			subject := record.Value.Subject.URI
			records[subject] = append(records[subject], pdsVote{URI: record.URI, Direction: record.Value.Direction})
		}
		if page.Cursor == "" || len(page.Records) == 0 {
			return records
		}
		cursor = page.Cursor
	}
}

// TestViewerVoteStateContract is the end-to-end contract for a signed-in
// viewer's own vote state (docs/PRD_VOTE_STATE_FROM_INDEX.md §7, "End-to-end").
// It carries NO ingestion marker: TestVoteIngestion owns social.coves.feed.vote.
//
// For a post and for a comment, one signed-in viewer votes through the
// AppView's social.coves.feed.vote.create and .delete:
//
//	subject A: like → unlike → like
//	subject B: like → switch direction
//	subject C: a vote written straight to the viewer's PDS, never through the AppView
//
// # IMMEDIATELY, AND THEN AFTER INDEXING
//
// After every AppView write the subject is read back at once, with no wait, and
// viewer.vote / viewer.voteUri must already name the vote the write produced
// (PRD §2: "a vote cast through this AppView shows as cast on the next read").
// Usually Jetstream has not indexed the write by then, but nothing here depends
// on that; the expected state is the same whether or not it has.
//
// Then the contract waits for indexing on a signal that does not involve viewer
// state: the subjects' indexed counts. The counts on A alone cannot say that A
// is fully indexed, because like → unlike → like shows one upvote both after the
// first like and after the last. So the wait covers all three subjects together.
// A, B and C are written in that order to the viewer's one repo, Jetstream
// delivers a repo's commits in order, and B's {0, 1, -1} and C's {1, 0, 1}
// cannot appear until B's replacement and C's create are indexed, which is after
// all of A's commits. Once the counts settle, viewer state must still match,
// now from the index, and the viewer's PDS must hold exactly the expected vote
// record per subject.
//
// # RATE LIMITS
//
// Each surface runs in its own subtest with its own pipeline, so its reads
// spend from a fresh per-IP bucket (newPipeline keys the bucket on t.Name()).
// The comment surface reads social.coves.community.comment.getComments, which
// allows 20 requests per minute; one read covers all three comments, and the
// waits use withReadCadence. The viewer signs in once, on the parent pipeline.
func TestViewerVoteStateContract(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "vsa")
	viewer := p.IndexedAccount(t, "vsv")
	community := indexedCommunity(t, p, "vs", author.DID)
	token := p.AppView.SignIn(t, viewer)

	surfaces := []struct {
		name string
		// setUp creates three fresh, indexed subjects (A, B, C) and returns them
		// with the read that observes all three.
		setUp   func(t *testing.T, p *pipeline) ([3]strongRef, observeVoteSubjects)
		options []testkit.WaitOption
	}{
		{
			name: "post",
			setUp: func(t *testing.T, p *pipeline) ([3]strongRef, observeVoteSubjects) {
				var subjects [3]strongRef
				for i := range subjects {
					subjects[i] = indexedPost(t, p, community, author,
						fmt.Sprintf("viewer vote subject %d %s", i, testkit.UniqueID(t)))
				}
				observe := func(ctx context.Context, reader *pipeline) (map[string]voteSubjectObservation, error) {
					uris := []string{subjects[0].URI, subjects[1].URI, subjects[2].URI}
					views, err := reader.Posts(ctx, uris...)
					if err != nil {
						return nil, err
					}
					if len(views) != len(uris) {
						return nil, fmt.Errorf("social.coves.community.post.get answered %d results for %d requested URIs",
							len(views), len(uris))
					}
					observed := map[string]voteSubjectObservation{}
					for i, view := range views {
						if view.NotFound {
							return nil, errPostVanished(uris[i])
						}
						observed[view.URI] = voteSubjectObservation{
							Counts: voteCounts{Upvotes: view.Stats.Upvotes, Downvotes: view.Stats.Downvotes, Score: view.Stats.Score},
							Viewer: observedViewerVote(view.Viewer),
						}
					}
					return observed, nil
				}
				return subjects, observe
			},
		},
		{
			name: "comment",
			setUp: func(t *testing.T, p *pipeline) ([3]strongRef, observeVoteSubjects) {
				host := indexedPost(t, p, community, author, "viewer vote thread "+testkit.UniqueID(t))
				var subjects [3]strongRef
				for i := range subjects {
					record := author.PutRecord(t, commentCollection, testkit.TID(),
						commentRecord(host, host, fmt.Sprintf("viewer vote subject %d", i)))
					subjects[i] = strongRef{URI: record.URI, CID: record.CID}
				}
				p.Await(t, "the three comments to be voted on to be indexed", func() (bool, error) {
					thread, err := p.Thread(context.Background(), host.URI, nil)
					if err != nil {
						return false, err
					}
					for _, subject := range subjects {
						if _, found := thread.find(subject.URI); !found {
							return false, nil
						}
					}
					return true, nil
				}, withReadCadence())

				observe := func(ctx context.Context, reader *pipeline) (map[string]voteSubjectObservation, error) {
					thread, err := reader.Thread(ctx, host.URI, nil)
					if err != nil {
						return nil, err
					}
					observed := map[string]voteSubjectObservation{}
					for _, subject := range subjects {
						node, found := thread.find(subject.URI)
						if !found {
							return nil, fmt.Errorf("the indexed comment %s is missing from its thread; a vote cannot "+
								"remove its subject. The thread held: %s", subject.URI, thread.uris())
						}
						stats := node.Comment.Stats
						observed[subject.URI] = voteSubjectObservation{
							Counts: voteCounts{Upvotes: stats.Upvotes, Downvotes: stats.Downvotes, Score: stats.Score},
							Viewer: observedViewerVote(node.Comment.Viewer),
						}
					}
					return observed, nil
				}
				return subjects, observe
			},
			options: []testkit.WaitOption{withReadCadence()},
		},
	}

	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			sp := newPipeline(t)
			subjects, observe := surface.setUp(t, sp)
			likedAgain, switched, direct := subjects[0], subjects[1], subjects[2]
			reader := &pipeline{PDS: sp.PDS, AppView: sp.AppView.As(token), clientIP: sp.clientIP}

			ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
			defer cancel()

			createVote := func(t *testing.T, subject strongRef, direction string) string {
				t.Helper()
				var created struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				}
				require.NoError(t, reader.AppView.Procedure(ctx, "social.coves.feed.vote.create", map[string]any{
					"subject":   map[string]any{"uri": subject.URI, "cid": subject.CID},
					"direction": direction,
				}, &created), "the signed-in viewer's vote.create (%s) on %s", direction, subject.URI)
				require.NotEmpty(t, created.URI,
					"vote.create (%s) on a subject without that vote must answer with the new vote's URI", direction)
				return created.URI
			}
			deleteVote := func(t *testing.T, subject strongRef) {
				t.Helper()
				require.NoError(t, reader.AppView.Procedure(ctx, "social.coves.feed.vote.delete", map[string]any{
					"subject": map[string]any{"uri": subject.URI, "cid": subject.CID},
				}, nil), "the signed-in viewer's vote.delete on %s", subject.URI)
			}
			// immediately reads the subject right after a write, with no wait.
			immediately := func(t *testing.T, subject strongRef, want viewerVoteState, step string) {
				t.Helper()
				observed, err := observe(ctx, reader)
				require.NoError(t, err)
				require.Equal(t, want, observed[subject.URI].Viewer,
					"%s %s: the very next read by the signed-in viewer must show viewer.vote and viewer.voteUri "+
						"for the write just made, whether or not Jetstream has indexed it", surface.name, step)
			}

			// ---- A: like → unlike → like ---------------------------------------
			firstLike := createVote(t, likedAgain, "up")
			immediately(t, likedAgain, votedState("up", firstLike), "A, after like")

			deleteVote(t, likedAgain)
			immediately(t, likedAgain, viewerVoteState{}, "A, after unlike")

			secondLike := createVote(t, likedAgain, "up")
			immediately(t, likedAgain, votedState("up", secondLike), "A, after liking again")

			// ---- B: like → switch direction ------------------------------------
			upvote := createVote(t, switched, "up")
			immediately(t, switched, votedState("up", upvote), "B, after like")

			downvote := createVote(t, switched, "down")
			immediately(t, switched, votedState("down", downvote), "B, after switching to down")

			// ---- C: a vote written straight to the PDS --------------------------
			directVote := viewer.PutRecord(t, voteCollection, testkit.TID(), voteRecord(direct, "up"))

			// ---- after indexing --------------------------------------------------
			want := map[string]voteCounts{
				likedAgain.URI: {Upvotes: 1, Downvotes: 0, Score: 1},
				switched.URI:   {Upvotes: 0, Downvotes: 1, Score: -1},
				direct.URI:     {Upvotes: 1, Downvotes: 0, Score: 1},
			}
			var indexed map[string]voteSubjectObservation
			reader.Await(t, "the viewer's vote commits on all three "+surface.name+"s to reach their indexed counts",
				func() (bool, error) {
					observed, err := observe(context.Background(), reader)
					if err != nil {
						return false, err
					}
					indexed = observed
					for uri, counts := range want {
						if observed[uri].Counts != counts {
							return false, nil
						}
					}
					return true, nil
				}, surface.options...)

			require.Equal(t, votedState("up", secondLike), indexed[likedAgain.URI].Viewer,
				"%s A after indexing: like → unlike → like must show the second like's record", surface.name)
			require.Equal(t, votedState("down", downvote), indexed[switched.URI].Viewer,
				"%s B after indexing: like → switch must show the downvote's record", surface.name)
			require.Equal(t, votedState("up", directVote.URI), indexed[direct.URI].Viewer,
				"%s C: a vote written straight to the PDS must appear in the viewer's state once indexed, "+
					"with the URI the PDS committed it under", surface.name)

			records := voteRecordsBySubject(t, viewer)
			require.Equal(t, []pdsVote{{URI: secondLike, Direction: "up"}}, records[likedAgain.URI],
				"%s A: the viewer's PDS must hold exactly the second like, the first having been deleted by the unlike",
				surface.name)
			require.Equal(t, []pdsVote{{URI: downvote, Direction: "down"}}, records[switched.URI],
				"%s B: the viewer's PDS must hold exactly the downvote, the upvote having been replaced", surface.name)
			require.Equal(t, []pdsVote{{URI: directVote.URI, Direction: "up"}}, records[direct.URI],
				"%s C: the viewer's PDS must hold exactly the directly written vote", surface.name)
		})
	}
}

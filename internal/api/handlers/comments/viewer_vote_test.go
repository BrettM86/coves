package comments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"
	core "Coves/internal/core/comments"
	"Coves/internal/core/votes"
	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

type commentVoteReader struct {
	votes.Service
	populationError error
	populationCalls int
	lookupCalls     int
	subjects        []string
	viewer          string
}

func (s *commentVoteReader) EnsureCachePopulated(ctx context.Context, session *oauthlib.ClientSessionData) error {
	s.populationCalls++
	if s.populationError != nil {
		return s.populationError
	}
	return s.Service.EnsureCachePopulated(ctx, session)
}
func (s *commentVoteReader) GetViewerVotesForSubjects(viewer string, subjects []string) map[string]*votes.CachedVote {
	s.lookupCalls++
	s.viewer = viewer
	s.subjects = subjects
	return s.Service.GetViewerVotesForSubjects(viewer, subjects)
}

func TestGetComments_CachedViewerVotes(t *testing.T) {
	const viewer = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	const rootURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/root"
	const replyURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/reply"
	const nestedURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/nested"
	for _, scenario := range []struct {
		name            string
		anonymous       bool
		noSession       bool
		noCache         bool
		loneVote        bool
		fetch           bool
		populationError error
		preserve        bool
	}{
		{name: "cached directions and toggle off override stale index"},
		{name: "lone write and failed fetch preserves indexed votes", loneVote: true, preserve: true},
		{name: "empty cache fetches PDS votes", fetch: true},
		{name: "anonymous", anonymous: true, preserve: true},
		{name: "DID without OAuth session", noSession: true, preserve: true},
		{name: "cache disabled", noCache: true, preserve: true},
		{name: "cache population failed", populationError: errors.New("PDS unavailable"), preserve: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			up, oldURI := "up", "at://"+viewer+"/social.coves.feed.vote/old"
			stale := func(uri string) *core.CommentView {
				return &core.CommentView{URI: uri, Viewer: &core.CommentViewerState{Vote: &up, VoteURI: &oldURI}, Stats: &core.CommentStats{Upvotes: 7, Downvotes: 2, Score: 5}}
			}
			root, reply, nested := stale(rootURI), stale(replyURI), &core.CommentView{URI: nestedURI}
			cursor := "next-page"
			response := &core.GetCommentsResponse{Post: map[string]string{"uri": testPostURI}, Cursor: &cursor, Comments: []*core.ThreadViewComment{
				{Comment: root, HasMore: true, Replies: []*core.ThreadViewComment{
					nil, {Comment: reply, Replies: []*core.ThreadViewComment{{Comment: nil, Replies: []*core.ThreadViewComment{{Comment: nested}}}}},
				}},
			}}
			cache := votes.NewVoteCache(time.Hour, nil)
			seed := map[string]*votes.CachedVote{
				rootURI:   {Direction: "down", URI: "at://" + viewer + "/social.coves.feed.vote/new"},
				nestedURI: {Direction: "up", URI: "at://" + viewer + "/social.coves.feed.vote/nested"},
			}
			if !scenario.fetch && !scenario.loneVote {
				cache.SetVotesForUser(viewer, seed)
			}
			if scenario.loneVote {
				cache.SetVote(viewer, rootURI, seed[rootURI])
			}
			if scenario.noCache {
				cache = nil
			}
			client := &commentVotePDS{viewer: viewer, seed: seed}
			if scenario.loneVote {
				client.err = errors.New("PDS unavailable")
			}
			factory := func(context.Context, *oauthlib.ClientSessionData) (pds.Client, error) { return client, nil }
			reader := &commentVoteReader{Service: votes.NewServiceWithPDSFactory(nil, cache, nil, factory), populationError: scenario.populationError}
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.comment.getComments?post="+testPostURI+"&parentRkey=root&viewerDID=did:plc:other", nil)
			if !scenario.anonymous {
				ctx := middleware.SetTestUserDID(request.Context(), viewer)
				if !scenario.noSession {
					ctx = middleware.SetTestOAuthSession(ctx, &oauthlib.ClientSessionData{AccountDID: syntax.DID(viewer)})
				}
				request = request.WithContext(ctx)
			}
			recorder := httptest.NewRecorder()
			NewGetCommentsHandler(&fakeCommentService{resp: response}, reader).HandleGetComments(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code)
			var got core.GetCommentsResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
			gotRoot := got.Comments[0].Comment
			gotReply := got.Comments[0].Replies[1].Comment
			gotNested := got.Comments[0].Replies[1].Replies[0].Replies[0].Comment
			require.Equal(t, &cursor, got.Cursor)
			require.True(t, got.Comments[0].HasMore)
			require.Equal(t, &core.CommentStats{Upvotes: 7, Downvotes: 2, Score: 5}, gotRoot.Stats)
			if scenario.preserve {
				require.Equal(t, &up, gotRoot.Viewer.Vote)
				require.Equal(t, &oldURI, gotReply.Viewer.VoteURI)
				require.Nil(t, gotNested.Viewer)
			} else {
				require.NotNil(t, gotRoot.Viewer)
				require.Equal(t, "down", *gotRoot.Viewer.Vote)
				require.Equal(t, "at://"+viewer+"/social.coves.feed.vote/new", *gotRoot.Viewer.VoteURI)
				require.Nil(t, gotReply.Viewer, "confirmed absence omits empty viewer state")
				require.NotNil(t, gotNested.Viewer)
				require.Equal(t, "up", *gotNested.Viewer.Vote)
				require.Equal(t, viewer, reader.viewer)
				require.ElementsMatch(t, []string{rootURI, replyURI, nestedURI}, reader.subjects)
				require.Equal(t, 1, reader.populationCalls)
				require.Equal(t, 1, reader.lookupCalls)
			}
			if scenario.fetch || scenario.loneVote {
				require.Equal(t, 1, client.calls)
			}
			if scenario.anonymous || scenario.noSession {
				require.Zero(t, reader.populationCalls)
			}
			if scenario.populationError != nil {
				require.Zero(t, reader.lookupCalls)
			}
		})
	}
}

// Only ListRecords is used by this read handler; embedded methods fail if called.
type commentVotePDS struct {
	pds.Client
	viewer string
	seed   map[string]*votes.CachedVote
	err    error
	calls  int
}

func (p *commentVotePDS) DID() string     { return p.viewer }
func (p *commentVotePDS) HostURL() string { return "https://pds.invalid" }
func (p *commentVotePDS) ListRecords(context.Context, string, int, string) (*pds.ListRecordsResponse, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	result := &pds.ListRecordsResponse{}
	for subject, vote := range p.seed {
		result.Records = append(result.Records, pds.RecordEntry{URI: vote.URI, Value: map[string]any{"subject": map[string]any{"uri": subject}, "direction": vote.Direction}})
	}
	return result, nil
}

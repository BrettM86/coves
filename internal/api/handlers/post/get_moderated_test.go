package post

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleGetModeratedPostUnionEncoding(t *testing.T) {
	uri := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.postv2/abc123"
	handler := NewGetHandler(&mockGetPostService{getPostsFunc: func(_ context.Context, _ posts.GetPostsRequest) ([]*posts.PostResult, error) {
		return []*posts.PostResult{{Moderated: &posts.ModeratedPost{
			URI: uri, AuthorDID: "did:plc:postauthor",
			Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.test", Name: "community"},
			Moderation: &posts.ModerationView{State: "removed", Sources: []posts.ModerationSourceView{{
				AuthorityDID: "did:web:instance.test", Scope: posts.ModerationScopeView{Kind: "instance"},
			}}},
		}}}, nil
	}}, nil, nil)
	recorder := httptest.NewRecorder()
	handler.HandleGet(recorder, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.post.get?"+url.Values{"uris": {uri}}.Encode(), nil))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var body struct {
		Posts []map[string]any `json:"posts"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.Posts, 1)
	item := body.Posts[0]
	assert.Equal(t, "social.coves.community.post.defs#moderatedPost", item["$type"])
	assert.Equal(t, uri, item["uri"])
	assert.Equal(t, "did:plc:postauthor", item["authorDid"])
	assert.Equal(t, map[string]any{
		"state":   "removed",
		"sources": []any{map[string]any{"authorityDid": "did:web:instance.test", "scope": map[string]any{"kind": "instance"}}},
	}, item["moderation"])
	assert.Equal(t, map[string]any{"did": "did:plc:community", "handle": "community.test", "name": "community"}, item["community"])
	for _, key := range []string{"record", "title", "embed", "cid", "content", "notFound", "removed", "blocked"} {
		assert.NotContains(t, item, key, "a moderated tombstone cannot leak post content or claim another union member")
	}
}

func TestHandleGetExistingTombstoneUnionEncoding(t *testing.T) {
	uri := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/abc123"
	for _, scenario := range []struct {
		name, expected string
		result         *posts.PostResult
	}{
		{"not found", `{"uri":"` + uri + `","notFound":true}`, &posts.PostResult{NotFound: &posts.NotFoundPost{URI: uri, NotFound: true}}},
		{"community removed", `{"uri":"` + uri + `","removed":true,"code":"rule-violation"}`, &posts.PostResult{Removed: &posts.RemovedPost{URI: uri, Removed: true, Code: "rule-violation"}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			handler := NewGetHandler(&mockGetPostService{getPostsFunc: func(_ context.Context, _ posts.GetPostsRequest) ([]*posts.PostResult, error) {
				return []*posts.PostResult{scenario.result}, nil
			}}, nil, nil)
			recorder := httptest.NewRecorder()
			handler.HandleGet(recorder, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.post.get?"+url.Values{"uris": {uri}}.Encode(), nil))
			require.Equal(t, http.StatusOK, recorder.Code)
			assert.JSONEq(t, `{"posts":[`+scenario.expected+`]}`, recorder.Body.String())
		})
	}
}

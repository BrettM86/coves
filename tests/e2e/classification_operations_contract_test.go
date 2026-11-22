//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	enqueueClassificationMethod             = "social.coves.moderation.enqueueClassification"
	getCommunityClassificationSummaryMethod = "social.coves.moderation.getCommunityClassificationSummary"
)

type enqueueClassificationResponse struct {
	Created        int `json:"created"`
	Revived        int `json:"revived"`
	AlreadyPresent int `json:"alreadyPresent"`
}

// TestClassificationOperationsAPIContract: on the hermetic stack with the
// classifier configured, a post queued at indexing is reported present, not
// duplicated, by two postsAndComments re-enqueues over a range holding only
// that post; the community summary and an allCommunities re-enqueue answer
// lexicon-valid; a non-admin is forbidden both endpoints (B10). It asserts
// queueing only, never a classification result.
func TestClassificationOperationsAPIContract(t *testing.T) {
	p := newPipeline(t)
	admin := testkit.ModerationAdmin(t, 1)
	nonAdmin := p.IndexedAccount(t, "clsopsoutsider")
	author := p.IndexedAccount(t, "clsopsauthor")
	community := indexedCommunity(t, p, "cop", author.DID)
	post := indexedPost(t, p, community, author, "operations contract "+testkit.UniqueID(t))

	view, err := p.Post(t.Context(), post.URI)
	require.NoError(t, err)
	require.False(t, view.NotFound, "the indexed post is served")
	since := view.IndexedAt.UTC()
	rangeInput := map[string]any{
		"target": "postsAndComments",
		"since":  since.Format(time.RFC3339Nano),
		"until":  since.Add(time.Millisecond).Format(time.RFC3339Nano),
	}
	summaryParams := url.Values{
		"community":      {community.DID},
		"question":       {"spam_scam"},
		"minProbability": {"500000"},
	}

	for _, run := range []string{"first", "repeat"} {
		t.Run("admin "+run+" postsAndComments re-enqueue over the post's range", func(t *testing.T) {
			token := admin.ServiceAuth(t, communityInstanceDID, enqueueClassificationMethod)
			var body json.RawMessage
			require.NoError(t, p.AppView.As(token).Procedure(t.Context(), enqueueClassificationMethod, rangeInput, &body))
			requireClassificationLexiconOutput(t, body, enqueueClassificationMethod)
			var response enqueueClassificationResponse
			require.NoError(t, json.Unmarshal(body, &response))
			assert.Equal(t, enqueueClassificationResponse{Created: 0, AlreadyPresent: 1}, response,
				"the post was queued at indexing, so it is present and nothing is created")
		})
	}

	t.Run("admin community summary", func(t *testing.T) {
		token := admin.ServiceAuth(t, communityInstanceDID, getCommunityClassificationSummaryMethod)
		var body json.RawMessage
		require.NoError(t, p.AppView.As(token).Query(t.Context(), getCommunityClassificationSummaryMethod, summaryParams, &body))
		requireClassificationLexiconOutput(t, body, getCommunityClassificationSummaryMethod)
	})

	t.Run("admin allCommunities re-enqueue", func(t *testing.T) {
		token := admin.ServiceAuth(t, communityInstanceDID, enqueueClassificationMethod)
		var body json.RawMessage
		require.NoError(t, p.AppView.As(token).Procedure(t.Context(), enqueueClassificationMethod,
			map[string]any{"target": "allCommunities"}, &body))
		requireClassificationLexiconOutput(t, body, enqueueClassificationMethod)
	})

	for _, call := range []struct {
		name   string
		method string
		invoke func(ctx context.Context, client *testkit.AppView) error
	}{
		{
			name:   "re-enqueue",
			method: enqueueClassificationMethod,
			invoke: func(ctx context.Context, client *testkit.AppView) error {
				return client.Procedure(ctx, enqueueClassificationMethod, rangeInput, nil)
			},
		},
		{
			name:   "summary",
			method: getCommunityClassificationSummaryMethod,
			invoke: func(ctx context.Context, client *testkit.AppView) error {
				return client.Query(ctx, getCommunityClassificationSummaryMethod, summaryParams, nil)
			},
		},
	} {
		t.Run("non-admin "+call.name+" is forbidden", func(t *testing.T) {
			token := nonAdmin.ServiceAuth(t, communityInstanceDID, call.method)
			err := call.invoke(t.Context(), p.AppView.As(token))
			_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Forbidden", "a non-admin's "+call.name+" call")
		})
	}
}

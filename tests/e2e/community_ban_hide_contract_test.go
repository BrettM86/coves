//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommunityBanDirectWriteHidden proves the ban covers records the banned
// account writes straight to its own PDS, bypassing comment.create's refusal.
// The community writes a ban on A to its repo; once the AppView has indexed it,
// A puts a comment on another author's post in the community directly into A's
// repo. getComments serves that comment as a content-free placeholder.
func TestCommunityBanDirectWriteHidden(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "bhb")
	banned := p.IndexedAccount(t, "bha")
	community := indexedCommunity(t, p, "bh", author.DID)
	bannedView := p.AppView.As(p.AppView.SignIn(t, banned))

	post := indexedPost(t, p, community, author, "a post the banned account writes to directly")

	community.PutRecord(t, banCollection, banRecordKey(banned.DID), map[string]any{
		"$type":     banCollection,
		"subject":   banned.DID,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
		"reason":    "spam",
	})
	p.Await(t, "the community's ban record to reach the banned account's viewer state", func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		var response map[string]any
		if err := bannedView.Query(ctx, "social.coves.community.get",
			url.Values{"community": {community.DID}}, &response); err != nil {
			return false, err
		}
		viewer, ok := response["viewer"].(map[string]any)
		if !ok {
			return false, fmt.Errorf("community.get as a signed-in account served no viewer object: %v", response["viewer"])
		}
		return viewer["banned"] == true, nil
	})

	const hiddenText = "a comment the banned account wrote straight to its PDS"
	hiddenRKey := testkit.TID()
	hiddenAt := commentURI(banned.DID, hiddenRKey)
	banned.PutRecord(t, commentCollection, hiddenRKey, commentRecord(post, post, hiddenText))

	p.FreshReadQuota(t, "community-ban-direct-write")
	var hidden threadNode
	p.Await(t, "the banned account's directly written comment to be served as a placeholder", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		node, ok := thread.find(hiddenAt)
		if !ok {
			return false, nil
		}
		hidden = node
		return node.Comment.IsDeleted, nil
	}, withReadCadence())

	assert.True(t, hidden.Comment.IsDeleted)
	assert.Nil(t, hidden.Comment.Record, "a ban-hidden comment's record must not be served")
	served, err := json.Marshal(hidden)
	require.NoError(t, err)
	assert.NotContains(t, string(served), hiddenText, "a ban-hidden comment's text must not be served")
}

//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const banCollection = "social.coves.moderation.ban"

// banRecordKey is the record key the ban lexicon fixes for a subject: the
// unpadded, lowercase RFC 4648 base32 of SHA-256 over the subject DID, so a
// community holds at most one ban record per account.
func banRecordKey(subjectDID string) string {
	digest := sha256.Sum256([]byte(subjectDID))
	return strings.ToLower(subjectRkeyEncoding.EncodeToString(digest[:]))
}

// TestCommunityBanIngestion is the pipeline proof for the community ban record.
//
// coves:ingestion-contract social.coves.moderation.ban
//
//	ban    → the community writes a ban on A to its own repo; A's community.get
//	         shows viewer.banned with the record's URI and reason
//	refuse → A's post.create, comment.create (on another author's post in the
//	         community) and vote.create (on that post) all answer 403 Banned
//	unban  → the community deletes the record; viewer.banned is gone and A's
//	         post.create is accepted again
//
// The three refusals are the production wiring of each write path's ban gate:
// cmd/server must hand the ban index to the post, comment and vote services,
// and only a request to the running router sees whether it did.
func TestCommunityBanIngestion(t *testing.T) {
	p := newPipeline(t)

	other := p.IndexedAccount(t, "bnb")
	banned := p.IndexedAccount(t, "bna")
	community := indexedCommunity(t, p, "bn", other.DID)
	bannedView := p.AppView.As(p.AppView.SignIn(t, banned))

	post := indexedPost(t, p, community, other, "a post the banned account will try to join in on")

	viewerOf := func() (map[string]any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		var response map[string]any
		if err := bannedView.Query(ctx, "social.coves.community.get",
			url.Values{"community": {community.DID}}, &response); err != nil {
			return nil, err
		}
		viewer, ok := response["viewer"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("community.get as a signed-in account served no viewer object: %v", response["viewer"])
		}
		return viewer, nil
	}

	rkey := banRecordKey(banned.DID)
	community.PutRecord(t, banCollection, rkey, map[string]any{
		"$type":     banCollection,
		"subject":   banned.DID,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
		"reason":    "spam",
	})

	var viewer map[string]any
	p.Await(t, "the community's ban record to reach the banned account's viewer state", func() (bool, error) {
		observed, err := viewerOf()
		if err != nil {
			return false, err
		}
		viewer = observed
		return observed["banned"] == true, nil
	})
	require.Equal(t, "at://"+community.DID+"/"+banCollection+"/"+rkey, viewer["banUri"])
	require.Equal(t, "spam", viewer["banReason"])
	require.NotContains(t, viewer, "banExpiresAt", "a ban written without expiresAt is permanent")

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	err := submitPost(bannedView, community.DID, "a post from a banned account "+testkit.UniqueID(t))
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "a banned account's post.create")

	err = bannedView.Procedure(ctx, "social.coves.community.comment.create", map[string]any{
		"reply": map[string]any{
			"root":   map[string]any{"uri": post.URI, "cid": post.CID},
			"parent": map[string]any{"uri": post.URI, "cid": post.CID},
		},
		"content": "a reply from a banned account",
	}, nil)
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "a banned account's comment.create on a post in the community")

	err = bannedView.Procedure(ctx, "social.coves.feed.vote.create", map[string]any{
		"subject":   map[string]any{"uri": post.URI, "cid": post.CID},
		"direction": "up",
	}, nil)
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "a banned account's vote.create on a post in the community")

	community.DeleteRecord(t, banCollection, rkey)
	p.Await(t, "the deleted ban record to leave the banned account's viewer state", func() (bool, error) {
		observed, err := viewerOf()
		if err != nil {
			return false, err
		}
		_, stillBanned := observed["banned"]
		return !stillBanned, nil
	})

	require.NoError(t, submitPost(bannedView, community.DID, "a post after the ban was lifted "+testkit.UniqueID(t)),
		"post.create must be accepted once the community deletes the ban record")
}

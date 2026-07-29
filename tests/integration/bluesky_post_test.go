//go:build integration

package integration

import (
	"Coves/tests/testkit"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBlueskyPostCrossPosting_E2E_LivePDS tests writing posts with Bluesky URLs to a real PDS
// This catches lexicon validation errors like invalid strongRef CIDs
func TestBlueskyPostCrossPosting_E2E_LivePDS(t *testing.T) {
	t.Parallel()
	// Check if PDS is running
	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	db := testkit.DB(t)

	ctx := context.Background()

	// Create test user on PDS. Use uniqueTestID() (Unix seconds + atomic counter)
	// rather than UnixNano()%1000000: PDS accounts persist across runs, and the
	// modulo collapses the timestamp to 6 digits, so it eventually collides with a
	// handle registered by an earlier run ("Handle already taken").
	testID := uniqueTestID()
	testUserHandle := fmt.Sprintf("bsky%s.local.coves.dev", testID)
	testUserEmail := fmt.Sprintf("bskytest-%s@test.local", testID)
	testUserPassword := "test-password-123"

	t.Logf("Creating test user on PDS: %s", testUserHandle)
	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}
	t.Logf("Test user created: DID=%s", userDID)

	// Index user in AppView
	_ = createTestUser(t, db, testUserHandle, userDID)

	// Create test community (needed for post creation)
	testCommunityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("bskytest%s", testID), "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	t.Run("Write post with Bluesky URL to PDS succeeds", func(t *testing.T) {
		// This test validates that Phase 1 (text-only) works correctly
		// The Bluesky URL should NOT be converted to an embed (which would require CID)
		// Instead, it should be stored as plain text content

		rkey := fmt.Sprintf("bskytest-%d", time.Now().UnixNano())

		// Post record with Bluesky URL in content (no embed conversion in Phase 1)
		postRecord := map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": testCommunityDID,
			"author":    userDID,
			"title":     "Post with Bluesky Link",
			"content":   "Check out this Bluesky post: https://bsky.app/profile/jay.bsky.team/post/3l7bsovn5rz2n",
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}

		// Write directly to PDS - this will catch lexicon validation errors
		uri, cid, writeErr := writePDSRecord(pdsURL, pdsAccessToken, userDID, "social.coves.community.post", rkey, postRecord)

		// The key assertion: this should succeed because we're NOT creating an embed
		// If embed conversion was enabled with empty CID, this would fail
		require.NoError(t, writeErr, "Writing post with Bluesky URL should succeed (Phase 1: no embed conversion)")
		require.NotEmpty(t, uri, "Should receive record URI")
		require.NotEmpty(t, cid, "Should receive record CID")

		t.Logf("✅ Post with Bluesky URL written successfully:")
		t.Logf("   URI: %s", uri)
		t.Logf("   CID: %s", cid)

		// Verify the record exists on PDS
		verifyResp, verifyErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=social.coves.community.post&rkey=%s",
			pdsURL, userDID, rkey))
		require.NoError(t, verifyErr, "Should be able to fetch record from PDS")
		defer func() { _ = verifyResp.Body.Close() }()

		require.Equal(t, http.StatusOK, verifyResp.StatusCode, "Record should exist on PDS")
		t.Logf("✅ Record verified on PDS")
	})

	t.Run("Write post with external embed (no Bluesky URL) succeeds", func(t *testing.T) {
		// Regular external embed (not a Bluesky URL) should still work
		rkey := fmt.Sprintf("exttest-%d", time.Now().UnixNano())

		postRecord := map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": testCommunityDID,
			"author":    userDID,
			"title":     "Post with External Link",
			"content":   "Check out this article",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":         "https://example.com/article",
					"title":       "Example Article",
					"description": "An interesting article about testing",
				},
			},
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}

		uri, cid, writeErr := writePDSRecord(pdsURL, pdsAccessToken, userDID, "social.coves.community.post", rkey, postRecord)
		require.NoError(t, writeErr, "Writing post with external embed should succeed")
		require.NotEmpty(t, uri)
		require.NotEmpty(t, cid)

		t.Logf("✅ Post with external embed written: %s", uri)
	})
}

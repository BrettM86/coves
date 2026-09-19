//go:build integration

package discover_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	discoverhandler "Coves/internal/api/handlers/discover"
	"Coves/internal/core/discover"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDiscover_HotSort_HostedCommunityPostOutranksTiedNonHostedPost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	// The two community names differ only at the character after "hosted", and
	// fixtures.Post still writes the LEGACY at://<community did>/social.coves.community.post/<rkey>
	// shape — a live postv2 record is at://<author did>/social.coves.community.postv2/<rkey>
	// — so here the community name, not the generated rkey, decides the URI
	// ordering. "hostedb" > "hosteda" makes the NON-hosted post the winner of
	// today's tie-breakers, which is the precondition this test asserts below.
	hostedDID, err := fixtures.Community(ctx, db, "hosteda"+testID, "hosteda"+testID+".test")
	require.NoError(t, err)
	nonHostedDID, err := fixtures.Community(ctx, db, "hostedb"+testID, "hostedb"+testID+".test")
	require.NoError(t, err)

	// Presence of a stored credential is the hosted fact; nothing decrypts it,
	// and no fixture helper writes it. hosted_by_did is not the fact — every
	// firehose-indexed community carries that field, including this test's
	// non-hosted one.
	_, err = db.ExecContext(ctx,
		`UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`,
		hostedDID, []byte("a stored refresh token"))
	require.NoError(t, err, "granting %s credentials", hostedDID)

	createdAt := time.Now().Add(-4 * time.Hour)
	hostedURI := fixtures.Post(t, db, hostedDID, "did:plc:hosted-bonus-author",
		"Hosted community post", 0, createdAt)
	nonHostedURI := fixtures.Post(t, db, nonHostedDID, "did:plc:hosted-bonus-author",
		"Non-hosted community post", 0, createdAt)

	require.Greater(t, nonHostedURI, hostedURI,
		"precondition: the non-hosted post must win the uri DESC tie-breaker, or this test could pass for the wrong reason")

	repository := postgres.NewDiscoverRepository(db, "hosted-bonus-cursor-secret")
	service := discover.NewDiscoverService(repository)
	handler := discoverhandler.NewGetDiscoverHandler(service, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=10", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "Discover Hot response: %s", rec.Body.String())

	var response discover.DiscoverResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))

	uris := make([]string, len(response.Feed))
	for i, item := range response.Feed {
		uris[i] = item.Post.URI
	}
	require.ElementsMatch(t, []string{hostedURI, nonHostedURI}, uris,
		"both posts must be candidates: the bonus reorders the feed, it does not filter it")

	assert.Equal(t, []string{hostedURI, nonHostedURI}, uris,
		"a Coves-hosted community's post must outrank an otherwise identically ranked post that wins the created_at/uri tie-breakers")
}

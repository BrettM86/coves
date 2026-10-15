//go:build integration

package routes_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noNetworkResolver fails the test on any call: a name@host actor is answered
// from the users table alone, never by a handle or DID lookup.
type noNetworkResolver struct{ t *testing.T }

var errNoNetworkResolver = errors.New("identity resolver must not be called")

func (r noNetworkResolver) Resolve(_ context.Context, identifier string) (*identity.Identity, error) {
	r.t.Errorf("identity resolver Resolve(%q) called", identifier)
	return nil, errNoNetworkResolver
}

func (r noNetworkResolver) ResolveHandle(_ context.Context, handle string) (string, string, error) {
	r.t.Errorf("identity resolver ResolveHandle(%q) called", handle)
	return "", "", errNoNetworkResolver
}

func (r noNetworkResolver) ResolveDID(_ context.Context, did string) (*identity.DIDDocument, error) {
	r.t.Errorf("identity resolver ResolveDID(%q) called", did)
	return nil, errNoNetworkResolver
}

func (r noNetworkResolver) Purge(_ context.Context, identifier string) error {
	r.t.Errorf("identity resolver Purge(%q) called", identifier)
	return errNoNetworkResolver
}

// TestGetProfile_ScopedActorIdentifierResolvesFromStoredOriginPair composes the
// real users repository and service behind getProfile: an @name@host actor
// with a mixed-case host finds the bridged user by its stored
// (origin_username, origin) pair.
func TestGetProfile_ScopedActorIdentifierResolvesFromStoredOriginPair(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(db)

	_, err := repo.Create(ctx, &users.User{
		DID:    "did:plc:alicebridged000000000000",
		Handle: "alice-b.lemmy-world.ap.example.test",
		PDSURL: testkit.Endpoints().PDS.BaseURL,
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`UPDATE users SET origin = $2, origin_username = $3 WHERE did = $1`,
		"did:plc:alicebridged000000000000", "lemmy.world", "Alice_B")
	require.NoError(t, err)

	svc := users.NewUserService(repo, noNetworkResolver{t: t}, testkit.Endpoints().PDS.BaseURL, nil, "")

	w := httptest.NewRecorder()
	routes.NewUserHandler(svc).GetProfile(w, httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.actor.getProfile?actor="+url.QueryEscape("@Alice_B@Lemmy.World"), nil))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var profile struct {
		DID string `json:"did"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &profile))
	assert.Equal(t, "did:plc:alicebridged000000000000", profile.DID)
}

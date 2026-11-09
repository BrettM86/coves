//go:build integration

package main

import (
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

// A session saved through the store the production wiring builds must live as
// long as the configured OAuth session lifetime (365 days), not the store's
// 7-day fallback. With the 7-day row, GetSession stops finding the session on
// day 7 and a read-only user is logged out even though their sealed token and
// the OAuth client's session TTL are both still valid.
func TestBuildAuth_SessionRowExpiresAtConfiguredSessionTTL(t *testing.T) {
	db := testkit.DB(t)

	a := &application{
		db:                    db,
		sessionCoordinationDB: db,
		cfg: &config.Config{
			IsDevEnv: true,
			OAuth: config.OAuthConfig{
				PublicURL: "http://127.0.0.1",
			},
		},
	}
	a.cfg.Identity.PLCURL = testkit.Endpoints().PLC.BaseURL
	a.cfg.PDS.URL = testkit.Endpoints().PDS.BaseURL

	require.NoError(t, a.buildAuth())

	did := syntax.DID("did:plc:" + testkit.UniqueID(t))
	sessionID := testkit.UniqueIDWithPrefix(t, "sessttl")
	session := oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               sessionID,
		HostURL:                 "https://pds.example.com",
		AuthServerURL:           "https://auth.example.com",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "at_test_token_sessttl",
		RefreshToken:            "rt_test_token_sessttl",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	before := time.Now()
	require.NoError(t, a.oauthStore.SaveSession(t.Context(), session))
	after := time.Now()

	var expiresAt time.Time
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT expires_at FROM oauth_sessions WHERE did = $1 AND session_id = $2`,
		did.String(), sessionID,
	).Scan(&expiresAt))

	const configuredSessionTTL = 365 * 24 * time.Hour
	const tolerance = time.Hour
	earliest := before.Add(configuredSessionTTL - tolerance)
	latest := after.Add(configuredSessionTTL + tolerance)
	require.Truef(t, !expiresAt.Before(earliest) && !expiresAt.After(latest),
		"oauth_sessions.expires_at = %s, %s after save; want within %s of 365 days after save (%s to %s)",
		expiresAt.UTC().Format(time.RFC3339), expiresAt.Sub(before).Round(time.Minute), tolerance,
		earliest.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
}

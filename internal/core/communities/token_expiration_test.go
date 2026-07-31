//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When a community's access token is considered stale.
//
// NeedsRefresh is the trigger for the whole credential-renewal path: every
// write on a community's behalf calls EnsureFreshToken, which asks this
// function whether to spend the single-use refresh token. Both directions of
// the answer cost something. Too eager and every write burns a refresh; too
// late and the write goes out on a token the PDS has already stopped accepting,
// which surfaces as a 401 from a repo the AppView is supposed to own.
//
// The five-minute buffer is the compromise, and the cases below are placed on
// either side of it deliberately — four minutes refreshes, six does not — so
// that moving the constant fails a test instead of quietly changing how often
// the instance re-authenticates.
//
// The function parses an UNVERIFIED JWT: it reads the exp claim without
// checking the signature, because the token came from the PDS over TLS and is
// about to be sent back to that same PDS, which will verify it properly. That
// makes the malformed-input cases load-bearing rather than pedantic — this code
// is a parser pointed at a string, and it must refuse rather than guess.

// jwtExpiringAt builds a JWT with the claims this code reads and a signature it
// does not.
//
// It is deliberately unsigned rubbish in the third segment: a test that needed
// a real signature would need a key, and a key would suggest the signature
// matters here. It does not — parseJWTExpiration never looks at it — and the
// day that changes, these tests should fail loudly rather than keep passing
// against a token nothing would accept.
func jwtExpiringAt(expiry time.Time) string {
	return jwtWithClaims(map[string]any{
		"sub": "did:plc:tokenexpiration",
		"iss": "https://pds.invalid",
		"exp": expiry.Unix(),
		"iat": time.Now().Unix(),
	})
}

// jwtWithClaims encodes an arbitrary claim set, so a test can build a token
// that is well-formed everywhere except the part it is about.
func jwtWithClaims(claims map[string]any) string {
	encode := func(v any) string {
		encoded, err := json.Marshal(v)
		if err != nil {
			// Only reachable by handing this function something unmarshalable,
			// which no caller here does.
			panic("token_expiration_test: encoding JWT segment: " + err.Error())
		}
		return base64.RawURLEncoding.EncodeToString(encoded)
	}
	header := encode(map[string]any{"alg": "ES256", "typ": "JWT"})
	payload := encode(claims)
	signature := base64.RawURLEncoding.EncodeToString([]byte("not-a-real-signature"))
	return header + "." + payload + "." + signature
}

func TestNeedsRefreshUsesAFiveMinuteBuffer(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		expiry  time.Duration
		refresh bool
	}{
		{"already expired", -time.Minute, true},
		{"expires in two minutes", 2 * time.Minute, true},
		{"expires in four minutes, inside the buffer", 4 * time.Minute, true},
		{"expires in six minutes, outside the buffer", 6 * time.Minute, false},
		{"expires in ten minutes", 10 * time.Minute, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			needsRefresh, err := communities.NeedsRefresh(jwtExpiringAt(time.Now().Add(testCase.expiry)))
			require.NoError(t, err)
			assert.Equal(t, testCase.refresh, needsRefresh)
		})
	}
}

func TestNeedsRefreshRefusesTokensItCannotRead(t *testing.T) {
	t.Parallel()

	// Every one of these must be an ERROR rather than a false. Defaulting an
	// unreadable token to "no refresh needed" would send it to the PDS as-is and
	// turn a parsing bug into an authentication failure three layers away; the
	// caller treats an error here as a hard stop on the write.
	for _, testCase := range []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"too few segments", "invalid.token"},
		{"too many segments", "not.a.valid.jwt.format.extra"},
		{"payload is not base64", "aGVhZGVy.!!!not-base64!!!.c2ln"},
		{"payload is not JSON", "aGVhZGVy." + base64.RawURLEncoding.EncodeToString([]byte("plain text")) + ".c2ln"},
		{"no exp claim", jwtWithClaims(map[string]any{"sub": "did:plc:noexpiry"})},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			needsRefresh, err := communities.NeedsRefresh(testCase.token)
			require.Error(t, err)
			assert.False(t, needsRefresh,
				"an unreadable token must not also come back as a positive refresh decision")
		})
	}
}

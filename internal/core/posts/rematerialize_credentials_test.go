package posts

import (
	"context"
	"errors"
	"fmt"
	"testing"

	covesoauth "Coves/internal/atproto/oauth"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A TRANSIENT CREDENTIAL FAILURE MUST NOT BE WRITTEN AS A TERMINAL VERDICT.
//
// ErrNoAuthorCredentials is not a diagnosis, it is a SENTENCE: the
// re-materialization census answers it by writing fallback_left_legacy, which is
// terminal three ways over — IsFallback short-circuits RematerializeOne,
// ListResumable excludes the row, and MarkFallback only accepts a row still at
// discovered, so nothing in the tool can ever move it back.
//
// The Kagi aggregator authors the overwhelming majority of production posts. If
// one network blip, one PDS 5xx, or one DPoP nonce failure while resuming ITS
// session is reported as "this author has no credentials", the census marks the
// ENTIRE CORPUS terminal in seconds and every subsequent run is a permanent
// no-op — with nothing in the tool to undo it.
//
// So the sentinel is split. "There is no grant to resume" is a genuine, terminal
// absence. Everything else is ErrAuthorCredentialsUnavailable: RETRYABLE, and
// the run fails loudly on it rather than sentencing a row.

// resumeFailureClass is the classifier under test: it is what decides whether a
// failure to open an author's repo is a verdict or a retry.
func TestClassifyResumeFailure_MissingGrantIsTerminal(t *testing.T) {
	err := classifyResumeFailure("did:plc:aggregator", covesoauth.ErrSessionNotFound)

	require.Truef(t, errors.Is(err, ErrNoAuthorCredentials),
		"a session the store does not hold is the one genuinely terminal case: nobody can re-authorize an aggregator from inside a batch tool, "+
			"so the post is left as legacy rather than forged. got: %v", err)
	assert.Falsef(t, errors.Is(err, ErrAuthorCredentialsUnavailable),
		"an absent grant must NOT also be retryable, or the run would fail instead of recording the fallback the census exists to produce")
}

func TestClassifyResumeFailure_TransientFailuresAreRetryableNotTerminal(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"network blip", errors.New("dial tcp 10.0.0.5:443: connect: connection refused")},
		{"PDS 5xx", fmt.Errorf("token refresh: %w", errors.New("unexpected status 502"))},
		{"DPoP nonce failure", errors.New("use_dpop_nonce")},
		{"database error reading the session store", fmt.Errorf("failed to get session: %w", errors.New("driver: bad connection"))},
	}

	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyResumeFailure("did:plc:aggregator", tc.err)

			assert.Falsef(t, errors.Is(err, ErrNoAuthorCredentials),
				"%s was classified as ErrNoAuthorCredentials. The re-materialization census writes that as fallback_left_legacy, which is TERMINAL and "+
					"has no in-tool path back — one blip while resuming the aggregator's session would sentence every post it ever wrote. got: %v", tc.name, err)
			assert.Truef(t, errors.Is(err, ErrAuthorCredentialsUnavailable),
				"%s must be RETRYABLE so the run fails loudly and the operator can re-run after the cause clears. got: %v", tc.name, err)
		})
	}
}

// The two sentinels must be genuinely distinct values, not one aliased to the
// other: every caller decides "sentence or retry" by telling them apart.
func TestAuthorCredentialSentinels_AreDistinct(t *testing.T) {
	assert.Falsef(t, errors.Is(ErrAuthorCredentialsUnavailable, ErrNoAuthorCredentials),
		"the retryable sentinel must not satisfy errors.Is against the terminal one, or a transient failure is still written as a terminal fallback")
	assert.Falsef(t, errors.Is(ErrNoAuthorCredentials, ErrAuthorCredentialsUnavailable),
		"the terminal sentinel must not satisfy errors.Is against the retryable one, or an author who genuinely cannot be restored fails the whole run forever")
}

// A nil-but-successful resume — the store answered without an error and handed
// back nothing — is an absent grant, not a transport fault.
func TestClassifyResumeFailure_NilErrorIsNotAFailure(t *testing.T) {
	assert.NoErrorf(t, classifyResumeFailure("did:plc:aggregator", nil),
		"classifying a nil resume error must produce no error at all")
}

// The tool resolves credentials ONCE PER DISTINCT AUTHOR, not once per post.
//
// Each resume is a refresh-token rotation against the PDS. An aggregator with
// 5,000 posts would otherwise trigger 5,000 rotations in a single run — minutes
// of avoidable load, thousands of chances for the transient failure above, and a
// token-rotation chain any one of whose links can break the session for the
// AppView itself.
func TestRematerializer_ResolvesCredentialsOncePerAuthor(t *testing.T) {
	resolutions := map[string]int{}
	tool := &Rematerializer{
		AuthorRepos: func(_ context.Context, did string, _ *oauth.ClientSessionData) (AuthorRepo, error) {
			resolutions[did]++
			return nil, fmt.Errorf("no repo in this unit test: %w", ErrNoAuthorCredentials)
		},
	}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = tool.authorRepo(ctx, "did:plc:aggregator")
		_, _ = tool.authorRepo(ctx, "did:plc:human")
	}

	assert.Equalf(t, 1, resolutions["did:plc:aggregator"],
		"the aggregator's session was resumed %d times. Each resume rotates the refresh token; an aggregator with 5,000 posts would rotate 5,000 times "+
			"in one run", resolutions["did:plc:aggregator"])
	assert.Equalf(t, 1, resolutions["did:plc:human"], "credentials must be resolved once per distinct author DID")
}

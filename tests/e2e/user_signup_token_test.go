//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_UserSignupToken is the API contract (§3.4b) for the Turnstile-gated
// half of signup: POST /xrpc/social.coves.actor.requestSignupToken, which mints
// the single-use PDS invite that social.coves.actor.signup then spends.
//
// Like its sibling in user_signup_test.go this is a CLIENT-SURFACE contract and
// proves nothing about ingestion — read that file's doc comment for why the
// distinction matters here in particular.
//
// The stack answers the captcha with a stub bound to 127.0.0.1 rather than
// calling Cloudflare (there is no egress from the CI network at all, §3.7), so
// any token string passes verification. What is under test is the handshake and
// its gates, not the captcha vendor.
//
// The bot gate is the reason this file exists and the reason it is at T2: the
// per-route rate limit lives in the running AppView's middleware chain, so only
// a request to the real router can show that the route still has it.
func TestE2E_UserSignupToken(t *testing.T) {
	// No availability probes and no skips: TestMain's Require floor proved the
	// stack before this package ran a single test (§3.1). The two skip calls
	// that stood here could only convert a broken stack into a green run.

	t.Run("Happy path: mint invite and sign up end-to-end", func(t *testing.T) {
		handle, email := signupAccount(t, "a")

		code, err := requestSignupToken("any-turnstile-value")
		if err != nil {
			t.Fatalf("requestSignupToken failed: %v", err)
		}
		if code == "" {
			t.Fatalf("expected non-empty inviteCode")
		}
		t.Logf("minted invite code: %s", code)

		// End-to-end: use the minted code to actually sign up.
		did, err := signupWithInvite(handle, email, "test1234", code)
		if err != nil {
			t.Fatalf("signup with minted invite failed: %v", err)
		}
		if !strings.HasPrefix(did, "did:") {
			t.Fatalf("expected DID, got: %q", did)
		}
		t.Logf("✅ signup completed: %s → %s", handle, did)
	})

	t.Run("Invite is single-use: replay returns 400", func(t *testing.T) {
		handle, email := signupAccount(t, "b")

		code, err := requestSignupToken("any")
		if err != nil {
			t.Fatalf("requestSignupToken failed: %v", err)
		}

		if _, err := signupWithInvite(handle, email, "test1234", code); err != nil {
			t.Fatalf("first signup must succeed: %v", err)
		}

		// Second attempt with the same code must fail.
		handle2, email2 := signupAccount(t, "br")
		if _, err := signupWithInvite(handle2, email2, "test1234", code); err == nil {
			t.Fatalf("expected second signup with same invite to fail, but it succeeded")
		}
	})

	t.Run("Two successive token requests issue distinct invite codes", func(t *testing.T) {
		// We test PDS single-use elsewhere; this asserts Coves itself isn't
		// caching/reusing an invite code across distinct calls.
		c1, err := requestSignupToken("any")
		require.NoError(t, err)
		c2, err := requestSignupToken("any")
		require.NoError(t, err)

		require.NotEmpty(t, c1)
		require.NotEmpty(t, c2)
		assert.NotEqual(t, c1, c2, "Coves must mint a fresh code per request, not reuse")
	})

	t.Run("Per-route rate limit: 6 hits in <60s → 6th is 429", func(t *testing.T) {
		// Per-route limit is the bot gate. If this test stops asserting,
		// regressions to that gate go undetected.
		//
		// Its own bucket, distinct from the minting calls above and fresh for
		// this run: exhausting a bucket is what this subtest is FOR, so it must
		// not be one anything else depends on.
		spoofedIP := spoofedClientIP("rate-limit-probe")

		payload, _ := json.Marshal(map[string]string{
			"turnstileToken": "any",
		})

		var statuses []int
		for i := 0; i < 7; i++ {
			req, err := http.NewRequest(http.MethodPost,
				testkit.Endpoints().AppView.BaseURL+"/xrpc/social.coves.actor.requestSignupToken",
				bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Real-IP", spoofedIP)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			statuses = append(statuses, resp.StatusCode)
			t.Logf("request %d (ip=%s) → %d", i+1, spoofedIP, resp.StatusCode)
		}

		// EXACTLY where the limit falls, not merely that it falls somewhere.
		//
		// This assertion used to be require.Contains(statuses, 429) — "at least
		// one of the seven was refused" — under a name promising the sixth. That
		// passes with a limiter set to 1/min, which would lock every real user
		// out on their second attempt, and it passes with one set to 6/min, which
		// is a weaker gate than intended. The bucket is this run's own
		// (spoofedClientIP above), so the boundary is deterministic and there is
		// no reason to assert anything vaguer.
		require.Len(t, statuses, 7, "the loop must have made seven attempts")
		for i, status := range statuses[:5] {
			require.NotEqualf(t, http.StatusTooManyRequests, status,
				"request %d of the first five was refused: the per-route limit is configured at 5/min "+
					"and this bucket is unique to this run, so nothing should have been spent before it. "+
					"A limiter set below 5 locks real users out early", i+1)
		}
		require.Equalf(t, http.StatusTooManyRequests, statuses[5],
			"the SIXTH request must be the first refusal (per-route limit 5/min). Got %d. Above 5/min the "+
				"bot gate is weaker than intended; a 429 earlier than this would have failed the loop above",
			statuses[5])
		require.Equalf(t, http.StatusTooManyRequests, statuses[6],
			"the seventh request must still be refused inside the same window — a limiter that lets the "+
				"next request through is not holding the window open. Got %d", statuses[6])
	})
}

// spoofedClientIP returns the rate-limit bucket for one of this file's two
// distinct purposes.
//
// The signup-token route is rate limited per client IP at 5/min, and the
// limiter's buckets live in the AppView process — so they OUTLIVE a test run.
// Against a stack that persists (COVES_CI_KEEP_STACK, and therefore every
// re-run of `make test-e2e`) the subtests below would inherit the previous
// run's quota and fail with 429s that say nothing about the code. Giving each
// run its own source address makes the tier re-runnable, which is the whole
// point of keeping a stack up.
//
// The purpose label separates the minting calls from the rate-limit probe,
// which deliberately exhausts its bucket: sharing one would make the subtests
// order-dependent, passing only because the probe happens to run last.
//
// Both come from testkit.SyntheticClientIP, the one primitive the tier uses for
// this — 64 hash bits over an IPv6 documentation range. Two earlier versions of
// this helper were wrong in the same direction and are worth not repeating: one
// keyed on os.Getpid(), which is not per-run at all inside a container that
// starts the same process tree every time, and one folded the hash into a
// single IPv4 octet, which starts colliding after a couple of hundred runs
// against a kept stack.
func spoofedClientIP(purpose string) string {
	return testkit.SyntheticClientIP("signup-token/" + purpose)
}

// requestSignupToken calls /xrpc/social.coves.actor.requestSignupToken and returns
// the minted invite code.
func requestSignupToken(turnstileToken string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"turnstileToken": turnstileToken,
	})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		testkit.Endpoints().AppView.BaseURL+"/xrpc/social.coves.actor.requestSignupToken",
		bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Real-IP", spoofedClientIP("minting"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("status %d: read body: %w", resp.StatusCode, readErr)
		}
		excerpt := string(raw)
		if len(excerpt) > 512 {
			excerpt = excerpt[:512] + "...[truncated]"
		}
		var errResp map[string]interface{}
		if jsonErr := json.Unmarshal(raw, &errResp); jsonErr != nil {
			return "", fmt.Errorf("status %d: parse body: %w: rawBody=%q", resp.StatusCode, jsonErr, excerpt)
		}
		return "", fmt.Errorf("status %d: parsed=%v rawBody=%q", resp.StatusCode, errResp, excerpt)
	}

	var result struct {
		InviteCode string `json:"inviteCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	return result.InviteCode, nil
}

// signupWithInvite calls /xrpc/social.coves.actor.signup with the given invite.
// Returns the created DID.
func signupWithInvite(handle, email, password, inviteCode string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"handle":     handle,
		"email":      email,
		"password":   password,
		"inviteCode": inviteCode,
	})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	resp, err := http.Post(
		testkit.Endpoints().AppView.BaseURL+"/xrpc/social.coves.actor.signup",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("POST signup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("signup status %d: read body: %w", resp.StatusCode, readErr)
		}
		excerpt := string(raw)
		if len(excerpt) > 512 {
			excerpt = excerpt[:512] + "...[truncated]"
		}
		var errResp map[string]interface{}
		if jsonErr := json.Unmarshal(raw, &errResp); jsonErr != nil {
			return "", fmt.Errorf("signup status %d: parse body: %w: rawBody=%q", resp.StatusCode, jsonErr, excerpt)
		}
		return "", fmt.Errorf("signup status %d: parsed=%v rawBody=%q", resp.StatusCode, errResp, excerpt)
	}

	var result struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	return result.DID, nil
}

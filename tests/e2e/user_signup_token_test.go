//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_UserSignupToken exercises the Turnstile-gated signup handshake
// (POST /xrpc/social.coves.actor.requestSignupToken) against a running dev stack.
// It uses Cloudflare's published "always-pass" site+secret keys, so no real
// captcha challenge is required.
//
// Prerequisites (same as TestE2E_UserSignup):
//   - AppView running on localhost:8081 (with TURNSTILE_SECRET_KEY set to
//     Cloudflare's always-pass secret 1x000…AA — this is the default in .env.dev)
//   - PDS running on localhost:3001 with PDS_INVITE_REQUIRED=true and
//     PDS_ADMIN_PASSWORD=admin
//
// Run with:
//
//	make e2e-up
//	go run ./cmd/server &
//	go test ./tests/e2e -run TestE2E_UserSignupToken -v
func TestE2E_UserSignupToken(t *testing.T) {
	if !isAppViewAvailable(t) {
		t.Skip("AppView not available at localhost:8081 - run 'go run ./cmd/server' first")
	}
	if !isPDSAvailable(t) {
		t.Skip("PDS not available at localhost:3001 - run 'make dev-up' first")
	}

	t.Run("Happy path: mint invite and sign up end-to-end", func(t *testing.T) {
		handle, email := uniqueAccount("a")

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
		handle, email := uniqueAccount("b")

		code, err := requestSignupToken("any")
		if err != nil {
			t.Fatalf("requestSignupToken failed: %v", err)
		}

		if _, err := signupWithInvite(handle, email, "test1234", code); err != nil {
			t.Fatalf("first signup must succeed: %v", err)
		}

		// Second attempt with the same code must fail.
		handle2, email2 := uniqueAccount("br")
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
				"http://localhost:8081/xrpc/social.coves.actor.requestSignupToken",
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

		// The per-route limiter is configured at 5/min. The 6th hit should be
		// the first 429. Hard assertion; no skip.
		require.Contains(t, statuses, http.StatusTooManyRequests,
			"expected at least one 429 in first 7 requests; per-route bot gate may be missing")
	})
}

// uniqueAccount builds a handle/email pair unique across runs while staying
// under the PDS handle-length limit (the chosen name / local label must be
// <= 18 chars). base36(UnixNano) is ~12 chars and monotonic, so we keep the
// prefix short (<= 2 chars): "<prefix>-<suffix>.local.coves.dev". Real users
// pick their own short handles; this constraint only bites synthetic tests.
func uniqueAccount(prefix string) (handle, email string) {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	return fmt.Sprintf("%s-%s.local.coves.dev", prefix, suffix),
		fmt.Sprintf("%s-%s@test.com", prefix, suffix)
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
		"http://localhost:8081/xrpc/social.coves.actor.requestSignupToken",
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
		"http://localhost:8081/xrpc/social.coves.actor.signup",
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

//go:build integration

// The create endpoint is the AppView's only authenticated write path for posts,
// so its rejection behaviour is a security boundary rather than an ergonomic
// detail: the author of a post is derived from the session and never from the
// request body, the body is bounded before it is parsed, and the community
// identifier is validated before anything is written.
//
// These tests drive the real handler over the real post and community services,
// which is the difference between them and the in-package unit tests in this
// directory. A unit test with a fake service proves the handler forwards what
// it was given; only the real stack proves the check is still ON the path a
// request takes — the failure mode that matters is a validation call that gets
// moved behind an early return and stops running while its own unit test keeps
// passing.
//
// The file is in the external test package because it imports
// internal/db/postgres, which pulls in the domain; the established form for
// every relocated integration test in this tree is package foo_test.
package post_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const createPostPath = "/xrpc/social.coves.community.post.create"

// createPost posts payload as the given DID and returns the recorder. An empty
// authorDID sends the request unauthenticated.
func createPost(t *testing.T, stack createStack, authorDID string, payload any) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err, "marshalling the request payload")
	return createPostRaw(t, stack, authorDID, body)
}

// createPostRaw is createPost for the cases that need to send bytes the JSON
// encoder would never produce.
func createPostRaw(t *testing.T, stack createStack, authorDID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, createPostPath, bytes.NewReader(body))
	if authorDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), authorDID))
	}

	rec := httptest.NewRecorder()
	stack.handler.HandleCreate(rec, req)
	return rec
}

// decodeXRPCError reads the {error, message} envelope every failure carries.
func decodeXRPCError(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope),
		"failure responses must be a JSON XRPC error envelope, got: %s", rec.Body.String())
	return envelope
}

// TestPostCreate_HandlerRejections covers everything the create handler must
// refuse before the request reaches the service.
func TestPostCreate_HandlerRejections(t *testing.T) {
	t.Parallel()
	stack := newCreateStack(t, testkit.DB(t))

	authorDID := fixtures.DID("author" + testkit.UniqueID(t))
	communityDID := fixtures.DID("community" + testkit.UniqueID(t))

	t.Run("client-supplied authorDid is refused", func(t *testing.T) {
		// Authorship comes from the session. Accepting it from the body would
		// let any authenticated caller post as anybody, so the handler rejects
		// the field outright rather than silently overwriting it — a silent
		// overwrite would leave a client believing it had posted as someone
		// else and succeeded.
		rec := createPost(t, stack, authorDID, map[string]any{
			"community": communityDID,
			"authorDid": fixtures.DID("attacker"),
			"content":   "Malicious post",
		})

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		envelope := decodeXRPCError(t, rec)
		assert.Equal(t, "InvalidRequest", envelope["error"])
		assert.Contains(t, envelope["message"], "authorDid must not be provided")
	})

	t.Run("unauthenticated request is refused", func(t *testing.T) {
		rec := createPost(t, stack, "", map[string]any{
			"community": communityDID,
			"content":   "Test post",
		})

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "AuthRequired", decodeXRPCError(t, rec)["error"])
	})

	t.Run("body over 1MB is refused before it is parsed", func(t *testing.T) {
		rec := createPost(t, stack, authorDID, map[string]any{
			"community": communityDID,
			"content":   strings.Repeat("A", 1*1024*1024+1000),
		})

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		assert.Equal(t, "RequestTooLarge", decodeXRPCError(t, rec)["error"])
	})

	t.Run("malformed JSON is refused", func(t *testing.T) {
		rec := createPostRaw(t, stack, authorDID, []byte(`{"community": "did:plc:test123", "content": `))

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "InvalidRequest", decodeXRPCError(t, rec)["error"])
	})

	t.Run("empty community is refused", func(t *testing.T) {
		rec := createPost(t, stack, authorDID, map[string]any{
			"community": "",
			"content":   "Test post",
		})

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		envelope := decodeXRPCError(t, rec)
		assert.Equal(t, "InvalidRequest", envelope["error"])
		assert.Contains(t, envelope["message"], "community is required")
	})

	t.Run("non-POST methods are refused", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			t.Run(method, func(t *testing.T) {
				rec := httptest.NewRecorder()
				stack.handler.HandleCreate(rec, httptest.NewRequest(method, createPostPath, nil))
				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			})
		}
	})
}

// TestPostCreate_CommunityIdentifierFormats covers the at-identifier surface of
// the community field: which shapes are turned away as malformed, and which
// shapes get far enough to be looked up.
//
// The accepted shapes are asserted negatively — "did not fail FORMAT
// validation" — because none of these communities exist in the database, so the
// request is expected to end in a lookup failure. Asserting a success status
// would require provisioning a community with working PDS credentials, which is
// the pipeline tier's job; what this file can prove is that a legal identifier
// is not rejected as illegal.
func TestPostCreate_CommunityIdentifierFormats(t *testing.T) {
	t.Parallel()
	stack := newCreateStack(t, testkit.DB(t))

	authorDID := fixtures.DID("author" + testkit.UniqueID(t))

	t.Run("malformed identifiers are refused", func(t *testing.T) {
		for _, identifier := range []string{
			"not-a-did-or-handle",
			"just-plain-text",
			"http://example.com",
		} {
			t.Run(identifier, func(t *testing.T) {
				rec := createPost(t, stack, authorDID, map[string]any{
					"community": identifier,
					"content":   "Test post",
				})

				// 400 (rejected as malformed) and 404 (parsed, then not found)
				// are both correct refusals; which one a given shape produces
				// depends on how far the resolver gets before it gives up, and
				// pinning that here would make the test brittle about an
				// implementation detail rather than about the refusal.
				assert.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusNotFound,
					"expected 400 or 404 for %q, got %d: %s", identifier, rec.Code, rec.Body.String())

				envelope := decodeXRPCError(t, rec)
				assert.NotEmpty(t, envelope["error"], "refusals must name an error code")
				assert.NotEmpty(t, envelope["message"], "refusals must carry a message")
			})
		}
	})

	// The four legal spellings of a community, per the at-identifier rules:
	// a bare DID, the scoped !name@instance form, the canonical DNS handle
	// c-name.instance the scoped form expands to, and that handle with the
	// atProto @ prefix.
	wellFormed := []string{
		"did:plc:test123",
		"did:web:example.com",
		"!mycommunity@bsky.social",
		"!gaming@test.coves.social",
		"c-gaming.test.coves.social",
		"c-books.bsky.social",
		"@c-gaming.test.coves.social",
		"@c-books.bsky.social",
	}

	t.Run("well-formed identifiers pass format validation", func(t *testing.T) {
		for _, identifier := range wellFormed {
			t.Run(identifier, func(t *testing.T) {
				rec := createPost(t, stack, authorDID, map[string]any{
					"community": identifier,
					"content":   "Test post",
				})

				if rec.Code != http.StatusBadRequest {
					return // Reached the lookup, which is as far as this test goes.
				}
				message := decodeXRPCError(t, rec)["message"]
				assert.NotContains(t, message, "community must be a DID",
					"%q is a legal community identifier and must not be rejected as malformed", identifier)
				assert.NotContains(t, message, "scoped handle must include",
					"%q is a legal community identifier and must not be rejected as malformed", identifier)
			})
		}
	})
}

// TestPostCreate_HostileContent covers content the handler must carry through
// unharmed rather than refuse: text is text, and the injection-shaped strings
// below are dangerous only if something downstream interpolates them.
func TestPostCreate_HostileContent(t *testing.T) {
	t.Parallel()
	stack := newCreateStack(t, testkit.DB(t))

	authorDID := fixtures.DID("author" + testkit.UniqueID(t))
	communityDID := fixtures.DID("community" + testkit.UniqueID(t))

	t.Run("unicode and emoji are not rejected", func(t *testing.T) {
		rec := createPost(t, stack, authorDID, map[string]any{
			"community": communityDID,
			"content":   "Hello 世界! 🌍 Testing unicode: café, naïve, Ω",
		})

		// The community does not exist, so the request cannot succeed; what it
		// must not do is come back as a validation failure.
		assert.NotEqual(t, http.StatusBadRequest, rec.Code,
			"valid UTF-8 content must not be rejected as invalid: %s", rec.Body.String())
	})

	t.Run("injection-shaped content does not reach an interpolator", func(t *testing.T) {
		// Every one of these is a plain string as far as the AppView is
		// concerned. A 500 would mean something tried to interpret it — a
		// concatenated query, a template, a path join — which is precisely the
		// bug this case exists to catch.
		for _, hostile := range []string{
			"'; DROP TABLE posts; --",
			"1' OR '1'='1",
			"<script>alert('xss')</script>",
			"../../../etc/passwd",
		} {
			t.Run(hostile, func(t *testing.T) {
				rec := createPost(t, stack, authorDID, map[string]any{
					"community": communityDID,
					"content":   hostile,
				})

				assert.NotEqual(t, http.StatusInternalServerError, rec.Code,
					"content %q must be handled as data, not interpreted: %s", hostile, rec.Body.String())
			})
		}
	})
}

// TestPostCreate_ServiceEnforcesAuthorship covers the same authorship rule as
// the handler test above, one layer down.
//
// The duplication is deliberate defence in depth. The handler is not the only
// caller of posts.Service.CreatePost — a consumer, a future admin path or a
// refactor that moves the route can all reach the service directly — so the
// service re-derives the author from the context instead of trusting the
// request it was handed. These cases call the service with the handler removed
// from the picture, which is the only way to prove that second check exists.
func TestPostCreate_ServiceEnforcesAuthorship(t *testing.T) {
	t.Parallel()
	stack := newCreateStack(t, testkit.DB(t))

	communityDID := fixtures.DID("community" + testkit.UniqueID(t))
	alice := fixtures.DID("alice" + testkit.UniqueID(t))
	bob := fixtures.DID("bob" + testkit.UniqueID(t))

	createAs := func(contextDID, requestDID string) error {
		content := "Test post"
		_, err := stack.service.CreatePost(
			middleware.SetTestUserDID(t.Context(), contextDID),
			posts.CreatePostRequest{
				Community: communityDID,
				AuthorDID: requestDID,
				Content:   &content,
			},
		)
		return err
	}

	t.Run("no authenticated DID in context is refused", func(t *testing.T) {
		err := createAs("", alice)

		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "authenticated")
	})

	t.Run("request DID that differs from the session is refused", func(t *testing.T) {
		// The spoofing case: authenticated as Alice, asking to post as Bob.
		err := createAs(alice, bob)

		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "does not match")
	})

	t.Run("matching DIDs pass the authorship check", func(t *testing.T) {
		// The community does not exist, so this still fails — but it must fail
		// on the lookup, not on authorship. Without this case the two above
		// would also pass if CreatePost rejected every request it ever saw.
		err := createAs(alice, alice)

		if err != nil {
			assert.NotContains(t, strings.ToLower(err.Error()), "does not match",
				"a request whose DID matches the session must not fail the authorship check")
		}
	})
}

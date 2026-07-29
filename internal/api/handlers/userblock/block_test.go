package userblock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/userblocks"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three user-block endpoints, at the HTTP boundary.
//
// This package had no tests at all; what stood in for them was
// tests/integration/userblock_handler_test.go, which reached the same handlers
// through a chi router, a real Postgres clone and a mock PDS, and then spent
// most of its length re-asserting service behaviour that
// internal/core/userblocks/service_test.go already covers directly. What is
// actually the handler's own is small and needs none of that: the request
// shapes it rejects, the JSON envelopes it emits, the query parameters it
// parses, and the sentinel errors it maps to statuses.
//
// So the service is a stub here, deliberately. Its behaviour — the self-block
// guard, the PDS conflict resolution, the record delete — is proven against the
// real thing in service_test.go, and repeating it through HTTP would only prove
// that a stub returns what it was told to. The 401s live in
// tests/e2e's block contract, where a real OAuth session can be absent for real
// reasons rather than because a test omitted a context value.

// stubService answers the Service interface from per-test functions. A nil
// function means the call is not expected to happen in that test; leaving them
// nil rather than defaulting to a success keeps an unexpected call visible.
type stubService struct {
	blockFunc       func(ctx context.Context, session *oauth.ClientSessionData, identifier string) (*userblocks.BlockResult, error)
	unblockFunc     func(ctx context.Context, session *oauth.ClientSessionData, identifier string) error
	blockedFunc     func(ctx context.Context, blockerDID string, limit, offset int) ([]*userblocks.UserBlock, error)
	isBlockedResult bool
}

func (s *stubService) BlockUser(ctx context.Context, session *oauth.ClientSessionData, identifier string) (*userblocks.BlockResult, error) {
	if s.blockFunc == nil {
		return nil, errUnexpectedCall
	}
	return s.blockFunc(ctx, session, identifier)
}

func (s *stubService) UnblockUser(ctx context.Context, session *oauth.ClientSessionData, identifier string) error {
	if s.unblockFunc == nil {
		return errUnexpectedCall
	}
	return s.unblockFunc(ctx, session, identifier)
}

func (s *stubService) GetBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*userblocks.UserBlock, error) {
	if s.blockedFunc == nil {
		return nil, errUnexpectedCall
	}
	return s.blockedFunc(ctx, blockerDID, limit, offset)
}

func (s *stubService) IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error) {
	return s.isBlockedResult, nil
}

// errUnexpectedCall surfaces as a 500, which is how a test that reached a
// service method it did not mean to reach announces itself.
var errUnexpectedCall = errors.New("the handler called a service method the test did not stub")

const callerDID = "did:plc:blockcaller"

// authenticated returns a request carrying the OAuth session the middleware
// would have put there.
func authenticated(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")

	did, err := syntax.ParseDID(callerDID)
	require.NoError(t, err)
	return req.WithContext(context.WithValue(req.Context(), middleware.OAuthSessionKey, &oauth.ClientSessionData{
		AccountDID:  did,
		SessionID:   "block-handler-test",
		HostURL:     "http://pds.invalid",
		AccessToken: "test-access-token",
	}))
}

// decodeError reads the XRPC error envelope out of a response.
func decodeError(t *testing.T, w *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), "response body: %s", w.Body.String())
	return envelope.Error, envelope.Message
}

func TestHandleBlock_ReturnsTheRecordEnvelope(t *testing.T) {
	var receivedIdentifier string
	var receivedDID string

	handler := NewBlockHandler(&stubService{
		blockFunc: func(_ context.Context, session *oauth.ClientSessionData, identifier string) (*userblocks.BlockResult, error) {
			receivedIdentifier = identifier
			receivedDID = session.AccountDID.String()
			// The write-forward puts the record in the BLOCKER's repo, so the
			// URI the service produces is derived from the session it was given.
			return &userblocks.BlockResult{
				RecordURI: "at://" + session.AccountDID.String() + "/social.coves.actor.block/3lblock1",
				RecordCID: "bafyblockcid",
			}, nil
		},
	})

	w := httptest.NewRecorder()
	handler.HandleBlock(w, authenticated(t, http.MethodPost,
		"/xrpc/social.coves.actor.blockUser", map[string]string{"subject": "did:plc:blocktarget"}))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var response struct {
		Block struct {
			RecordURI string `json:"recordUri"`
			RecordCID string `json:"recordCid"`
		} `json:"block"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

	// The wire keys are camelCase per the lexicon; a rename here breaks every
	// client and nothing else in the codebase would notice.
	assert.Equal(t, "at://"+callerDID+"/social.coves.actor.block/3lblock1", response.Block.RecordURI)
	assert.Equal(t, "bafyblockcid", response.Block.RecordCID)

	// The handler must forward the CALLER's session, not a fresh or empty one:
	// a session dropped here writes the block record into the wrong repo.
	assert.Equal(t, callerDID, receivedDID)
	assert.Equal(t, "did:plc:blocktarget", receivedIdentifier,
		"the subject must reach the service unmodified — handles are resolved there, not here")
}

func TestHandleBlock_RequiresASubject(t *testing.T) {
	// The service is unstubbed on purpose: a subject-less request must be
	// rejected before anything is written anywhere.
	handler := NewBlockHandler(&stubService{})

	for _, tc := range []struct {
		name string
		body any
	}{
		{name: "absent", body: map[string]string{}},
		{name: "empty", body: map[string]string{"subject": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.HandleBlock(w, authenticated(t, http.MethodPost,
				"/xrpc/social.coves.actor.blockUser", tc.body))

			require.Equal(t, http.StatusBadRequest, w.Code)
			code, message := decodeError(t, w)
			assert.Equal(t, "InvalidRequest", code)
			assert.Contains(t, message, "subject",
				"the message must name the field, or a client cannot tell which one it got wrong")
		})
	}
}

func TestHandleBlock_MapsServiceErrors(t *testing.T) {
	// The mapping is the handler's whole contribution to these cases: the
	// service returns a sentinel and the client needs a status and an XRPC error
	// name it can branch on. The conditions that PRODUCE the sentinels are
	// covered in internal/core/userblocks/service_test.go.
	for _, tc := range []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name: "self-block", err: userblocks.ErrCannotBlockSelf,
			wantStatus: http.StatusBadRequest, wantCode: "InvalidRequest",
			// The one case where the MESSAGE is asserted and not just the code.
			// "InvalidRequest" is the handler's catch-all for four different
			// mistakes, so it is the message that tells a user which one they
			// made — and a client with no branch for this sentinel shows the
			// message verbatim. It is also the assertion that fails if the
			// sentinel is ever remapped onto the generic validation rule, which
			// would answer 400 InvalidRequest with a message about the subject
			// field being wrong rather than about who it points at.
			wantMessage: "cannot block yourself",
		},
		{
			// The PDS refused the record as a duplicate and the AppView has no
			// block indexed to hand back instead. 409 tells the client to retry
			// the read, not the write.
			name: "already blocked on the PDS but not indexed here", err: userblocks.ErrBlockAlreadyExists,
			wantStatus: http.StatusConflict, wantCode: "AlreadyExists",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewBlockHandler(&stubService{
				blockFunc: func(context.Context, *oauth.ClientSessionData, string) (*userblocks.BlockResult, error) {
					return nil, tc.err
				},
			})

			w := httptest.NewRecorder()
			handler.HandleBlock(w, authenticated(t, http.MethodPost,
				"/xrpc/social.coves.actor.blockUser", map[string]string{"subject": "did:plc:blocktarget"}))

			require.Equal(t, tc.wantStatus, w.Code, "body: %s", w.Body.String())
			code, message := decodeError(t, w)
			assert.Equal(t, tc.wantCode, code)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, message)
			}
		})
	}
}

func TestHandleUnblock_ReturnsSuccess(t *testing.T) {
	var receivedIdentifier string
	handler := NewBlockHandler(&stubService{
		unblockFunc: func(_ context.Context, _ *oauth.ClientSessionData, identifier string) error {
			receivedIdentifier = identifier
			return nil
		},
	})

	w := httptest.NewRecorder()
	handler.HandleUnblock(w, authenticated(t, http.MethodPost,
		"/xrpc/social.coves.actor.unblockUser", map[string]string{"subject": "did:plc:blocktarget"}))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.JSONEq(t, `{"success":true}`, w.Body.String())
	assert.Equal(t, "did:plc:blocktarget", receivedIdentifier)
}

func TestHandleUnblock_MapsBlockNotFound(t *testing.T) {
	handler := NewBlockHandler(&stubService{
		unblockFunc: func(context.Context, *oauth.ClientSessionData, string) error {
			return userblocks.ErrBlockNotFound
		},
	})

	w := httptest.NewRecorder()
	handler.HandleUnblock(w, authenticated(t, http.MethodPost,
		"/xrpc/social.coves.actor.unblockUser", map[string]string{"subject": "did:plc:blocktarget"}))

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	code, _ := decodeError(t, w)
	assert.Equal(t, "NotFound", code)
}

func TestHandleGetBlocked_ReturnsTheList(t *testing.T) {
	blockedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var gotDID string
	var gotLimit, gotOffset int

	handler := NewBlockHandler(&stubService{
		blockedFunc: func(_ context.Context, blockerDID string, limit, offset int) ([]*userblocks.UserBlock, error) {
			gotDID, gotLimit, gotOffset = blockerDID, limit, offset
			return []*userblocks.UserBlock{{
				BlockerDID: blockerDID,
				BlockedDID: "did:plc:blocked1",
				BlockedAt:  blockedAt,
				RecordURI:  "at://" + blockerDID + "/social.coves.actor.block/3lblock1",
				RecordCID:  "bafyblock1",
			}}, nil
		},
	})

	w := httptest.NewRecorder()
	handler.HandleGetBlocked(w, authenticated(t, http.MethodGet,
		"/xrpc/social.coves.actor.getBlockedUsers?limit=10&offset=5", nil))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The list is the caller's own — the DID comes from the session, never from
	// a query parameter, or one user could enumerate another's blocks.
	assert.Equal(t, callerDID, gotDID)
	assert.Equal(t, 10, gotLimit)
	assert.Equal(t, 5, gotOffset)

	assert.JSONEq(t, `{"blocks":[{
		"blockedDid": "did:plc:blocked1",
		"recordUri":  "at://`+callerDID+`/social.coves.actor.block/3lblock1",
		"recordCid":  "bafyblock1",
		"blockedAt":  "2026-01-02T03:04:05Z"
	}]}`, w.Body.String())
}

func TestHandleGetBlocked_EmptyListSerialisesAsAnArray(t *testing.T) {
	// The service returns a nil slice when nobody is blocked, and a nil slice
	// marshals to null. Clients that iterate the field would have to special-case
	// it, so the handler allocates an empty slice instead — a property that is
	// invisible in Go and only observable on the wire.
	handler := NewBlockHandler(&stubService{
		blockedFunc: func(context.Context, string, int, int) ([]*userblocks.UserBlock, error) {
			return nil, nil
		},
	})

	w := httptest.NewRecorder()
	handler.HandleGetBlocked(w, authenticated(t, http.MethodGet,
		"/xrpc/social.coves.actor.getBlockedUsers", nil))

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"blocks":[]}`, w.Body.String())
}

func TestHandleGetBlocked_RejectsUnparseablePagination(t *testing.T) {
	// An unparseable limit is a client bug, and answering it with the default
	// page would hand back results that silently ignore what was asked for.
	for _, tc := range []struct {
		name  string
		query string
		field string
	}{
		{name: "limit", query: "?limit=lots", field: "limit"},
		{name: "offset", query: "?offset=later", field: "offset"},
		{name: "limit before offset", query: "?limit=nope&offset=alsonope", field: "limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewBlockHandler(&stubService{})

			w := httptest.NewRecorder()
			handler.HandleGetBlocked(w, authenticated(t, http.MethodGet,
				"/xrpc/social.coves.actor.getBlockedUsers"+tc.query, nil))

			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			code, message := decodeError(t, w)
			assert.Equal(t, "InvalidRequest", code)
			assert.Contains(t, message, tc.field)
		})
	}
}

func TestHandleBlock_RejectsAMalformedBody(t *testing.T) {
	handler := NewBlockHandler(&stubService{})

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.blockUser",
		bytes.NewBufferString("{not json"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	code, _ := decodeError(t, w)
	assert.Equal(t, "InvalidRequest", code)
}

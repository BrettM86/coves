package community

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockTestService implements communities.Service for block handler tests
type blockTestService struct {
	blockFunc      func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error)
	unblockFunc    func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error
	getBlockedFunc func(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error)
}

func (m *blockTestService) CreateCommunity(ctx context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) ListCommunities(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	return nil, nil
}

func (m *blockTestService) SearchCommunities(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	return nil, 0, nil
}

func (m *blockTestService) SubscribeToCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string, contentVisibility int) (*communities.Subscription, error) {
	return nil, nil
}

func (m *blockTestService) UnsubscribeFromCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	return nil
}

func (m *blockTestService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *blockTestService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Subscription, error) {
	return nil, nil
}

func (m *blockTestService) BlockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
	if m.blockFunc != nil {
		return m.blockFunc(ctx, session, communityIdentifier)
	}
	userDID := ""
	if session != nil {
		userDID = session.AccountDID.String()
	}
	return &communities.CommunityBlock{
		UserDID:      userDID,
		CommunityDID: "did:plc:community123",
		RecordURI:    "at://did:plc:user/social.coves.community.block/abc123",
		RecordCID:    "bafytest123",
		BlockedAt:    time.Now(),
	}, nil
}

func (m *blockTestService) UnblockCommunity(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
	if m.unblockFunc != nil {
		return m.unblockFunc(ctx, session, communityIdentifier)
	}
	return nil
}

func (m *blockTestService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	if m.getBlockedFunc != nil {
		return m.getBlockedFunc(ctx, userDID, limit, offset)
	}
	return nil, nil
}

func (m *blockTestService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	return false, nil
}

func (m *blockTestService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error) {
	return nil, nil
}

func (m *blockTestService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*communities.Membership, error) {
	return nil, nil
}

func (m *blockTestService) ValidateHandle(handle string) error {
	return nil
}

func (m *blockTestService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (m *blockTestService) EnsureFreshToken(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	return community, nil
}

func (m *blockTestService) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return nil, nil
}

// createBlockTestOAuthSession creates a mock OAuth session for block handler tests
func createBlockTestOAuthSession(did string) *oauth.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauth.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   "test-session",
		HostURL:     testSessionPDSHostURL,
		AccessToken: "test-access-token",
	}
}

func TestBlockHandler_Block_Success(t *testing.T) {
	tests := []struct {
		name              string
		community         string
		expectedCommunity string
	}{
		{
			name:              "block with DID",
			community:         "did:plc:community123",
			expectedCommunity: "did:plc:community123",
		},
		{
			name:              "block with canonical handle",
			community:         "c-worldnews.coves.social",
			expectedCommunity: "c-worldnews.coves.social",
		},
		{
			name:              "block with scoped identifier",
			community:         "!worldnews@coves.social",
			expectedCommunity: "!worldnews@coves.social",
		},
		{
			name:              "block with at-identifier",
			community:         "@c-tech.coves.social",
			expectedCommunity: "@c-tech.coves.social",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedIdentifier string
			mockService := &blockTestService{
				blockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
					receivedIdentifier = communityIdentifier
					userDID := ""
					if session != nil {
						userDID = session.AccountDID.String()
					}
					return &communities.CommunityBlock{
						UserDID:      userDID,
						CommunityDID: "did:plc:resolved",
						RecordURI:    "at://did:plc:user/social.coves.community.block/abc123",
						RecordCID:    "bafytest123",
						BlockedAt:    time.Now(),
					}, nil
				},
			}

			handler := NewBlockHandler(mockService)

			reqBody := map[string]interface{}{
				"community": tc.community,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Inject OAuth session into context
			session := createBlockTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleBlock(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
			}

			// Verify the community identifier was passed through correctly
			if receivedIdentifier != tc.expectedCommunity {
				t.Errorf("Expected community %q to be passed to service, got %q", tc.expectedCommunity, receivedIdentifier)
			}

			// Verify response structure
			var resp struct {
				Block struct {
					RecordURI string `json:"recordUri"`
					RecordCID string `json:"recordCid"`
				} `json:"block"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}
			if resp.Block.RecordURI == "" || resp.Block.RecordCID == "" {
				t.Errorf("Expected recordUri and recordCid in response, got %+v", resp)
			}
		})
	}
}

func TestBlockHandler_Block_RequiresOAuthSession(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// No OAuth session in context

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}

	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthRequired" {
		t.Errorf("Expected error AuthRequired, got %s", errResp.Error)
	}
}

func TestBlockHandler_Block_RequiresCommunity(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	session := createBlockTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestBlockHandler_Block_ServiceErrors(t *testing.T) {
	tests := []struct {
		name           string
		serviceErr     error
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "community not found",
			serviceErr:     communities.ErrCommunityNotFound,
			expectedStatus: http.StatusNotFound,
			expectedError:  "NotFound",
		},
		{
			name:           "validation error",
			serviceErr:     communities.NewValidationError("community", "invalid format"),
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidRequest",
		},
		{
			name:           "already blocked",
			serviceErr:     communities.ErrBlockAlreadyExists,
			expectedStatus: http.StatusConflict,
			expectedError:  "AlreadyExists",
		},
		{
			name:           "unauthorized",
			serviceErr:     communities.ErrUnauthorized,
			expectedStatus: http.StatusForbidden,
			expectedError:  "Forbidden",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockService := &blockTestService{
				blockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) (*communities.CommunityBlock, error) {
					return nil, tc.serviceErr
				},
			}

			handler := NewBlockHandler(mockService)

			reqBody := map[string]interface{}{
				"community": "did:plc:test",
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			session := createBlockTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleBlock(w, req)

			if w.Code != tc.expectedStatus {
				t.Errorf("Expected status %d, got %d. Body: %s", tc.expectedStatus, w.Code, w.Body.String())
			}

			var errResp struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Error != tc.expectedError {
				t.Errorf("Expected error %s, got %s", tc.expectedError, errResp.Error)
			}
		})
	}
}

func TestBlockHandler_Unblock_Success(t *testing.T) {
	tests := []struct {
		name              string
		community         string
		expectedCommunity string
	}{
		{
			name:              "unblock with DID",
			community:         "did:plc:community123",
			expectedCommunity: "did:plc:community123",
		},
		{
			name:              "unblock with canonical handle",
			community:         "c-worldnews.coves.social",
			expectedCommunity: "c-worldnews.coves.social",
		},
		{
			name:              "unblock with scoped identifier",
			community:         "!worldnews@coves.social",
			expectedCommunity: "!worldnews@coves.social",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var receivedIdentifier string
			mockService := &blockTestService{
				unblockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
					receivedIdentifier = communityIdentifier
					return nil
				},
			}

			handler := NewBlockHandler(mockService)

			reqBody := map[string]interface{}{
				"community": tc.community,
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			session := createBlockTestOAuthSession("did:plc:testuser")
			ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleUnblock(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
			}

			if receivedIdentifier != tc.expectedCommunity {
				t.Errorf("Expected community %q to be passed to service, got %q", tc.expectedCommunity, receivedIdentifier)
			}

			var resp struct {
				Success bool `json:"success"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}
			if !resp.Success {
				t.Errorf("Expected success: true in response")
			}
		})
	}
}

func TestBlockHandler_Unblock_RequiresOAuthSession(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// No OAuth session in context

	w := httptest.NewRecorder()
	handler.HandleUnblock(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}

	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Error != "AuthRequired" {
		t.Errorf("Expected error AuthRequired, got %s", errResp.Error)
	}
}

func TestBlockHandler_Unblock_BlockNotFound(t *testing.T) {
	mockService := &blockTestService{
		unblockFunc: func(ctx context.Context, session *oauth.ClientSessionData, communityIdentifier string) error {
			return communities.ErrBlockNotFound
		},
	}

	handler := NewBlockHandler(mockService)

	reqBody := map[string]interface{}{
		"community": "did:plc:test",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	session := createBlockTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleUnblock(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d. Body: %s", w.Code, w.Body.String())
	}
}

func TestBlockHandler_MethodNotAllowed(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	// Test GET on block endpoint
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.blockCommunity", nil)
	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}

	// Test GET on unblock endpoint
	req = httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.unblockCommunity", nil)
	w = httptest.NewRecorder()
	handler.HandleUnblock(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestBlockHandler_InvalidJSON(t *testing.T) {
	mockService := &blockTestService{}
	handler := NewBlockHandler(mockService)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.blockCommunity", bytes.NewBufferString("invalid json"))
	req.Header.Set("Content-Type", "application/json")

	session := createBlockTestOAuthSession("did:plc:testuser")
	ctx := context.WithValue(req.Context(), middleware.OAuthSessionKey, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleBlock(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestBlockHandler_HandleGetBlocked(t *testing.T) {
	const (
		callerDID = "did:plc:communityblockcaller"
		endpoint  = "/xrpc/social.coves.community.getBlockedCommunities"
	)

	authenticatedRequest := func(method, target string) *http.Request {
		req := httptest.NewRequest(method, target, nil)
		session := createBlockTestOAuthSession(callerDID)
		return req.WithContext(context.WithValue(req.Context(), middleware.OAuthSessionKey, session))
	}

	t.Run("returns the caller's blocked communities", func(t *testing.T) {
		blockedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		var gotDID string
		var gotLimit, gotOffset int
		handler := NewBlockHandler(&blockTestService{
			getBlockedFunc: func(_ context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
				gotDID, gotLimit, gotOffset = userDID, limit, offset
				return []*communities.CommunityBlock{
					{
						CommunityDID: "did:plc:blockedcommunity1",
						RecordURI:    "at://" + userDID + "/social.coves.community.block/3lblock1",
						RecordCID:    "bafycommunityblock1",
						BlockedAt:    blockedAt,
					},
					{
						CommunityDID: "did:plc:blockedcommunity2",
						RecordURI:    "at://" + userDID + "/social.coves.community.block/3lblock2",
						RecordCID:    "bafycommunityblock2",
						BlockedAt:    blockedAt.Add(time.Hour),
					},
				}, nil
			},
		})

		w := httptest.NewRecorder()
		handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet,
			endpoint+"?userDID=did:plc:not-the-caller"))

		assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		assert.Equal(t, callerDID, gotDID,
			"the block list belongs to the authenticated session, never a DID supplied in the query string")
		assert.Equal(t, 50, gotLimit)
		assert.Equal(t, 0, gotOffset)
		assert.JSONEq(t, `{"blocks":[
			{"communityDid":"did:plc:blockedcommunity1","recordUri":"at://`+callerDID+`/social.coves.community.block/3lblock1","recordCid":"bafycommunityblock1","blockedAt":"2026-01-02T03:04:05Z"},
			{"communityDid":"did:plc:blockedcommunity2","recordUri":"at://`+callerDID+`/social.coves.community.block/3lblock2","recordCid":"bafycommunityblock2","blockedAt":"2026-01-02T04:04:05Z"}
		]}`, w.Body.String())
	})

	t.Run("passes limit and the cursor's offset through", func(t *testing.T) {
		var gotLimit, gotOffset int
		handler := NewBlockHandler(&blockTestService{
			getBlockedFunc: func(_ context.Context, _ string, limit, offset int) ([]*communities.CommunityBlock, error) {
				gotLimit, gotOffset = limit, offset
				return nil, nil
			},
		})

		w := httptest.NewRecorder()
		handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint+"?limit=7&cursor=3"))

		assert.Equal(t, 7, gotLimit)
		assert.Equal(t, 3, gotOffset)
	})

	t.Run("an empty list serialises as an empty array, not null", func(t *testing.T) {
		handler := NewBlockHandler(&blockTestService{
			getBlockedFunc: func(context.Context, string, int, int) ([]*communities.CommunityBlock, error) {
				return nil, nil
			},
		})

		w := httptest.NewRecorder()
		handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint))

		assert.JSONEq(t, `{"blocks":[]}`, w.Body.String())
		assert.NotContains(t, w.Body.String(), "null",
			"clients must receive an iterable empty array, not a JSON null")
	})

	t.Run("rejects a non-integer limit with 400 InvalidRequest", func(t *testing.T) {
		handler := NewBlockHandler(&blockTestService{})
		w := httptest.NewRecorder()
		handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint+"?limit=abc"))

		assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		assert.Contains(t, w.Body.String(), `"error":"InvalidRequest"`)
	})

	t.Run("rejects out-of-range values with 400 InvalidRequest", func(t *testing.T) {
		// The lexicon promises 1..100. Silently rewriting an invalid limit also
		// corrupts cursor arithmetic: limit=101 returning 50 rows makes a client
		// advance as though row 51 had been served and skip it permanently.
		for _, limit := range []string{"0", "101", "-5"} {
			t.Run("limit "+limit, func(t *testing.T) {
				calls := 0
				handler := NewBlockHandler(&blockTestService{
					getBlockedFunc: func(context.Context, string, int, int) ([]*communities.CommunityBlock, error) {
						calls++
						return nil, nil
					},
				})
				w := httptest.NewRecorder()
				handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint+"?limit="+limit))

				assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
				assert.Contains(t, w.Body.String(), `"error":"InvalidRequest"`)
				assert.Zero(t, calls, "invalid pagination must be rejected before querying the service")
			})
		}
	})

	t.Run("rejects a malformed cursor", func(t *testing.T) {
		for _, cursor := range []string{"abc", "-1"} {
			t.Run("cursor "+cursor, func(t *testing.T) {
				calls := 0
				handler := NewBlockHandler(&blockTestService{
					getBlockedFunc: func(context.Context, string, int, int) ([]*communities.CommunityBlock, error) {
						calls++
						return nil, nil
					},
				})
				w := httptest.NewRecorder()
				handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint+"?cursor="+cursor))

				assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
				assert.Contains(t, w.Body.String(), `"error":"InvalidRequest"`)
				assert.Zero(t, calls, "an invalid cursor must be rejected before querying the service")
			})
		}
	})

	t.Run("returns a next cursor only when the page is full", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			blockCount int
			wantCursor bool
		}{
			{name: "full page", blockCount: 2, wantCursor: true},
			{name: "partial page", blockCount: 1, wantCursor: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var gotOffset int
				handler := NewBlockHandler(&blockTestService{
					getBlockedFunc: func(_ context.Context, userDID string, _ int, offset int) ([]*communities.CommunityBlock, error) {
						gotOffset = offset
						blocks := make([]*communities.CommunityBlock, tc.blockCount)
						for i := range blocks {
							blocks[i] = &communities.CommunityBlock{
								UserDID:      userDID,
								CommunityDID: fmt.Sprintf("did:plc:cursorcommunity%d", i),
							}
						}
						return blocks, nil
					},
				})
				w := httptest.NewRecorder()
				handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint+"?limit=2&cursor=4"))

				require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
				assert.Equal(t, 4, gotOffset)
				var response map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				cursor, present := response["cursor"]
				if tc.wantCursor {
					require.True(t, present, "a full page must advertise the possible next page")
					assert.JSONEq(t, `"6"`, string(cursor))
				} else {
					assert.False(t, present,
						"the cursor key must be omitted when the short page proves there is no next page")
				}
			})
		}
	})

	t.Run("requires authentication", func(t *testing.T) {
		handler := NewBlockHandler(&blockTestService{})
		w := httptest.NewRecorder()
		handler.HandleGetBlocked(w, httptest.NewRequest(http.MethodGet, endpoint, nil))

		assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
		assert.Contains(t, w.Body.String(), `"error":"AuthRequired"`)
	})

	t.Run("a service failure is a 500 without internal detail", func(t *testing.T) {
		internalErr := errors.New("database password leaked")
		handler := NewBlockHandler(&blockTestService{
			getBlockedFunc: func(context.Context, string, int, int) ([]*communities.CommunityBlock, error) {
				return nil, internalErr
			},
		})

		w := httptest.NewRecorder()
		handler.HandleGetBlocked(w, authenticatedRequest(http.MethodGet, endpoint))

		assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
		assert.NotContains(t, w.Body.String(), internalErr.Error(),
			"an internal service error must not be exposed to the client")
	})
}

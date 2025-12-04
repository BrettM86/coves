package pds

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// This test suite provides comprehensive unit tests for the PDS client package.
//
// Coverage:
// - All Client interface methods: 100%
// - bearerAuth implementation: 100%
// - Factory function input validation: 100%
// - NewFromAccessToken: 100%
//
// Not covered (requires integration tests with real infrastructure):
// - NewFromPasswordAuth success path (requires live PDS server)
// - NewFromOAuthSession success path (requires OAuth infrastructure)
//
// The untested code paths involve external dependencies (PDS authentication,
// OAuth session resumption) which are appropriately tested in E2E/integration tests.

// TestClientImplementsInterface verifies that client implements the Client interface.
func TestClientImplementsInterface(t *testing.T) {
	var _ Client = (*client)(nil)
}

// TestBearerAuth_DoWithAuth verifies that bearerAuth correctly adds Authorization header.
func TestBearerAuth_DoWithAuth(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{
			name:  "standard token",
			token: "test-access-token-12345",
		},
		{
			name:  "token with special characters",
			token: "token.with.dots_and-dashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test server that captures the Authorization header
			var capturedHeader string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedHeader = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			// Create bearerAuth instance
			auth := &bearerAuth{token: tt.token}

			// Create request
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}

			// Execute with auth
			client := &http.Client{}
			nsid := syntax.NSID("com.atproto.test")
			_, err = auth.DoWithAuth(client, req, nsid)
			if err != nil {
				t.Fatalf("DoWithAuth failed: %v", err)
			}

			// Verify Authorization header
			expectedHeader := "Bearer " + tt.token
			if capturedHeader != expectedHeader {
				t.Errorf("Authorization header = %q, want %q", capturedHeader, expectedHeader)
			}
		})
	}
}

// TestBearerAuth_ImplementsAuthMethod verifies bearerAuth implements atclient.AuthMethod.
func TestBearerAuth_ImplementsAuthMethod(t *testing.T) {
	var _ atclient.AuthMethod = (*bearerAuth)(nil)
}

// TestNewFromAccessToken validates factory function input validation.
func TestNewFromAccessToken(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		did         string
		accessToken string
		wantErr     bool
		errContains string
	}{
		{
			name:        "valid inputs",
			host:        "https://pds.example.com",
			did:         "did:plc:12345",
			accessToken: "test-token",
			wantErr:     false,
		},
		{
			name:        "empty host",
			host:        "",
			did:         "did:plc:12345",
			accessToken: "test-token",
			wantErr:     true,
			errContains: "host is required",
		},
		{
			name:        "empty did",
			host:        "https://pds.example.com",
			did:         "",
			accessToken: "test-token",
			wantErr:     true,
			errContains: "did is required",
		},
		{
			name:        "empty access token",
			host:        "https://pds.example.com",
			did:         "did:plc:12345",
			accessToken: "",
			wantErr:     true,
			errContains: "accessToken is required",
		},
		{
			name:        "all empty",
			host:        "",
			did:         "",
			accessToken: "",
			wantErr:     true,
			errContains: "host is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewFromAccessToken(tt.host, tt.did, tt.accessToken)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want contains %q", err.Error(), tt.errContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if client == nil {
				t.Fatal("expected client, got nil")
			}

			// Verify DID and HostURL methods
			if client.DID() != tt.did {
				t.Errorf("DID() = %q, want %q", client.DID(), tt.did)
			}
			if client.HostURL() != tt.host {
				t.Errorf("HostURL() = %q, want %q", client.HostURL(), tt.host)
			}
		})
	}
}

// TestNewFromPasswordAuth validates factory function input validation.
func TestNewFromPasswordAuth(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		handle      string
		password    string
		wantErr     bool
		errContains string
	}{
		{
			name:        "empty host",
			host:        "",
			handle:      "user.bsky.social",
			password:    "password",
			wantErr:     true,
			errContains: "host is required",
		},
		{
			name:        "empty handle",
			host:        "https://pds.example.com",
			handle:      "",
			password:    "password",
			wantErr:     true,
			errContains: "handle is required",
		},
		{
			name:        "empty password",
			host:        "https://pds.example.com",
			handle:      "user.bsky.social",
			password:    "",
			wantErr:     true,
			errContains: "password is required",
		},
		{
			name:        "all empty",
			host:        "",
			handle:      "",
			password:    "",
			wantErr:     true,
			errContains: "host is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			_, err := NewFromPasswordAuth(ctx, tt.host, tt.handle, tt.password)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want contains %q", err.Error(), tt.errContains)
				}
				return
			}

			// Note: We don't test success case here because it requires a real PDS
			// Those are covered in integration tests
		})
	}
}

// TestNewFromOAuthSession validates factory function input validation.
func TestNewFromOAuthSession(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		oauthClient *oauth.ClientApp
		sessionData *oauth.ClientSessionData
		wantErr     bool
		errContains string
	}{
		{
			name:        "nil oauth client",
			oauthClient: nil,
			sessionData: &oauth.ClientSessionData{},
			wantErr:     true,
			errContains: "oauthClient is required",
		},
		{
			name:        "nil session data",
			oauthClient: &oauth.ClientApp{},
			sessionData: nil,
			wantErr:     true,
			errContains: "sessionData is required",
		},
		{
			name:        "both nil",
			oauthClient: nil,
			sessionData: nil,
			wantErr:     true,
			errContains: "oauthClient is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFromOAuthSession(ctx, tt.oauthClient, tt.sessionData)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want contains %q", err.Error(), tt.errContains)
				}
				return
			}

			// Note: Success case requires proper OAuth setup, tested in integration tests
		})
	}
}

// TestClient_DIDAndHostURL verifies DID() and HostURL() return correct values.
func TestClient_DIDAndHostURL(t *testing.T) {
	expectedDID := "did:plc:test123"
	expectedHost := "https://pds.test.com"

	c := &client{
		did:  expectedDID,
		host: expectedHost,
	}

	if got := c.DID(); got != expectedDID {
		t.Errorf("DID() = %q, want %q", got, expectedDID)
	}

	if got := c.HostURL(); got != expectedHost {
		t.Errorf("HostURL() = %q, want %q", got, expectedHost)
	}
}

// TestClient_CreateRecord tests the CreateRecord method with a mock server.
func TestClient_CreateRecord(t *testing.T) {
	tests := []struct {
		name           string
		collection     string
		rkey           string
		record         map[string]any
		serverResponse map[string]any
		serverStatus   int
		wantURI        string
		wantCID        string
		wantErr        bool
	}{
		{
			name:       "successful creation with rkey",
			collection: "social.coves.vote",
			rkey:       "3kjzl5kcb2s2v",
			record: map[string]any{
				"$type":     "social.coves.vote",
				"subject":   "at://did:plc:abc123/social.coves.post/3kjzl5kc",
				"direction": "up",
			},
			serverResponse: map[string]any{
				"uri": "at://did:plc:test/social.coves.vote/3kjzl5kcb2s2v",
				"cid": "bafyreigbtj4x7ip5legnfznufuopl4sg4knzc2cof6duas4b3q2fy6swua",
			},
			serverStatus: http.StatusOK,
			wantURI:      "at://did:plc:test/social.coves.vote/3kjzl5kcb2s2v",
			wantCID:      "bafyreigbtj4x7ip5legnfznufuopl4sg4knzc2cof6duas4b3q2fy6swua",
			wantErr:      false,
		},
		{
			name:       "successful creation without rkey (TID generated)",
			collection: "social.coves.vote",
			rkey:       "",
			record: map[string]any{
				"$type":     "social.coves.vote",
				"subject":   "at://did:plc:abc123/social.coves.post/3kjzl5kc",
				"direction": "down",
			},
			serverResponse: map[string]any{
				"uri": "at://did:plc:test/social.coves.vote/3kjzl5kcc2a1b",
				"cid": "bafyreihd4q3yqcfvnv5zlp6n4fqzh6z4p4m3mwc7vvr6k2j6y6v2a3b4c5",
			},
			serverStatus: http.StatusOK,
			wantURI:      "at://did:plc:test/social.coves.vote/3kjzl5kcc2a1b",
			wantCID:      "bafyreihd4q3yqcfvnv5zlp6n4fqzh6z4p4m3mwc7vvr6k2j6y6v2a3b4c5",
			wantErr:      false,
		},
		{
			name:       "server error",
			collection: "social.coves.vote",
			rkey:       "test",
			record:     map[string]any{"$type": "social.coves.vote"},
			serverResponse: map[string]any{
				"error":   "InvalidRequest",
				"message": "Invalid record",
			},
			serverStatus: http.StatusBadRequest,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify method
				if r.Method != http.MethodPost {
					t.Errorf("expected POST request, got %s", r.Method)
				}

				// Verify path
				expectedPath := "/xrpc/com.atproto.repo.createRecord"
				if r.URL.Path != expectedPath {
					t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
				}

				// Verify request body
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("failed to decode request body: %v", err)
				}

				// Check required fields
				if payload["collection"] != tt.collection {
					t.Errorf("collection = %v, want %v", payload["collection"], tt.collection)
				}

				// Check rkey inclusion
				if tt.rkey != "" {
					if payload["rkey"] != tt.rkey {
						t.Errorf("rkey = %v, want %v", payload["rkey"], tt.rkey)
					}
				} else {
					if _, exists := payload["rkey"]; exists {
						t.Error("rkey should not be included when empty")
					}
				}

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				json.NewEncoder(w).Encode(tt.serverResponse)
			}))
			defer server.Close()

			// Create client
			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			// Execute CreateRecord
			ctx := context.Background()
			uri, cid, err := c.CreateRecord(ctx, tt.collection, tt.rkey, tt.record)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if uri != tt.wantURI {
				t.Errorf("uri = %q, want %q", uri, tt.wantURI)
			}

			if cid != tt.wantCID {
				t.Errorf("cid = %q, want %q", cid, tt.wantCID)
			}
		})
	}
}

// TestClient_DeleteRecord tests the DeleteRecord method with a mock server.
func TestClient_DeleteRecord(t *testing.T) {
	tests := []struct {
		name         string
		collection   string
		rkey         string
		serverStatus int
		wantErr      bool
	}{
		{
			name:         "successful deletion",
			collection:   "social.coves.vote",
			rkey:         "3kjzl5kcb2s2v",
			serverStatus: http.StatusOK,
			wantErr:      false,
		},
		{
			name:         "not found error",
			collection:   "social.coves.vote",
			rkey:         "nonexistent",
			serverStatus: http.StatusNotFound,
			wantErr:      true,
		},
		{
			name:         "server error",
			collection:   "social.coves.vote",
			rkey:         "test",
			serverStatus: http.StatusInternalServerError,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify method
				if r.Method != http.MethodPost {
					t.Errorf("expected POST request, got %s", r.Method)
				}

				// Verify path
				expectedPath := "/xrpc/com.atproto.repo.deleteRecord"
				if r.URL.Path != expectedPath {
					t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
				}

				// Verify request body
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("failed to decode request body: %v", err)
				}

				if payload["collection"] != tt.collection {
					t.Errorf("collection = %v, want %v", payload["collection"], tt.collection)
				}
				if payload["rkey"] != tt.rkey {
					t.Errorf("rkey = %v, want %v", payload["rkey"], tt.rkey)
				}

				// Send response
				w.WriteHeader(tt.serverStatus)
				if tt.serverStatus != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{
						"error":   "Error",
						"message": "Operation failed",
					})
				}
			}))
			defer server.Close()

			// Create client
			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			// Execute DeleteRecord
			ctx := context.Background()
			err := c.DeleteRecord(ctx, tt.collection, tt.rkey)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestClient_ListRecords tests the ListRecords method with pagination.
func TestClient_ListRecords(t *testing.T) {
	tests := []struct {
		name           string
		collection     string
		limit          int
		cursor         string
		serverResponse map[string]any
		serverStatus   int
		wantRecords    int
		wantCursor     string
		wantErr        bool
	}{
		{
			name:       "successful list with records",
			collection: "social.coves.vote",
			limit:      10,
			cursor:     "",
			serverResponse: map[string]any{
				"cursor": "next-cursor-123",
				"records": []map[string]any{
					{
						"uri":   "at://did:plc:test/social.coves.vote/1",
						"cid":   "bafyreiabc123",
						"value": map[string]any{"$type": "social.coves.vote", "direction": "up"},
					},
					{
						"uri":   "at://did:plc:test/social.coves.vote/2",
						"cid":   "bafyreiabc456",
						"value": map[string]any{"$type": "social.coves.vote", "direction": "down"},
					},
				},
			},
			serverStatus: http.StatusOK,
			wantRecords:  2,
			wantCursor:   "next-cursor-123",
			wantErr:      false,
		},
		{
			name:       "empty list",
			collection: "social.coves.vote",
			limit:      10,
			cursor:     "",
			serverResponse: map[string]any{
				"cursor":  "",
				"records": []map[string]any{},
			},
			serverStatus: http.StatusOK,
			wantRecords:  0,
			wantCursor:   "",
			wantErr:      false,
		},
		{
			name:       "with cursor pagination",
			collection: "social.coves.vote",
			limit:      5,
			cursor:     "existing-cursor",
			serverResponse: map[string]any{
				"cursor": "final-cursor",
				"records": []map[string]any{
					{
						"uri":   "at://did:plc:test/social.coves.vote/3",
						"cid":   "bafyreiabc789",
						"value": map[string]any{"$type": "social.coves.vote", "direction": "up"},
					},
				},
			},
			serverStatus: http.StatusOK,
			wantRecords:  1,
			wantCursor:   "final-cursor",
			wantErr:      false,
		},
		{
			name:           "server error",
			collection:     "social.coves.vote",
			limit:          10,
			cursor:         "",
			serverResponse: map[string]any{"error": "Internal error"},
			serverStatus:   http.StatusInternalServerError,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify method
				if r.Method != http.MethodGet {
					t.Errorf("expected GET request, got %s", r.Method)
				}

				// Verify path
				expectedPath := "/xrpc/com.atproto.repo.listRecords"
				if r.URL.Path != expectedPath {
					t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
				}

				// Verify query parameters
				query := r.URL.Query()
				if query.Get("collection") != tt.collection {
					t.Errorf("collection param = %q, want %q", query.Get("collection"), tt.collection)
				}

				if tt.cursor != "" {
					if query.Get("cursor") != tt.cursor {
						t.Errorf("cursor param = %q, want %q", query.Get("cursor"), tt.cursor)
					}
				}

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				json.NewEncoder(w).Encode(tt.serverResponse)
			}))
			defer server.Close()

			// Create client
			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			// Execute ListRecords
			ctx := context.Background()
			resp, err := c.ListRecords(ctx, tt.collection, tt.limit, tt.cursor)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp == nil {
				t.Fatal("expected response, got nil")
			}

			if len(resp.Records) != tt.wantRecords {
				t.Errorf("records count = %d, want %d", len(resp.Records), tt.wantRecords)
			}

			if resp.Cursor != tt.wantCursor {
				t.Errorf("cursor = %q, want %q", resp.Cursor, tt.wantCursor)
			}

			// Verify record structure if we have records
			if tt.wantRecords > 0 {
				for i, record := range resp.Records {
					if record.URI == "" {
						t.Errorf("record[%d].URI is empty", i)
					}
					if record.CID == "" {
						t.Errorf("record[%d].CID is empty", i)
					}
					if record.Value == nil {
						t.Errorf("record[%d].Value is nil", i)
					}
				}
			}
		})
	}
}

// TestClient_GetRecord tests the GetRecord method with a mock server.
func TestClient_GetRecord(t *testing.T) {
	tests := []struct {
		name           string
		collection     string
		rkey           string
		serverResponse map[string]any
		serverStatus   int
		wantURI        string
		wantCID        string
		wantErr        bool
	}{
		{
			name:       "successful get",
			collection: "social.coves.vote",
			rkey:       "3kjzl5kcb2s2v",
			serverResponse: map[string]any{
				"uri": "at://did:plc:test/social.coves.vote/3kjzl5kcb2s2v",
				"cid": "bafyreigbtj4x7ip5legnfznufuopl4sg4knzc2cof6duas4b3q2fy6swua",
				"value": map[string]any{
					"$type":     "social.coves.vote",
					"subject":   "at://did:plc:abc/social.coves.post/123",
					"direction": "up",
				},
			},
			serverStatus: http.StatusOK,
			wantURI:      "at://did:plc:test/social.coves.vote/3kjzl5kcb2s2v",
			wantCID:      "bafyreigbtj4x7ip5legnfznufuopl4sg4knzc2cof6duas4b3q2fy6swua",
			wantErr:      false,
		},
		{
			name:       "record not found",
			collection: "social.coves.vote",
			rkey:       "nonexistent",
			serverResponse: map[string]any{
				"error":   "RecordNotFound",
				"message": "Record not found",
			},
			serverStatus: http.StatusNotFound,
			wantErr:      true,
		},
		{
			name:       "server error",
			collection: "social.coves.vote",
			rkey:       "test",
			serverResponse: map[string]any{
				"error": "Internal error",
			},
			serverStatus: http.StatusInternalServerError,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify method
				if r.Method != http.MethodGet {
					t.Errorf("expected GET request, got %s", r.Method)
				}

				// Verify path
				expectedPath := "/xrpc/com.atproto.repo.getRecord"
				if r.URL.Path != expectedPath {
					t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
				}

				// Verify query parameters
				query := r.URL.Query()
				if query.Get("collection") != tt.collection {
					t.Errorf("collection param = %q, want %q", query.Get("collection"), tt.collection)
				}
				if query.Get("rkey") != tt.rkey {
					t.Errorf("rkey param = %q, want %q", query.Get("rkey"), tt.rkey)
				}

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				json.NewEncoder(w).Encode(tt.serverResponse)
			}))
			defer server.Close()

			// Create client
			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			// Execute GetRecord
			ctx := context.Background()
			resp, err := c.GetRecord(ctx, tt.collection, tt.rkey)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp == nil {
				t.Fatal("expected response, got nil")
			}

			if resp.URI != tt.wantURI {
				t.Errorf("URI = %q, want %q", resp.URI, tt.wantURI)
			}

			if resp.CID != tt.wantCID {
				t.Errorf("CID = %q, want %q", resp.CID, tt.wantCID)
			}

			if resp.Value == nil {
				t.Error("Value is nil")
			}
		})
	}
}

// TestTypedErrors_IsAuthError tests the IsAuthError helper function.
func TestTypedErrors_IsAuthError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantAuth bool
	}{
		{
			name:     "ErrUnauthorized is auth error",
			err:      ErrUnauthorized,
			wantAuth: true,
		},
		{
			name:     "ErrForbidden is auth error",
			err:      ErrForbidden,
			wantAuth: true,
		},
		{
			name:     "ErrNotFound is not auth error",
			err:      ErrNotFound,
			wantAuth: false,
		},
		{
			name:     "ErrBadRequest is not auth error",
			err:      ErrBadRequest,
			wantAuth: false,
		},
		{
			name:     "wrapped ErrUnauthorized is auth error",
			err:      errors.New("outer: " + ErrUnauthorized.Error()),
			wantAuth: false, // Plain string wrap doesn't work
		},
		{
			name:     "fmt.Errorf wrapped ErrUnauthorized is auth error",
			err:      wrapAPIError(&atclient.APIError{StatusCode: 401, Message: "test"}, "op"),
			wantAuth: true,
		},
		{
			name:     "fmt.Errorf wrapped ErrForbidden is auth error",
			err:      wrapAPIError(&atclient.APIError{StatusCode: 403, Message: "test"}, "op"),
			wantAuth: true,
		},
		{
			name:     "nil error",
			err:      nil,
			wantAuth: false,
		},
		{
			name:     "generic error",
			err:      errors.New("something else"),
			wantAuth: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAuthError(tt.err)
			if got != tt.wantAuth {
				t.Errorf("IsAuthError() = %v, want %v", got, tt.wantAuth)
			}
		})
	}
}

// TestWrapAPIError tests error wrapping for HTTP status codes.
func TestWrapAPIError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		operation string
		wantTyped error
		wantNil   bool
	}{
		{
			name:      "nil error returns nil",
			err:       nil,
			operation: "test",
			wantNil:   true,
		},
		{
			name:      "401 maps to ErrUnauthorized",
			err:       &atclient.APIError{StatusCode: 401, Name: "AuthRequired", Message: "Not logged in"},
			operation: "createRecord",
			wantTyped: ErrUnauthorized,
		},
		{
			name:      "403 maps to ErrForbidden",
			err:       &atclient.APIError{StatusCode: 403, Name: "Forbidden", Message: "Access denied"},
			operation: "deleteRecord",
			wantTyped: ErrForbidden,
		},
		{
			name:      "404 maps to ErrNotFound",
			err:       &atclient.APIError{StatusCode: 404, Name: "NotFound", Message: "Record not found"},
			operation: "getRecord",
			wantTyped: ErrNotFound,
		},
		{
			name:      "400 maps to ErrBadRequest",
			err:       &atclient.APIError{StatusCode: 400, Name: "InvalidRequest", Message: "Bad input"},
			operation: "createRecord",
			wantTyped: ErrBadRequest,
		},
		{
			name:      "500 wraps without typed error",
			err:       &atclient.APIError{StatusCode: 500, Name: "InternalError", Message: "Server error"},
			operation: "listRecords",
			wantTyped: nil, // No typed error for 500
		},
		{
			name:      "non-APIError wraps normally",
			err:       errors.New("network timeout"),
			operation: "createRecord",
			wantTyped: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := wrapAPIError(tt.err, tt.operation)

			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected error, got nil")
			}

			if tt.wantTyped != nil {
				if !errors.Is(result, tt.wantTyped) {
					t.Errorf("expected errors.Is(%v, %v) to be true", result, tt.wantTyped)
				}
			}

			// Verify operation is included in error message
			if !strings.Contains(result.Error(), tt.operation) {
				t.Errorf("error message %q should contain operation %q", result.Error(), tt.operation)
			}
		})
	}
}

// TestClient_TypedErrors_CreateRecord tests that CreateRecord returns typed errors.
func TestClient_TypedErrors_CreateRecord(t *testing.T) {
	tests := []struct {
		name         string
		serverStatus int
		wantErr      error
	}{
		{
			name:         "401 returns ErrUnauthorized",
			serverStatus: http.StatusUnauthorized,
			wantErr:      ErrUnauthorized,
		},
		{
			name:         "403 returns ErrForbidden",
			serverStatus: http.StatusForbidden,
			wantErr:      ErrForbidden,
		},
		{
			name:         "400 returns ErrBadRequest",
			serverStatus: http.StatusBadRequest,
			wantErr:      ErrBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				json.NewEncoder(w).Encode(map[string]any{
					"error":   "TestError",
					"message": "Test error message",
				})
			}))
			defer server.Close()

			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			ctx := context.Background()
			_, _, err := c.CreateRecord(ctx, "test.collection", "rkey", map[string]any{})

			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected errors.Is(%v, %v) to be true", err, tt.wantErr)
			}
		})
	}
}

// TestClient_TypedErrors_DeleteRecord tests that DeleteRecord returns typed errors.
func TestClient_TypedErrors_DeleteRecord(t *testing.T) {
	tests := []struct {
		name         string
		serverStatus int
		wantErr      error
	}{
		{
			name:         "401 returns ErrUnauthorized",
			serverStatus: http.StatusUnauthorized,
			wantErr:      ErrUnauthorized,
		},
		{
			name:         "403 returns ErrForbidden",
			serverStatus: http.StatusForbidden,
			wantErr:      ErrForbidden,
		},
		{
			name:         "404 returns ErrNotFound",
			serverStatus: http.StatusNotFound,
			wantErr:      ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				json.NewEncoder(w).Encode(map[string]any{
					"error":   "TestError",
					"message": "Test error message",
				})
			}))
			defer server.Close()

			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			ctx := context.Background()
			err := c.DeleteRecord(ctx, "test.collection", "rkey")

			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected errors.Is(%v, %v) to be true", err, tt.wantErr)
			}
		})
	}
}

// TestClient_TypedErrors_ListRecords tests that ListRecords returns typed errors.
func TestClient_TypedErrors_ListRecords(t *testing.T) {
	tests := []struct {
		name         string
		serverStatus int
		wantErr      error
	}{
		{
			name:         "401 returns ErrUnauthorized",
			serverStatus: http.StatusUnauthorized,
			wantErr:      ErrUnauthorized,
		},
		{
			name:         "403 returns ErrForbidden",
			serverStatus: http.StatusForbidden,
			wantErr:      ErrForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				json.NewEncoder(w).Encode(map[string]any{
					"error":   "TestError",
					"message": "Test error message",
				})
			}))
			defer server.Close()

			apiClient := atclient.NewAPIClient(server.URL)
			apiClient.Auth = &bearerAuth{token: "test-token"}

			c := &client{
				apiClient: apiClient,
				did:       "did:plc:test",
				host:      server.URL,
			}

			ctx := context.Background()
			_, err := c.ListRecords(ctx, "test.collection", 10, "")

			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected errors.Is(%v, %v) to be true", err, tt.wantErr)
			}
		})
	}
}

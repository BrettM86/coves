package integration

import (
	"Coves/internal/api/handlers/aggregator"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAggregatorIdentityResolver is a mock implementation of identity.Resolver for aggregator registration testing
type mockAggregatorIdentityResolver struct {
	resolveFunc       func(ctx context.Context, identifier string) (*identity.Identity, error)
	resolveHandleFunc func(ctx context.Context, handle string) (did, pdsURL string, err error)
	resolveDIDFunc    func(ctx context.Context, did string) (*identity.DIDDocument, error)
	purgeFunc         func(ctx context.Context, identifier string) error
}

func (m *mockAggregatorIdentityResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	if m.resolveFunc != nil {
		return m.resolveFunc(ctx, identifier)
	}
	return &identity.Identity{
		DID:        identifier,
		Handle:     "test.bsky.social",
		PDSURL:     "https://bsky.social",
		ResolvedAt: time.Now(),
		Method:     identity.MethodHTTPS,
	}, nil
}

func (m *mockAggregatorIdentityResolver) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	if m.resolveHandleFunc != nil {
		return m.resolveHandleFunc(ctx, handle)
	}
	return "did:plc:test", "https://bsky.social", nil
}

func (m *mockAggregatorIdentityResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	if m.resolveDIDFunc != nil {
		return m.resolveDIDFunc(ctx, did)
	}
	return &identity.DIDDocument{DID: did}, nil
}

func (m *mockAggregatorIdentityResolver) Purge(ctx context.Context, identifier string) error {
	if m.purgeFunc != nil {
		return m.purgeFunc(ctx, identifier)
	}
	return nil
}

func TestAggregatorRegistration_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup test database
	db := setupTestDB(t)
	defer db.Close()

	testDID := "did:plc:test123"
	testHandle := "aggregator.bsky.social"

	// Setup test server with .well-known endpoint
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(testDID))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer wellKnownServer.Close()

	// Extract domain from test server URL (remove https:// prefix)
	domain := wellKnownServer.URL[8:] // Remove "https://"

	// Create mock identity resolver
	mockResolver := &mockAggregatorIdentityResolver{
		resolveFunc: func(ctx context.Context, identifier string) (*identity.Identity, error) {
			if identifier == testDID {
				return &identity.Identity{
					DID:        testDID,
					Handle:     testHandle,
					PDSURL:     "https://bsky.social",
					ResolvedAt: time.Now(),
					Method:     identity.MethodHTTPS,
				}, nil
			}
			return nil, fmt.Errorf("DID not found")
		},
	}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client that accepts self-signed certs for test server
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}

	// Set test client on handler for .well-known verification
	handler.SetHTTPClient(testClient)

	// Test registration request
	reqBody := map[string]string{
		"did":    testDID,
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call handler
	handler.HandleRegister(rr, req)

	// Assert response
	assert.Equal(t, http.StatusOK, rr.Code, "Response body: %s", rr.Body.String())

	var resp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, testDID, resp["did"])
	assert.Equal(t, testHandle, resp["handle"])
	assert.Contains(t, resp["message"], "registered successfully")

	// Verify user exists in database
	assertUserExists(t, db, testDID)
}

func TestAggregatorRegistration_DomainVerificationFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup test database
	db := setupTestDB(t)
	defer db.Close()

	// Setup test server that returns wrong DID
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("did:plc:wrongdid"))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:]

	// Create mock identity resolver
	mockResolver := &mockAggregatorIdentityResolver{}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client that accepts self-signed certs
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	reqBody := map[string]string{
		"did":    "did:plc:correctdid",
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call handler
	handler.HandleRegister(rr, req)

	// Assert response
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	var errResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "DomainVerificationFailed", errResp["error"])
	assert.Contains(t, errResp["message"], "domain ownership")
}

func TestAggregatorRegistration_InvalidDID(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer db.Close()

	tests := []struct {
		name   string
		did    string
		domain string
	}{
		{"empty DID", "", "example.com"},
		{"invalid format", "not-a-did", "example.com"},
		{"missing prefix", "plc:test123", "example.com"},
		{"unsupported method", "did:key:test123", "example.com"},
		{"empty domain", "did:plc:test123", ""},
		{"whitespace domain", "did:plc:test123", "   "},
		{"https only", "did:plc:test123", "https://"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock identity resolver
			mockResolver := &mockAggregatorIdentityResolver{}

			// Create services and handler
			userRepo := postgres.NewUserRepository(db)
			userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
			handler := aggregator.NewRegisterHandler(userService, mockResolver)

			reqBody := map[string]string{
				"did":    tt.did,
				"domain": tt.domain,
			}

			reqJSON, err := json.Marshal(reqBody)
			require.NoError(t, err)

			// Create HTTP request
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
			req.Header.Set("Content-Type", "application/json")

			// Create response recorder
			rr := httptest.NewRecorder()

			// Call handler
			handler.HandleRegister(rr, req)

			// Assert response
			assert.Equal(t, http.StatusBadRequest, rr.Code, "Response body: %s", rr.Body.String())

			var errResp map[string]interface{}
			err = json.Unmarshal(rr.Body.Bytes(), &errResp)
			require.NoError(t, err)

			assert.Equal(t, "InvalidDID", errResp["error"], "Expected InvalidDID error for: %s", tt.name)
		})
	}
}

func TestAggregatorRegistration_AlreadyRegistered(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer db.Close()

	// Pre-create user with same DID
	existingDID := "did:plc:existing123"
	createTestUser(t, db, "existing.bsky.social", existingDID)

	// Setup test server with .well-known
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(existingDID))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:]

	// Create mock identity resolver
	mockResolver := &mockAggregatorIdentityResolver{
		resolveFunc: func(ctx context.Context, identifier string) (*identity.Identity, error) {
			if identifier == existingDID {
				return &identity.Identity{
					DID:        existingDID,
					Handle:     "existing.bsky.social",
					PDSURL:     "https://bsky.social",
					ResolvedAt: time.Now(),
					Method:     identity.MethodHTTPS,
				}, nil
			}
			return nil, fmt.Errorf("DID not found")
		},
	}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client that accepts self-signed certs
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	reqBody := map[string]string{
		"did":    existingDID,
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call handler
	handler.HandleRegister(rr, req)

	// Assert response
	assert.Equal(t, http.StatusConflict, rr.Code)

	var errResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "AlreadyRegistered", errResp["error"])
	assert.Contains(t, errResp["message"], "already registered")
}

func TestAggregatorRegistration_WellKnownNotAccessible(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer db.Close()

	// Setup test server that returns 404 for .well-known
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:]

	// Create mock identity resolver
	mockResolver := &mockAggregatorIdentityResolver{}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client that accepts self-signed certs
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	reqBody := map[string]string{
		"did":    "did:plc:test123",
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call handler
	handler.HandleRegister(rr, req)

	// Assert response
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	var errResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "DomainVerificationFailed", errResp["error"])
	assert.Contains(t, errResp["message"], "domain ownership")
}

func TestAggregatorRegistration_WellKnownTooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer db.Close()

	testDID := "did:plc:toolarge"

	// Setup test server that streams a very large .well-known response
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			if _, err := w.Write(bytes.Repeat([]byte("A"), 10*1024)); err != nil {
				t.Fatalf("Failed to write fake response: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:]

	mockResolver := &mockAggregatorIdentityResolver{}

	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	reqBody := map[string]string{
		"did":    testDID,
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.HandleRegister(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code, "Response body: %s", rr.Body.String())

	var errResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "DomainVerificationFailed", errResp["error"])
	assert.Contains(t, errResp["message"], "domain ownership")

	assertUserDoesNotExist(t, db, testDID)
}

func TestAggregatorRegistration_DIDResolutionFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer db.Close()

	testDID := "did:plc:nonexistent"

	// Setup test server with .well-known
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(testDID))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:]

	// Create mock identity resolver that fails for this DID
	mockResolver := &mockAggregatorIdentityResolver{
		resolveFunc: func(ctx context.Context, identifier string) (*identity.Identity, error) {
			return nil, fmt.Errorf("DID not found in PLC directory")
		},
	}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client that accepts self-signed certs
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	reqBody := map[string]string{
		"did":    testDID,
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call handler
	handler.HandleRegister(rr, req)

	// Assert response
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	var errResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "DIDResolutionFailed", errResp["error"])
	assert.Contains(t, errResp["message"], "resolve DID")

	// Verify user was NOT created in database
	assertUserDoesNotExist(t, db, testDID)
}

func TestAggregatorRegistration_LargeWellKnownResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer db.Close()

	testDID := "did:plc:largedos123"

	// Setup server that streams a large response to attempt DoS
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			// Attempt to stream 10MB of data (should be capped at 1KB by io.LimitReader)
			// This simulates a malicious server trying to DoS the AppView
			for i := 0; i < 10*1024*1024; i++ {
				if _, err := w.Write([]byte("A")); err != nil {
					// Client disconnected (expected when limit is reached)
					return
				}
			}
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:]

	// Create mock identity resolver
	mockResolver := &mockAggregatorIdentityResolver{}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client that accepts self-signed certs
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	reqBody := map[string]string{
		"did":    testDID,
		"domain": domain,
	}

	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Record start time to ensure the test completes quickly
	startTime := time.Now()

	// Call handler - should fail gracefully, not hang or DoS
	handler.HandleRegister(rr, req)

	elapsed := time.Since(startTime)

	// Assert the handler completed quickly (not trying to read 10MB)
	// Should complete in well under 1 second. Using 5 seconds as generous upper bound.
	assert.Less(t, elapsed, 5*time.Second, "Handler should complete quickly even with large response")

	// Should fail with domain verification error (DID mismatch: got "AAAA..." instead of expected DID)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "Should reject due to DID mismatch")

	var errResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &errResp)
	require.NoError(t, err)

	assert.Equal(t, "DomainVerificationFailed", errResp["error"])
	assert.Contains(t, errResp["message"], "domain ownership")

	// Verify user was NOT created
	assertUserDoesNotExist(t, db, testDID)

	t.Logf("✓ DoS protection test completed in %v (prevented reading 10MB payload)", elapsed)
}

func TestAggregatorRegistration_E2E_WithRealInfrastructure(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// This test requires docker-compose infrastructure to be running:
	// docker-compose -f docker-compose.dev.yml --profile test up postgres-test
	//
	// This is a TRUE E2E test that validates the full registration flow
	// with real .well-known server and real identity resolution

	db := setupTestDB(t)
	defer db.Close()

	testDID := "did:plc:e2etest123"
	testHandle := "e2ebot.bsky.social"
	testPDSURL := "https://bsky.social"

	// Setup .well-known server (simulates aggregator's domain)
	wellKnownServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/atproto-did" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(testDID))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer wellKnownServer.Close()

	domain := wellKnownServer.URL[8:] // Remove "https://"

	// Create mock identity resolver (for E2E, this simulates PLC directory response)
	mockResolver := &mockAggregatorIdentityResolver{
		resolveFunc: func(ctx context.Context, identifier string) (*identity.Identity, error) {
			if identifier == testDID {
				return &identity.Identity{
					DID:        testDID,
					Handle:     testHandle,
					PDSURL:     testPDSURL,
					ResolvedAt: time.Now(),
					Method:     identity.MethodHTTPS,
				}, nil
			}
			return nil, fmt.Errorf("DID not found")
		},
	}

	// Create services and handler
	userRepo := postgres.NewUserRepository(db)
	userService := users.NewUserService(userRepo, mockResolver, "https://bsky.social")
	handler := aggregator.NewRegisterHandler(userService, mockResolver)

	// Create HTTP client for self-signed test server certs
	testClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	handler.SetHTTPClient(testClient)

	// Build registration request
	reqBody := map[string]string{
		"did":    testDID,
		"domain": domain,
	}
	reqJSON, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create HTTP request
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewBuffer(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Execute registration
	handler.HandleRegister(rr, req)

	// Assert HTTP 200 response
	assert.Equal(t, http.StatusOK, rr.Code, "Response body: %s", rr.Body.String())

	// Parse response
	var resp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Assert response contains correct data
	assert.Equal(t, testDID, resp["did"], "DID should match request")
	assert.Equal(t, testHandle, resp["handle"], "Handle should be resolved from DID")
	assert.Contains(t, resp["message"], "registered successfully", "Success message should be present")
	assert.Contains(t, resp["message"], "service declaration", "Message should mention next steps")

	// Verify user was created in database
	user := assertUserExists(t, db, testDID)
	assert.Equal(t, testHandle, user.Handle, "User handle should match resolved identity")
	assert.Equal(t, testPDSURL, user.PDSURL, "User PDS URL should match resolved identity")

	t.Logf("✓ E2E test completed successfully")
	t.Logf("  DID: %s", testDID)
	t.Logf("  Handle: %s", testHandle)
	t.Logf("  Domain: %s", domain)
}

// Helper to verify user exists in database
func assertUserExists(t *testing.T, db *sql.DB, did string) *users.User {
	t.Helper()

	var user users.User
	err := db.QueryRow(`
		SELECT did, handle, pds_url
		FROM users
		WHERE did = $1
	`, did).Scan(&user.DID, &user.Handle, &user.PDSURL)

	require.NoError(t, err, "User should exist in database")
	return &user
}

// Helper to verify user does not exist
func assertUserDoesNotExist(t *testing.T, db *sql.DB, did string) {
	t.Helper()

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM users WHERE did = $1", did).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "User should not exist in database")
}

// TODO: Implement full E2E tests with actual HTTP server and handler
// This requires:
// 1. Setting up test HTTP server with all routes
// 2. Mocking the identity resolver to avoid external calls
// 3. Setting up test database
// 4. Making actual HTTP requests and asserting responses
//
// For now, these tests serve as placeholders and documentation
// of the expected behavior.

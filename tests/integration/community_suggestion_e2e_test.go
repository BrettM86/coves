//go:build integration

package integration

import (
	"Coves/internal/api/routes"
	"Coves/internal/core/communitysuggestions"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// --- Test helpers ---

// suggestionResponse represents the JSON response for a single community suggestion
type suggestionResponse struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	SubmitterDID string `json:"submitterDid"`
	Status       string `json:"status"`
	VoteCount    int    `json:"voteCount"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	Viewer       *struct {
		Vote *int `json:"vote"`
	} `json:"viewer"`
}

// listSuggestionsResponse represents the JSON response for listing suggestions
type listSuggestionsResponse struct {
	Suggestions []suggestionResponse `json:"suggestions"`
	Cursor      string               `json:"cursor"`
}

// xrpcErrorResponse represents an XRPC error response
type xrpcErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// createTestSuggestionRequest creates a suggestion via the HTTP API and returns the response recorder.
// Does NOT fail the test on non-200 responses so callers can assert specific error codes.
func createTestSuggestionRequest(t *testing.T, router http.Handler, token, title, description string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"title":       title,
		"description": description,
	})
	if err != nil {
		t.Fatalf("Failed to marshal create suggestion request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/xrpc/social.coves.community.suggestion.create",
		bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// mustCreateTestSuggestion creates a suggestion and fails the test if it doesn't succeed.
// Returns the decoded suggestion response.
func mustCreateTestSuggestion(t *testing.T, router http.Handler, token, title, description string) suggestionResponse {
	t.Helper()

	rec := createTestSuggestionRequest(t, router, token, title, description)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 creating suggestion, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp suggestionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode create suggestion response: %v", err)
	}
	return resp
}

// voteOnSuggestionRequest casts a vote via the HTTP API and returns the response recorder.
func voteOnSuggestionRequest(t *testing.T, router http.Handler, token string, suggestionID int64, value int) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]interface{}{
		"suggestionId": suggestionID,
		"value":        value,
	})
	if err != nil {
		t.Fatalf("Failed to marshal vote request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/xrpc/social.coves.community.suggestion.vote",
		bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// removeVoteRequest removes a vote via the HTTP API and returns the response recorder.
func removeVoteRequest(t *testing.T, router http.Handler, token string, suggestionID int64) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]interface{}{
		"suggestionId": suggestionID,
	})
	if err != nil {
		t.Fatalf("Failed to marshal remove vote request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/xrpc/social.coves.community.suggestion.removeVote",
		bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// getSuggestionRequest fetches a suggestion by ID via the HTTP API.
func getSuggestionRequest(t *testing.T, router http.Handler, token string, id int64) *httptest.ResponseRecorder {
	t.Helper()

	url := fmt.Sprintf("/xrpc/social.coves.community.suggestion.get?id=%d", id)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// listSuggestionsRequest lists suggestions via the HTTP API with query params.
func listSuggestionsRequest(t *testing.T, router http.Handler, token string, queryParams string) *httptest.ResponseRecorder {
	t.Helper()

	url := "/xrpc/social.coves.community.suggestion.list"
	if queryParams != "" {
		url += "?" + queryParams
	}

	req := httptest.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// updateStatusRequest updates a suggestion's status via the HTTP API.
func updateStatusRequest(t *testing.T, router http.Handler, token string, suggestionID int64, status string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]interface{}{
		"suggestionId": suggestionID,
		"status":       status,
	})
	if err != nil {
		t.Fatalf("Failed to marshal update status request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/xrpc/social.coves.community.suggestion.updateStatus",
		bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// setupSuggestionTestRouter sets up a chi router with real community suggestion handlers,
// a real PostgreSQL repository, and a mock OAuth middleware for authentication injection.
// Returns the router, the E2EOAuthMiddleware (for adding users), and a cleanup function.
func setupSuggestionTestRouter(t *testing.T, adminDIDs []string) (http.Handler, *E2EOAuthMiddleware) {
	t.Helper()

	db := testkit.DB(t)

	// Wire up real repository and service
	repo := postgres.NewCommunitySuggestionRepository(db)
	service := communitysuggestions.NewService(repo)

	// Create E2E OAuth middleware for injecting test users
	e2eAuth := NewE2EOAuthMiddleware()

	// Set up chi router with real handlers via route registration
	r := chi.NewRouter()
	routes.RegisterCommunitySuggestionRoutes(r, service, e2eAuth.OAuthAuthMiddleware, adminDIDs)

	return r, e2eAuth
}

// TestCommunitySuggestionE2E is the comprehensive E2E integration test for the
// Community Suggestions & Voting feature. It tests the full stack:
// HTTP handlers -> service -> repository -> PostgreSQL.
func TestCommunitySuggestionE2E(t *testing.T) {
	adminDID := "did:plc:testadmin"
	userDID := "did:plc:testuser1"
	user2DID := "did:plc:testuser2"
	user3DID := "did:plc:testuser3"

	router, e2eAuth := setupSuggestionTestRouter(t, []string{adminDID})

	// Register test users with the mock auth system
	_ = e2eAuth.AddUser(adminDID) // admin registered for shared router
	userToken := e2eAuth.AddUser(userDID)
	user2Token := e2eAuth.AddUser(user2DID)
	_ = e2eAuth.AddUser(user3DID) // registered but used in subtests with own routers

	// =====================================================================
	// Test: Create Suggestion
	// =====================================================================
	t.Run("Create suggestion", func(t *testing.T) {
		rec := createTestSuggestionRequest(t, router, userToken, "Golang Community", "A place for Go developers to share and learn")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp suggestionResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if resp.ID <= 0 {
			t.Errorf("Expected positive ID, got %d", resp.ID)
		}
		if resp.Title != "Golang Community" {
			t.Errorf("Expected title 'Golang Community', got %q", resp.Title)
		}
		if resp.Description != "A place for Go developers to share and learn" {
			t.Errorf("Expected description to match, got %q", resp.Description)
		}
		if resp.Status != "open" {
			t.Errorf("Expected status 'open', got %q", resp.Status)
		}
		if resp.VoteCount != 0 {
			t.Errorf("Expected voteCount 0, got %d", resp.VoteCount)
		}
		if resp.SubmitterDID != userDID {
			t.Errorf("Expected submitterDid %q, got %q", userDID, resp.SubmitterDID)
		}
		if resp.CreatedAt == "" {
			t.Error("Expected createdAt to be populated")
		}
		if resp.UpdatedAt == "" {
			t.Error("Expected updatedAt to be populated")
		}
	})

	// =====================================================================
	// Test: Create Suggestion - Validation Errors
	// =====================================================================
	t.Run("Create suggestion - missing title", func(t *testing.T) {
		rec := createTestSuggestionRequest(t, router, userToken, "", "Some description")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400, got %d: %s", rec.Code, rec.Body.String())
		}

		var errResp xrpcErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("Failed to decode error response: %v", err)
		}
		if errResp.Error != "InvalidRequest" {
			t.Errorf("Expected error 'InvalidRequest', got %q", errResp.Error)
		}
	})

	t.Run("Create suggestion - empty description", func(t *testing.T) {
		rec := createTestSuggestionRequest(t, router, userToken, "Valid Title", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Create suggestion - title too long", func(t *testing.T) {
		longTitle := strings.Repeat("a", communitysuggestions.MaxTitleLength+1)
		rec := createTestSuggestionRequest(t, router, userToken, longTitle, "Valid description")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400, got %d: %s", rec.Code, rec.Body.String())
		}

		var errResp xrpcErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("Failed to decode error response: %v", err)
		}
		if errResp.Error != "InvalidRequest" {
			t.Errorf("Expected error 'InvalidRequest', got %q", errResp.Error)
		}
	})

	// =====================================================================
	// Test: Create Suggestion - Auth Required
	// =====================================================================
	t.Run("Create suggestion - auth required", func(t *testing.T) {
		rec := createTestSuggestionRequest(t, router, "", "No Auth", "Should fail")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// =====================================================================
	// Test: Get Suggestion by ID
	// =====================================================================
	t.Run("Get suggestion by ID", func(t *testing.T) {
		// Create a suggestion first
		created := mustCreateTestSuggestion(t, router, user2Token,
			"Get Test Community", "Community to test get endpoint")

		rec := getSuggestionRequest(t, router, user2Token, created.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp suggestionResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if resp.ID != created.ID {
			t.Errorf("Expected ID %d, got %d", created.ID, resp.ID)
		}
		if resp.Title != "Get Test Community" {
			t.Errorf("Expected title 'Get Test Community', got %q", resp.Title)
		}
		if resp.Description != "Community to test get endpoint" {
			t.Errorf("Expected description to match, got %q", resp.Description)
		}
		if resp.Status != "open" {
			t.Errorf("Expected status 'open', got %q", resp.Status)
		}
	})

	// =====================================================================
	// Test: Get Suggestion - Not Found
	// =====================================================================
	t.Run("Get suggestion - not found", func(t *testing.T) {
		rec := getSuggestionRequest(t, router, userToken, 999999)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("Expected 404, got %d: %s", rec.Code, rec.Body.String())
		}

		var errResp xrpcErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("Failed to decode error response: %v", err)
		}
		if errResp.Error != "NotFound" {
			t.Errorf("Expected error 'NotFound', got %q", errResp.Error)
		}
	})

	// =====================================================================
	// Test: List Suggestions - Default Sort (Popular)
	// =====================================================================
	t.Run("List suggestions - default sort popular", func(t *testing.T) {
		// Create a fresh router to avoid pollution from previous tests
		listRouter, listAuth := setupSuggestionTestRouter(t, []string{adminDID})
		listUserToken := listAuth.AddUser(userDID)
		listUser2Token := listAuth.AddUser(user2DID)

		// Create two suggestions
		s1 := mustCreateTestSuggestion(t, listRouter, listUserToken,
			"Popular Community", "Should be sorted by votes")
		s2 := mustCreateTestSuggestion(t, listRouter, listUserToken,
			"Less Popular Community", "Should appear after popular")

		// Vote on the first suggestion to make it more popular
		voteRec := voteOnSuggestionRequest(t, listRouter, listUser2Token, s1.ID, 1)
		if voteRec.Code != http.StatusOK {
			t.Fatalf("Vote failed: %d: %s", voteRec.Code, voteRec.Body.String())
		}

		// List with default sort (popular)
		rec := listSuggestionsRequest(t, listRouter, listUserToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp listSuggestionsResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if len(resp.Suggestions) < 2 {
			t.Fatalf("Expected at least 2 suggestions, got %d", len(resp.Suggestions))
		}

		// First suggestion should be the one with votes (s1)
		if resp.Suggestions[0].ID != s1.ID {
			t.Errorf("Expected first suggestion ID %d (popular), got %d", s1.ID, resp.Suggestions[0].ID)
		}
		if resp.Suggestions[0].VoteCount != 1 {
			t.Errorf("Expected first suggestion voteCount 1, got %d", resp.Suggestions[0].VoteCount)
		}

		// Second suggestion should be the one without votes (s2)
		found := false
		for _, sg := range resp.Suggestions {
			if sg.ID == s2.ID {
				found = true
				if sg.VoteCount != 0 {
					t.Errorf("Expected s2 voteCount 0, got %d", sg.VoteCount)
				}
				break
			}
		}
		if !found {
			t.Error("Expected to find s2 in list results")
		}
	})

	// =====================================================================
	// Test: List Suggestions - Sort by New
	// =====================================================================
	t.Run("List suggestions - sort by new", func(t *testing.T) {
		listRouter, listAuth := setupSuggestionTestRouter(t, []string{adminDID})
		listUserToken := listAuth.AddUser(userDID)

		// Create two suggestions with a small delay to ensure different timestamps
		_ = mustCreateTestSuggestion(t, listRouter, listUserToken,
			"Older Community", "Created first")
		time.Sleep(10 * time.Millisecond)
		s2 := mustCreateTestSuggestion(t, listRouter, listUserToken,
			"Newer Community", "Created second")

		rec := listSuggestionsRequest(t, listRouter, listUserToken, "sort=new")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp listSuggestionsResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if len(resp.Suggestions) < 2 {
			t.Fatalf("Expected at least 2 suggestions, got %d", len(resp.Suggestions))
		}

		// First suggestion should be the newest (s2)
		if resp.Suggestions[0].ID != s2.ID {
			t.Errorf("Expected first suggestion ID %d (newest), got %d", s2.ID, resp.Suggestions[0].ID)
		}
	})

	// =====================================================================
	// Test: List Suggestions - Status Filter
	// =====================================================================
	t.Run("List suggestions - status filter", func(t *testing.T) {
		listRouter, listAuth := setupSuggestionTestRouter(t, []string{adminDID})
		listUserToken := listAuth.AddUser(userDID)
		listAdminToken := listAuth.AddUser(adminDID)

		// Create two suggestions
		s1 := mustCreateTestSuggestion(t, listRouter, listUserToken,
			"Open Suggestion", "Should remain open")
		_ = mustCreateTestSuggestion(t, listRouter, listUserToken,
			"To Be Approved", "Will be updated to approved")

		// Update s1 status to approved via admin
		statusRec := updateStatusRequest(t, listRouter, listAdminToken, s1.ID, "approved")
		if statusRec.Code != http.StatusOK {
			t.Fatalf("Status update failed: %d: %s", statusRec.Code, statusRec.Body.String())
		}

		// List only approved suggestions
		rec := listSuggestionsRequest(t, listRouter, listUserToken, "status=approved")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp listSuggestionsResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		// Should only contain approved suggestions
		for _, sg := range resp.Suggestions {
			if sg.Status != "approved" {
				t.Errorf("Expected all suggestions to have status 'approved', got %q for ID %d", sg.Status, sg.ID)
			}
		}

		// Should have at least 1 result
		if len(resp.Suggestions) == 0 {
			t.Error("Expected at least 1 approved suggestion")
		}

		// List only open suggestions
		rec2 := listSuggestionsRequest(t, listRouter, listUserToken, "status=open")
		if rec2.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec2.Code, rec2.Body.String())
		}

		var resp2 listSuggestionsResponse
		if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		for _, sg := range resp2.Suggestions {
			if sg.Status != "open" {
				t.Errorf("Expected all suggestions to have status 'open', got %q for ID %d", sg.Status, sg.ID)
			}
		}
	})

	// =====================================================================
	// Test: List Suggestions - Pagination
	// =====================================================================
	t.Run("List suggestions - pagination", func(t *testing.T) {
		listRouter, listAuth := setupSuggestionTestRouter(t, []string{adminDID})
		listUserToken := listAuth.AddUser(userDID)

		// Create 5 suggestions. Spread them across distinct submitters so we stay
		// under the per-user daily rate limit (MaxSuggestionsPerDay). Pagination
		// lists all open suggestions regardless of submitter, so this also mirrors
		// the realistic case of many users each proposing a community.
		for i := 0; i < 5; i++ {
			submitterToken := listAuth.AddUser(fmt.Sprintf("did:plc:paginator%d", i))
			mustCreateTestSuggestion(t, listRouter, submitterToken,
				fmt.Sprintf("Pagination Test %d", i),
				fmt.Sprintf("Description for pagination test %d", i))
		}

		// First page: limit=2
		rec := listSuggestionsRequest(t, listRouter, listUserToken, "sort=new&limit=2")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var page1 listSuggestionsResponse
		if err := json.NewDecoder(rec.Body).Decode(&page1); err != nil {
			t.Fatalf("Failed to decode page 1: %v", err)
		}

		if len(page1.Suggestions) != 2 {
			t.Fatalf("Expected 2 suggestions on page 1, got %d", len(page1.Suggestions))
		}

		// Cursor should be non-empty since there are more results
		if page1.Cursor == "" {
			t.Fatal("Expected non-empty cursor on page 1")
		}

		// Second page: use cursor from first page
		rec2 := listSuggestionsRequest(t, listRouter, listUserToken,
			fmt.Sprintf("sort=new&limit=2&cursor=%s", page1.Cursor))
		if rec2.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec2.Code, rec2.Body.String())
		}

		var page2 listSuggestionsResponse
		if err := json.NewDecoder(rec2.Body).Decode(&page2); err != nil {
			t.Fatalf("Failed to decode page 2: %v", err)
		}

		if len(page2.Suggestions) != 2 {
			t.Fatalf("Expected 2 suggestions on page 2, got %d", len(page2.Suggestions))
		}

		// Ensure page 2 suggestions are different from page 1
		page1IDs := map[int64]bool{
			page1.Suggestions[0].ID: true,
			page1.Suggestions[1].ID: true,
		}
		for _, sg := range page2.Suggestions {
			if page1IDs[sg.ID] {
				t.Errorf("Suggestion ID %d appeared on both page 1 and page 2", sg.ID)
			}
		}

		// Third page: should have 1 result and empty cursor
		rec3 := listSuggestionsRequest(t, listRouter, listUserToken,
			fmt.Sprintf("sort=new&limit=2&cursor=%s", page2.Cursor))
		if rec3.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec3.Code, rec3.Body.String())
		}

		var page3 listSuggestionsResponse
		if err := json.NewDecoder(rec3.Body).Decode(&page3); err != nil {
			t.Fatalf("Failed to decode page 3: %v", err)
		}

		if len(page3.Suggestions) != 1 {
			t.Fatalf("Expected 1 suggestion on page 3, got %d", len(page3.Suggestions))
		}

		// Cursor should be empty since there are no more results
		if page3.Cursor != "" {
			t.Errorf("Expected empty cursor on last page, got %q", page3.Cursor)
		}
	})

	// =====================================================================
	// Test: Vote on Suggestion
	// =====================================================================
	t.Run("Vote on suggestion", func(t *testing.T) {
		voteRouter, voteAuth := setupSuggestionTestRouter(t, []string{adminDID})
		voteUserToken := voteAuth.AddUser(userDID)
		voteUser2Token := voteAuth.AddUser(user2DID)

		// Create a suggestion
		created := mustCreateTestSuggestion(t, voteRouter, voteUserToken,
			"Vote Test Community", "Testing voting functionality")

		// Vote +1
		rec := voteOnSuggestionRequest(t, voteRouter, voteUser2Token, created.ID, 1)
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		// Verify vote_count incremented by getting the suggestion
		getRec := getSuggestionRequest(t, voteRouter, voteUser2Token, created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var getResp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
			t.Fatalf("Failed to decode get response: %v", err)
		}

		if getResp.VoteCount != 1 {
			t.Errorf("Expected voteCount 1, got %d", getResp.VoteCount)
		}

		// Verify viewer state shows vote = 1 for the voter
		if getResp.Viewer == nil {
			t.Fatal("Expected non-nil Viewer state for authenticated user")
		}
		if getResp.Viewer.Vote == nil {
			t.Fatal("Expected non-nil Viewer.Vote for user who voted")
		}
		if *getResp.Viewer.Vote != 1 {
			t.Errorf("Expected Viewer.Vote = 1, got %d", *getResp.Viewer.Vote)
		}
	})

	// =====================================================================
	// Test: Vote Toggle (Same Direction Removes)
	// =====================================================================
	t.Run("Vote toggle - same direction removes", func(t *testing.T) {
		toggleRouter, toggleAuth := setupSuggestionTestRouter(t, []string{adminDID})
		toggleUserToken := toggleAuth.AddUser(userDID)
		toggleUser2Token := toggleAuth.AddUser(user2DID)

		created := mustCreateTestSuggestion(t, toggleRouter, toggleUserToken,
			"Toggle Test", "Testing vote toggle")

		// Vote +1
		rec1 := voteOnSuggestionRequest(t, toggleRouter, toggleUser2Token, created.ID, 1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("First vote failed: %d: %s", rec1.Code, rec1.Body.String())
		}

		// Vote +1 again (should toggle off)
		rec2 := voteOnSuggestionRequest(t, toggleRouter, toggleUser2Token, created.ID, 1)
		if rec2.Code != http.StatusOK {
			t.Fatalf("Toggle vote failed: %d: %s", rec2.Code, rec2.Body.String())
		}

		// Verify vote_count is back to 0
		getRec := getSuggestionRequest(t, toggleRouter, toggleUser2Token, created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var getResp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
			t.Fatalf("Failed to decode get response: %v", err)
		}

		if getResp.VoteCount != 0 {
			t.Errorf("Expected voteCount 0 after toggle, got %d", getResp.VoteCount)
		}

		// Viewer state should NOT show a vote (vote was removed)
		if getResp.Viewer != nil && getResp.Viewer.Vote != nil {
			t.Errorf("Expected nil Viewer.Vote after toggle, got %d", *getResp.Viewer.Vote)
		}
	})

	// =====================================================================
	// Test: Vote Flip (Opposite Direction Changes)
	// =====================================================================
	t.Run("Vote flip - opposite direction changes", func(t *testing.T) {
		flipRouter, flipAuth := setupSuggestionTestRouter(t, []string{adminDID})
		flipUserToken := flipAuth.AddUser(userDID)
		flipUser2Token := flipAuth.AddUser(user2DID)

		created := mustCreateTestSuggestion(t, flipRouter, flipUserToken,
			"Flip Test", "Testing vote flip")

		// Vote +1
		rec1 := voteOnSuggestionRequest(t, flipRouter, flipUser2Token, created.ID, 1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("First vote failed: %d: %s", rec1.Code, rec1.Body.String())
		}

		// Vote -1 (should flip)
		rec2 := voteOnSuggestionRequest(t, flipRouter, flipUser2Token, created.ID, -1)
		if rec2.Code != http.StatusOK {
			t.Fatalf("Flip vote failed: %d: %s", rec2.Code, rec2.Body.String())
		}

		// Verify vote_count is -1
		getRec := getSuggestionRequest(t, flipRouter, flipUser2Token, created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var getResp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
			t.Fatalf("Failed to decode get response: %v", err)
		}

		if getResp.VoteCount != -1 {
			t.Errorf("Expected voteCount -1 after flip, got %d", getResp.VoteCount)
		}

		// Viewer state should show vote = -1
		if getResp.Viewer == nil || getResp.Viewer.Vote == nil {
			t.Fatal("Expected non-nil Viewer.Vote after flip")
		}
		if *getResp.Viewer.Vote != -1 {
			t.Errorf("Expected Viewer.Vote = -1, got %d", *getResp.Viewer.Vote)
		}
	})

	// =====================================================================
	// Test: Remove Vote Explicitly
	// =====================================================================
	t.Run("Remove vote explicitly", func(t *testing.T) {
		removeRouter, removeAuth := setupSuggestionTestRouter(t, []string{adminDID})
		removeUserToken := removeAuth.AddUser(userDID)
		removeUser2Token := removeAuth.AddUser(user2DID)

		created := mustCreateTestSuggestion(t, removeRouter, removeUserToken,
			"Remove Vote Test", "Testing explicit vote removal")

		// Vote +1
		rec1 := voteOnSuggestionRequest(t, removeRouter, removeUser2Token, created.ID, 1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("Vote failed: %d: %s", rec1.Code, rec1.Body.String())
		}

		// Verify vote_count is 1
		getRec := getSuggestionRequest(t, removeRouter, removeUser2Token, created.ID)
		var beforeResp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&beforeResp); err != nil {
			t.Fatalf("Failed to decode: %v", err)
		}
		if beforeResp.VoteCount != 1 {
			t.Fatalf("Expected voteCount 1 before removal, got %d", beforeResp.VoteCount)
		}

		// Explicitly remove vote
		removeRec := removeVoteRequest(t, removeRouter, removeUser2Token, created.ID)
		if removeRec.Code != http.StatusOK {
			t.Fatalf("Remove vote failed: %d: %s", removeRec.Code, removeRec.Body.String())
		}

		// Verify vote_count is back to 0
		getRec2 := getSuggestionRequest(t, removeRouter, removeUser2Token, created.ID)
		if getRec2.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec2.Code, getRec2.Body.String())
		}

		var afterResp suggestionResponse
		if err := json.NewDecoder(getRec2.Body).Decode(&afterResp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if afterResp.VoteCount != 0 {
			t.Errorf("Expected voteCount 0 after removal, got %d", afterResp.VoteCount)
		}
	})

	// =====================================================================
	// Test: Vote - Suggestion Not Found
	// =====================================================================
	t.Run("Vote - suggestion not found", func(t *testing.T) {
		rec := voteOnSuggestionRequest(t, router, userToken, 999999, 1)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("Expected 404, got %d: %s", rec.Code, rec.Body.String())
		}

		var errResp xrpcErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("Failed to decode error response: %v", err)
		}
		if errResp.Error != "NotFound" {
			t.Errorf("Expected error 'NotFound', got %q", errResp.Error)
		}
	})

	// =====================================================================
	// Test: Update Status - Admin
	// =====================================================================
	t.Run("Update status - admin", func(t *testing.T) {
		statusRouter, statusAuth := setupSuggestionTestRouter(t, []string{adminDID})
		statusUserToken := statusAuth.AddUser(userDID)
		statusAdminToken := statusAuth.AddUser(adminDID)

		created := mustCreateTestSuggestion(t, statusRouter, statusUserToken,
			"Status Update Test", "Testing admin status update")

		// Admin updates status to approved
		rec := updateStatusRequest(t, statusRouter, statusAdminToken, created.ID, "approved")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		// Verify status changed
		getRec := getSuggestionRequest(t, statusRouter, statusUserToken, created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var getResp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if getResp.Status != "approved" {
			t.Errorf("Expected status 'approved', got %q", getResp.Status)
		}
	})

	// =====================================================================
	// Test: Update Status - Non-Admin Forbidden
	// =====================================================================
	t.Run("Update status - non-admin forbidden", func(t *testing.T) {
		statusRouter, statusAuth := setupSuggestionTestRouter(t, []string{adminDID})
		statusUserToken := statusAuth.AddUser(userDID)

		created := mustCreateTestSuggestion(t, statusRouter, statusUserToken,
			"Forbidden Status Test", "Non-admin should not update")

		// Non-admin tries to update status
		rec := updateStatusRequest(t, statusRouter, statusUserToken, created.ID, "approved")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("Expected 403, got %d: %s", rec.Code, rec.Body.String())
		}

		var errResp xrpcErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("Failed to decode error response: %v", err)
		}
		if errResp.Error != "Forbidden" {
			t.Errorf("Expected error 'Forbidden', got %q", errResp.Error)
		}
	})

	// =====================================================================
	// Test: Rate Limiting (3 suggestions per day per DID)
	// =====================================================================
	t.Run("Rate limiting - max suggestions per day", func(t *testing.T) {
		rlRouter, rlAuth := setupSuggestionTestRouter(t, []string{adminDID})
		rlUserToken := rlAuth.AddUser(user3DID)

		// Create MaxSuggestionsPerDay suggestions (should succeed)
		for i := 0; i < communitysuggestions.MaxSuggestionsPerDay; i++ {
			rec := createTestSuggestionRequest(t, rlRouter, rlUserToken,
				fmt.Sprintf("Rate Limit Test %d", i),
				fmt.Sprintf("Description %d", i))
			if rec.Code != http.StatusOK {
				t.Fatalf("Expected 200 for suggestion %d, got %d: %s", i, rec.Code, rec.Body.String())
			}
		}

		// Try to create one more (should fail with 429)
		rec := createTestSuggestionRequest(t, rlRouter, rlUserToken,
			"Over Limit", "Should be rate limited")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("Expected 429, got %d: %s", rec.Code, rec.Body.String())
		}

		var errResp xrpcErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
			t.Fatalf("Failed to decode error response: %v", err)
		}
		if errResp.Error != "RateLimitExceeded" {
			t.Errorf("Expected error 'RateLimitExceeded', got %q", errResp.Error)
		}
	})

	// =====================================================================
	// Test: List with Viewer State - Authenticated
	// =====================================================================
	t.Run("List suggestions - viewer state populated for authenticated user", func(t *testing.T) {
		vsRouter, vsAuth := setupSuggestionTestRouter(t, []string{adminDID})
		vsUserToken := vsAuth.AddUser(userDID)
		vsUser2Token := vsAuth.AddUser(user2DID)

		// Create two suggestions
		s1 := mustCreateTestSuggestion(t, vsRouter, vsUserToken,
			"Viewer State Test 1", "User will vote on this")
		_ = mustCreateTestSuggestion(t, vsRouter, vsUserToken,
			"Viewer State Test 2", "User will not vote on this")

		// User2 votes on s1
		voteRec := voteOnSuggestionRequest(t, vsRouter, vsUser2Token, s1.ID, 1)
		if voteRec.Code != http.StatusOK {
			t.Fatalf("Vote failed: %d: %s", voteRec.Code, voteRec.Body.String())
		}

		// List as user2 (should see voter state)
		rec := listSuggestionsRequest(t, vsRouter, vsUser2Token, "sort=new")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp listSuggestionsResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		// Find s1 in the list and verify viewer state
		var foundS1 bool
		for _, sg := range resp.Suggestions {
			if sg.ID == s1.ID {
				foundS1 = true
				if sg.Viewer == nil || sg.Viewer.Vote == nil {
					t.Error("Expected non-nil Viewer.Vote for voted suggestion in list")
				} else if *sg.Viewer.Vote != 1 {
					t.Errorf("Expected Viewer.Vote = 1, got %d", *sg.Viewer.Vote)
				}
			}
		}
		if !foundS1 {
			t.Error("Expected to find s1 in list response")
		}
	})

	// =====================================================================
	// Test: List without Auth - No Viewer State
	// =====================================================================
	t.Run("List suggestions - no viewer state for unauthenticated", func(t *testing.T) {
		noAuthRouter, noAuthAuth := setupSuggestionTestRouter(t, []string{adminDID})
		noAuthUserToken := noAuthAuth.AddUser(userDID)

		mustCreateTestSuggestion(t, noAuthRouter, noAuthUserToken,
			"No Auth Viewer Test", "No viewer state expected")

		// List without auth token
		rec := listSuggestionsRequest(t, noAuthRouter, "", "sort=new")
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp listSuggestionsResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		for _, sg := range resp.Suggestions {
			if sg.Viewer != nil {
				t.Errorf("Expected nil Viewer for unauthenticated request, got non-nil for ID %d", sg.ID)
			}
		}
	})

	// =====================================================================
	// Test: Update Status - All Valid Statuses
	// =====================================================================
	t.Run("Update status - all valid transitions", func(t *testing.T) {
		allStatusRouter, allStatusAuth := setupSuggestionTestRouter(t, []string{adminDID})
		allStatusUserToken := allStatusAuth.AddUser(userDID)
		allStatusAdminToken := allStatusAuth.AddUser(adminDID)

		validStatuses := []string{"under_review", "approved", "declined", "open"}

		for _, status := range validStatuses {
			// Use a distinct submitter per suggestion to stay under the per-user
			// daily rate limit (MaxSuggestionsPerDay); status transitions are
			// independent of who submitted the suggestion.
			submitterToken := allStatusAuth.AddUser(fmt.Sprintf("did:plc:statususer-%s", status))
			created := mustCreateTestSuggestion(t, allStatusRouter, submitterToken,
				fmt.Sprintf("Status %s Test", status),
				fmt.Sprintf("Testing transition to %s", status))

			rec := updateStatusRequest(t, allStatusRouter, allStatusAdminToken, created.ID, status)
			if rec.Code != http.StatusOK {
				t.Errorf("Expected 200 for status %q, got %d: %s", status, rec.Code, rec.Body.String())
			}

			// Verify
			getRec := getSuggestionRequest(t, allStatusRouter, allStatusUserToken, created.ID)
			var getResp suggestionResponse
			if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}
			if getResp.Status != status {
				t.Errorf("Expected status %q, got %q", status, getResp.Status)
			}
		}
	})

	// =====================================================================
	// Test: Multiple Users Voting
	// =====================================================================
	t.Run("Multiple users voting on same suggestion", func(t *testing.T) {
		multiRouter, multiAuth := setupSuggestionTestRouter(t, []string{adminDID})
		multiUserToken := multiAuth.AddUser(userDID)
		multiUser2Token := multiAuth.AddUser(user2DID)
		multiUser3Token := multiAuth.AddUser(user3DID)

		created := mustCreateTestSuggestion(t, multiRouter, multiUserToken,
			"Multi Vote Test", "Multiple users vote here")

		// User 1 votes +1
		rec1 := voteOnSuggestionRequest(t, multiRouter, multiUserToken, created.ID, 1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("User1 vote failed: %d: %s", rec1.Code, rec1.Body.String())
		}

		// User 2 votes +1
		rec2 := voteOnSuggestionRequest(t, multiRouter, multiUser2Token, created.ID, 1)
		if rec2.Code != http.StatusOK {
			t.Fatalf("User2 vote failed: %d: %s", rec2.Code, rec2.Body.String())
		}

		// User 3 votes -1
		rec3 := voteOnSuggestionRequest(t, multiRouter, multiUser3Token, created.ID, -1)
		if rec3.Code != http.StatusOK {
			t.Fatalf("User3 vote failed: %d: %s", rec3.Code, rec3.Body.String())
		}

		// Verify total: +1 +1 -1 = +1
		getRec := getSuggestionRequest(t, multiRouter, multiUserToken, created.ID)
		var getResp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if getResp.VoteCount != 1 {
			t.Errorf("Expected voteCount 1 (from +1 +1 -1), got %d", getResp.VoteCount)
		}

		// Verify viewer state for user1 (voted +1)
		if getResp.Viewer == nil || getResp.Viewer.Vote == nil {
			t.Fatal("Expected non-nil Viewer.Vote for user1")
		}
		if *getResp.Viewer.Vote != 1 {
			t.Errorf("Expected Viewer.Vote = 1 for user1, got %d", *getResp.Viewer.Vote)
		}
	})

	// =====================================================================
	// Test: Vote Auth Required
	// =====================================================================
	t.Run("Vote - auth required", func(t *testing.T) {
		// Try to vote without auth
		rec := voteOnSuggestionRequest(t, router, "", 1, 1)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for unauthenticated vote, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// =====================================================================
	// Test: Remove Vote Auth Required
	// =====================================================================
	t.Run("Remove vote - auth required", func(t *testing.T) {
		rec := removeVoteRequest(t, router, "", 1)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for unauthenticated remove vote, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// =====================================================================
	// Test: Update Status Auth Required
	// =====================================================================
	t.Run("Update status - auth required", func(t *testing.T) {
		rec := updateStatusRequest(t, router, "", 1, "approved")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for unauthenticated status update, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestCommunitySuggestionE2E_ViewerStateOnGet tests that the get endpoint properly
// populates viewer state for authenticated users.
func TestCommunitySuggestionE2E_ViewerStateOnGet(t *testing.T) {
	adminDID := "did:plc:testadmin"
	userDID := "did:plc:vieweruser1"
	user2DID := "did:plc:vieweruser2"

	router, e2eAuth := setupSuggestionTestRouter(t, []string{adminDID})
	userToken := e2eAuth.AddUser(userDID)
	user2Token := e2eAuth.AddUser(user2DID)

	// Create a suggestion
	created := mustCreateTestSuggestion(t, router, userToken,
		"Viewer Get Test", "Testing viewer state on get")

	// User2 votes +1
	voteRec := voteOnSuggestionRequest(t, router, user2Token, created.ID, 1)
	if voteRec.Code != http.StatusOK {
		t.Fatalf("Vote failed: %d: %s", voteRec.Code, voteRec.Body.String())
	}

	t.Run("Voter sees their vote in viewer state", func(t *testing.T) {
		getRec := getSuggestionRequest(t, router, user2Token, created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var resp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode: %v", err)
		}

		if resp.Viewer == nil || resp.Viewer.Vote == nil {
			t.Fatal("Expected non-nil Viewer.Vote for voter")
		}
		if *resp.Viewer.Vote != 1 {
			t.Errorf("Expected Viewer.Vote = 1, got %d", *resp.Viewer.Vote)
		}
	})

	t.Run("Non-voter sees no vote in viewer state", func(t *testing.T) {
		// User1 did NOT vote, should see nil viewer.vote
		getRec := getSuggestionRequest(t, router, userToken, created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var resp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode: %v", err)
		}

		// Viewer state should be nil since user1 didn't vote
		if resp.Viewer != nil && resp.Viewer.Vote != nil {
			t.Errorf("Expected nil Viewer.Vote for non-voter, got %d", *resp.Viewer.Vote)
		}
	})

	t.Run("Unauthenticated user sees no viewer state", func(t *testing.T) {
		getRec := getSuggestionRequest(t, router, "", created.ID)
		if getRec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var resp suggestionResponse
		if err := json.NewDecoder(getRec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode: %v", err)
		}

		if resp.Viewer != nil {
			t.Error("Expected nil Viewer for unauthenticated request")
		}
	})
}

// TestCommunitySuggestionE2E_DownvoteFlow tests the full downvote lifecycle:
// downvote, toggle off, then upvote.
func TestCommunitySuggestionE2E_DownvoteFlow(t *testing.T) {
	adminDID := "did:plc:testadmin"
	userDID := "did:plc:downvoteuser1"
	voterDID := "did:plc:downvotevoter1"

	router, e2eAuth := setupSuggestionTestRouter(t, []string{adminDID})
	userToken := e2eAuth.AddUser(userDID)
	voterToken := e2eAuth.AddUser(voterDID)

	created := mustCreateTestSuggestion(t, router, userToken,
		"Downvote Flow Test", "Testing downvote lifecycle")

	// Step 1: Downvote (-1)
	rec1 := voteOnSuggestionRequest(t, router, voterToken, created.ID, -1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("Downvote failed: %d: %s", rec1.Code, rec1.Body.String())
	}

	getRec1 := getSuggestionRequest(t, router, voterToken, created.ID)
	var resp1 suggestionResponse
	if err := json.NewDecoder(getRec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("Failed to decode: %v", err)
	}
	if resp1.VoteCount != -1 {
		t.Errorf("Step 1: Expected voteCount -1, got %d", resp1.VoteCount)
	}
	if resp1.Viewer == nil || resp1.Viewer.Vote == nil || *resp1.Viewer.Vote != -1 {
		t.Error("Step 1: Expected Viewer.Vote = -1")
	}

	// Step 2: Downvote again (toggle off)
	rec2 := voteOnSuggestionRequest(t, router, voterToken, created.ID, -1)
	if rec2.Code != http.StatusOK {
		t.Fatalf("Toggle downvote failed: %d: %s", rec2.Code, rec2.Body.String())
	}

	getRec2 := getSuggestionRequest(t, router, voterToken, created.ID)
	var resp2 suggestionResponse
	if err := json.NewDecoder(getRec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("Failed to decode: %v", err)
	}
	if resp2.VoteCount != 0 {
		t.Errorf("Step 2: Expected voteCount 0, got %d", resp2.VoteCount)
	}

	// Step 3: Upvote (+1) after removing downvote
	rec3 := voteOnSuggestionRequest(t, router, voterToken, created.ID, 1)
	if rec3.Code != http.StatusOK {
		t.Fatalf("Upvote failed: %d: %s", rec3.Code, rec3.Body.String())
	}

	getRec3 := getSuggestionRequest(t, router, voterToken, created.ID)
	var resp3 suggestionResponse
	if err := json.NewDecoder(getRec3.Body).Decode(&resp3); err != nil {
		t.Fatalf("Failed to decode: %v", err)
	}
	if resp3.VoteCount != 1 {
		t.Errorf("Step 3: Expected voteCount 1, got %d", resp3.VoteCount)
	}
	if resp3.Viewer == nil || resp3.Viewer.Vote == nil || *resp3.Viewer.Vote != 1 {
		t.Error("Step 3: Expected Viewer.Vote = 1")
	}
}

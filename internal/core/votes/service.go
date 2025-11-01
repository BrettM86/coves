package votes

import (
	"Coves/internal/core/posts"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type voteService struct {
	repo     Repository
	postRepo posts.Repository
	pdsURL   string
}

// NewVoteService creates a new vote service
func NewVoteService(
	repo Repository,
	postRepo posts.Repository,
	pdsURL string,
) Service {
	return &voteService{
		repo:     repo,
		postRepo: postRepo,
		pdsURL:   pdsURL,
	}
}

// CreateVote creates a new vote or toggles an existing vote
// Toggle logic:
//   - No vote -> Create vote
//   - Same direction -> Delete vote (toggle off)
//   - Different direction -> Delete old + Create new (toggle direction)
func (s *voteService) CreateVote(ctx context.Context, voterDID string, userAccessToken string, req CreateVoteRequest) (*CreateVoteResponse, error) {
	// 1. Validate input
	if voterDID == "" {
		return nil, NewValidationError("voterDid", "required")
	}
	if userAccessToken == "" {
		return nil, NewValidationError("userAccessToken", "required")
	}
	if req.Subject == "" {
		return nil, NewValidationError("subject", "required")
	}
	if req.Direction != "up" && req.Direction != "down" {
		return nil, ErrInvalidDirection
	}

	// 2. Validate subject URI format (should be at://...)
	if !strings.HasPrefix(req.Subject, "at://") {
		return nil, ErrInvalidSubject
	}

	// 3. Get subject post/comment to verify it exists and get its CID (for strong reference)
	// For now, we assume the subject is a post. In the future, we'll support comments too.
	post, err := s.postRepo.GetByURI(ctx, req.Subject)
	if err != nil {
		if err == posts.ErrNotFound {
			return nil, ErrSubjectNotFound
		}
		return nil, fmt.Errorf("failed to get subject post: %w", err)
	}

	// 4. Check for existing vote on PDS (source of truth for toggle logic)
	// IMPORTANT: We query the user's PDS directly instead of AppView to avoid race conditions.
	// AppView is eventually consistent (updated via Jetstream), so querying it can cause
	// duplicate vote records if the user toggles before Jetstream catches up.
	existingVoteRecord, err := s.findVoteOnPDS(ctx, voterDID, userAccessToken, req.Subject)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing vote on PDS: %w", err)
	}

	// 5. Handle toggle logic
	var existingVoteURI *string

	if existingVoteRecord != nil {
		// Vote exists on PDS - implement toggle logic
		if existingVoteRecord.Direction == req.Direction {
			// Same direction -> Delete vote (toggle off)
			log.Printf("[VOTE-CREATE] Toggle off: deleting existing %s vote on %s", req.Direction, req.Subject)

			// Delete from user's PDS
			if err := s.deleteRecordOnPDSAs(ctx, voterDID, "social.coves.interaction.vote", existingVoteRecord.RKey, userAccessToken); err != nil {
				return nil, fmt.Errorf("failed to delete vote on PDS: %w", err)
			}

			// Return empty response (vote was deleted, not created)
			return &CreateVoteResponse{
				URI: "",
				CID: "",
			}, nil
		}

		// Different direction -> Delete old vote first, then create new one below
		log.Printf("[VOTE-CREATE] Toggle direction: %s -> %s on %s", existingVoteRecord.Direction, req.Direction, req.Subject)

		if err := s.deleteRecordOnPDSAs(ctx, voterDID, "social.coves.interaction.vote", existingVoteRecord.RKey, userAccessToken); err != nil {
			return nil, fmt.Errorf("failed to delete old vote on PDS: %w", err)
		}

		existingVoteURI = &existingVoteRecord.URI
	}

	// 6. Build vote record with strong reference
	voteRecord := map[string]interface{}{
		"$type": "social.coves.interaction.vote",
		"subject": map[string]interface{}{
			"uri": req.Subject,
			"cid": post.CID,
		},
		"direction": req.Direction,
		"createdAt": time.Now().Format(time.RFC3339),
	}

	// 7. Write to user's PDS repository
	recordURI, recordCID, err := s.createRecordOnPDSAs(ctx, voterDID, "social.coves.interaction.vote", "", voteRecord, userAccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create vote on PDS: %w", err)
	}

	log.Printf("[VOTE-CREATE] Created %s vote: %s (CID: %s)", req.Direction, recordURI, recordCID)

	// 8. Return response
	return &CreateVoteResponse{
		URI:      recordURI,
		CID:      recordCID,
		Existing: existingVoteURI,
	}, nil
}

// DeleteVote removes a vote from a post/comment
func (s *voteService) DeleteVote(ctx context.Context, voterDID string, userAccessToken string, req DeleteVoteRequest) error {
	// 1. Validate input
	if voterDID == "" {
		return NewValidationError("voterDid", "required")
	}
	if userAccessToken == "" {
		return NewValidationError("userAccessToken", "required")
	}
	if req.Subject == "" {
		return NewValidationError("subject", "required")
	}

	// 2. Find existing vote on PDS (source of truth)
	// IMPORTANT: Query PDS directly to avoid race conditions with AppView indexing
	existingVoteRecord, err := s.findVoteOnPDS(ctx, voterDID, userAccessToken, req.Subject)
	if err != nil {
		return fmt.Errorf("failed to check existing vote on PDS: %w", err)
	}

	if existingVoteRecord == nil {
		return ErrVoteNotFound
	}

	// 3. Delete from user's PDS
	if err := s.deleteRecordOnPDSAs(ctx, voterDID, "social.coves.interaction.vote", existingVoteRecord.RKey, userAccessToken); err != nil {
		return fmt.Errorf("failed to delete vote on PDS: %w", err)
	}

	log.Printf("[VOTE-DELETE] Deleted vote: %s", existingVoteRecord.URI)

	return nil
}

// GetVote retrieves a user's vote on a specific subject
func (s *voteService) GetVote(ctx context.Context, voterDID string, subjectURI string) (*Vote, error) {
	return s.repo.GetByVoterAndSubject(ctx, voterDID, subjectURI)
}

// Helper methods for PDS operations

// createRecordOnPDSAs creates a record on the PDS using the user's access token
func (s *voteService) createRecordOnPDSAs(ctx context.Context, repoDID, collection, rkey string, record map[string]interface{}, accessToken string) (string, string, error) {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"record":     record,
	}

	if rkey != "" {
		payload["rkey"] = rkey
	}

	return s.callPDSWithAuth(ctx, "POST", endpoint, payload, accessToken)
}

// deleteRecordOnPDSAs deletes a record from the PDS using the user's access token
func (s *voteService) deleteRecordOnPDSAs(ctx context.Context, repoDID, collection, rkey, accessToken string) error {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.deleteRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"rkey":       rkey,
	}

	_, _, err := s.callPDSWithAuth(ctx, "POST", endpoint, payload, accessToken)
	return err
}

// callPDSWithAuth makes a PDS call with a specific access token
func (s *voteService) callPDSWithAuth(ctx context.Context, method, endpoint string, payload map[string]interface{}, accessToken string) (string, string, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add authentication with provided access token
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	// Use 30 second timeout for write operations
	timeout := 30 * time.Second
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to call PDS: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("PDS returned error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response to extract URI and CID
	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("failed to parse PDS response: %w", err)
	}

	return result.URI, result.CID, nil
}

// Helper functions

// PDSVoteRecord represents a vote record returned from PDS listRecords
type PDSVoteRecord struct {
	URI       string
	RKey      string
	Direction string
	Subject   struct {
		URI string
		CID string
	}
}

// findVoteOnPDS queries the user's PDS to find an existing vote on a specific subject
// This is the source of truth for toggle logic (avoiding AppView race conditions)
//
// IMPORTANT: This function paginates through ALL user votes with reverse=true (newest first)
// to handle users with >100 votes. Without pagination, votes on older posts would not be found,
// causing duplicate vote records and 404 errors on delete operations.
func (s *voteService) findVoteOnPDS(ctx context.Context, voterDID, accessToken, subjectURI string) (*PDSVoteRecord, error) {
	const maxPages = 50 // Safety limit: prevent infinite loops (50 pages * 100 = 5000 votes max)
	var cursor string
	pageCount := 0

	client := &http.Client{Timeout: 10 * time.Second}

	for {
		pageCount++
		if pageCount > maxPages {
			log.Printf("[VOTE-PDS] Reached max pagination limit (%d pages) searching for vote on %s", maxPages, subjectURI)
			break
		}

		// Build endpoint with pagination cursor and reverse=true (newest first)
		endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=social.coves.interaction.vote&limit=100&reverse=true",
			strings.TrimSuffix(s.pdsURL, "/"), voterDID)

		if cursor != "" {
			endpoint += fmt.Sprintf("&cursor=%s", cursor)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to query PDS: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("PDS returned error %d: %s", resp.StatusCode, string(body))
		}

		var result struct {
			Records []struct {
				URI   string `json:"uri"`
				Value struct {
					Subject struct {
						URI string `json:"uri"`
						CID string `json:"cid"`
					} `json:"subject"`
					Direction string `json:"direction"`
				} `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor,omitempty"` // Pagination cursor for next page
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to decode PDS response: %w", err)
		}
		resp.Body.Close()

		// Find vote on this specific subject in current page
		for _, record := range result.Records {
			if record.Value.Subject.URI == subjectURI {
				rkey := extractRKeyFromURI(record.URI)
				log.Printf("[VOTE-PDS] Found existing vote on page %d: %s (direction: %s)", pageCount, record.URI, record.Value.Direction)
				return &PDSVoteRecord{
					URI:       record.URI,
					RKey:      rkey,
					Direction: record.Value.Direction,
					Subject: struct {
						URI string
						CID string
					}{
						URI: record.Value.Subject.URI,
						CID: record.Value.Subject.CID,
					},
				}, nil
			}
		}

		// No more pages to check
		if result.Cursor == "" {
			log.Printf("[VOTE-PDS] No existing vote found after checking %d page(s)", pageCount)
			break
		}

		// Move to next page
		cursor = result.Cursor
	}

	// No vote found on this subject after paginating through all records
	return nil, nil
}

// extractRKeyFromURI extracts the rkey from an AT-URI (at://did/collection/rkey)
func extractRKeyFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[len(parts)-1]
	}
	return ""
}

// ValidationError represents a validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error for field '%s': %s", e.Field, e.Message)
}

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return &ValidationError{
		Field:   field,
		Message: message,
	}
}

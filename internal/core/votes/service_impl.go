package votes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	oauthclient "Coves/internal/atproto/oauth"
)

// voteService implements the Service interface for vote operations
type voteService struct {
	repo             Repository
	subjectValidator SubjectValidator
	oauthClient      *oauthclient.OAuthClient
	oauthStore       oauth.ClientAuthStore
	logger           *slog.Logger
}

// NewService creates a new vote service instance
// subjectValidator can be nil to skip subject existence checks (not recommended for production)
func NewService(repo Repository, subjectValidator SubjectValidator, oauthClient *oauthclient.OAuthClient, oauthStore oauth.ClientAuthStore, logger *slog.Logger) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:             repo,
		subjectValidator: subjectValidator,
		oauthClient:      oauthClient,
		oauthStore:       oauthStore,
		logger:           logger,
	}
}

// CreateVote creates a new vote or toggles off an existing vote
// Implements the toggle behavior:
// - No existing vote → Create new vote with given direction
// - Vote exists with same direction → Delete vote (toggle off)
// - Vote exists with different direction → Update to new direction
func (s *voteService) CreateVote(ctx context.Context, session *oauth.ClientSessionData, req CreateVoteRequest) (*CreateVoteResponse, error) {
	// Validate direction
	if req.Direction != "up" && req.Direction != "down" {
		return nil, ErrInvalidDirection
	}

	// Validate subject URI format
	if req.Subject.URI == "" {
		return nil, ErrInvalidSubject
	}
	if !strings.HasPrefix(req.Subject.URI, "at://") {
		return nil, ErrInvalidSubject
	}

	// Validate subject CID is provided
	if req.Subject.CID == "" {
		return nil, ErrInvalidSubject
	}

	// Validate subject exists in AppView (post or comment)
	// This prevents creating votes on non-existent content
	if s.subjectValidator != nil {
		exists, err := s.subjectValidator.SubjectExists(ctx, req.Subject.URI)
		if err != nil {
			s.logger.Error("failed to validate subject existence",
				"error", err,
				"subject", req.Subject.URI)
			return nil, fmt.Errorf("failed to validate subject: %w", err)
		}
		if !exists {
			return nil, ErrSubjectNotFound
		}
	}

	// Check for existing vote by querying PDS directly (source of truth)
	// This avoids eventual consistency issues with the AppView database
	existing, err := s.getVoteFromPDS(ctx, session, req.Subject.URI)
	if err != nil {
		s.logger.Error("failed to check existing vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI)
		return nil, fmt.Errorf("failed to check existing vote: %w", err)
	}

	// Toggle logic
	if existing != nil {
		// Vote exists - check if same direction
		if existing.Direction == req.Direction {
			// Same direction - toggle off (delete)
			if err := s.deleteVoteRecord(ctx, session, existing.RKey); err != nil {
				s.logger.Error("failed to delete vote on PDS",
					"error", err,
					"voter", session.AccountDID,
					"rkey", existing.RKey)
				return nil, fmt.Errorf("failed to delete vote: %w", err)
			}

			s.logger.Info("vote toggled off",
				"voter", session.AccountDID,
				"subject", req.Subject.URI,
				"direction", req.Direction)

			// Return empty response to indicate deletion
			return &CreateVoteResponse{
				URI: "",
				CID: "",
			}, nil
		}

		// Different direction - delete old vote first, then create new one
		if err := s.deleteVoteRecord(ctx, session, existing.RKey); err != nil {
			s.logger.Error("failed to delete existing vote on PDS",
				"error", err,
				"voter", session.AccountDID,
				"rkey", existing.RKey)
			return nil, fmt.Errorf("failed to delete existing vote: %w", err)
		}

		s.logger.Info("deleted existing vote before creating new direction",
			"voter", session.AccountDID,
			"subject", req.Subject.URI,
			"old_direction", existing.Direction,
			"new_direction", req.Direction)
	}

	// Create new vote
	uri, cid, err := s.createVoteRecord(ctx, session, req)
	if err != nil {
		s.logger.Error("failed to create vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI,
			"direction", req.Direction)
		return nil, fmt.Errorf("failed to create vote: %w", err)
	}

	s.logger.Info("vote created",
		"voter", session.AccountDID,
		"subject", req.Subject.URI,
		"direction", req.Direction,
		"uri", uri,
		"cid", cid)

	return &CreateVoteResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// DeleteVote removes a vote on the specified subject
func (s *voteService) DeleteVote(ctx context.Context, session *oauth.ClientSessionData, req DeleteVoteRequest) error {
	// Validate subject URI format
	if req.Subject.URI == "" {
		return ErrInvalidSubject
	}
	if !strings.HasPrefix(req.Subject.URI, "at://") {
		return ErrInvalidSubject
	}

	// Find existing vote by querying PDS directly (source of truth)
	// This avoids eventual consistency issues with the AppView database
	existing, err := s.getVoteFromPDS(ctx, session, req.Subject.URI)
	if err != nil {
		s.logger.Error("failed to find vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI)
		return fmt.Errorf("failed to find vote: %w", err)
	}
	if existing == nil {
		return ErrVoteNotFound
	}

	// Delete the vote record from user's PDS
	if err := s.deleteVoteRecord(ctx, session, existing.RKey); err != nil {
		s.logger.Error("failed to delete vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"rkey", existing.RKey)
		return fmt.Errorf("failed to delete vote: %w", err)
	}

	s.logger.Info("vote deleted",
		"voter", session.AccountDID,
		"subject", req.Subject.URI,
		"uri", existing.URI)

	return nil
}

// createVoteRecord writes a vote record to the user's PDS
func (s *voteService) createVoteRecord(ctx context.Context, session *oauth.ClientSessionData, req CreateVoteRequest) (string, string, error) {
	// Generate TID for the record key
	tid := syntax.NewTIDNow(0)

	// Build vote record following the lexicon schema
	record := VoteRecord{
		Type: "social.coves.feed.vote",
		Subject: StrongRef{
			URI: req.Subject.URI,
			CID: req.Subject.CID,
		},
		Direction: req.Direction,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Call com.atproto.repo.createRecord on the user's PDS
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", strings.TrimSuffix(session.HostURL, "/"))

	payload := map[string]interface{}{
		"repo":       session.AccountDID.String(),
		"collection": "social.coves.feed.vote",
		"rkey":       tid.String(),
		"record":     record,
	}

	uri, cid, err := s.callPDSWithAuth(ctx, "POST", endpoint, payload, session.AccessToken)
	if err != nil {
		return "", "", err
	}

	return uri, cid, nil
}

// getVoteFromPDS queries the user's PDS directly to find an existing vote for a subject.
// This avoids eventual consistency issues with the AppView database populated by Jetstream.
// Paginates through all vote records to handle users with >100 votes.
// Returns the vote record with rkey, or nil if no vote exists for the subject.
func (s *voteService) getVoteFromPDS(ctx context.Context, session *oauth.ClientSessionData, subjectURI string) (*existingVote, error) {
	baseURL := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=social.coves.feed.vote&limit=100",
		strings.TrimSuffix(session.HostURL, "/"),
		session.AccountDID.String())

	client := &http.Client{Timeout: 10 * time.Second}
	cursor := ""

	// Paginate through all vote records
	for {
		endpoint := baseURL
		if cursor != "" {
			endpoint = fmt.Sprintf("%s&cursor=%s", baseURL, cursor)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+session.AccessToken)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to call PDS: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if closeErr != nil {
			s.logger.Warn("failed to close response body", "error", closeErr)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		// Handle auth errors - map to ErrNotAuthorized per lexicon
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			s.logger.Warn("PDS auth failure",
				"status", resp.StatusCode,
				"did", session.AccountDID)
			return nil, ErrNotAuthorized
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
		}

		// Parse the listRecords response
		var result struct {
			Records []struct {
				URI   string `json:"uri"`
				CID   string `json:"cid"`
				Value struct {
					Type    string `json:"$type"`
					Subject struct {
						URI string `json:"uri"`
						CID string `json:"cid"`
					} `json:"subject"`
					Direction string `json:"direction"`
					CreatedAt string `json:"createdAt"`
				} `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("failed to parse PDS response: %w", err)
		}

		// Search for the vote matching our subject in this page
		for _, rec := range result.Records {
			if rec.Value.Subject.URI == subjectURI {
				// Extract rkey from the URI (at://did/collection/rkey)
				parts := strings.Split(rec.URI, "/")
				if len(parts) < 5 {
					continue
				}
				rkey := parts[len(parts)-1]

				return &existingVote{
					URI:       rec.URI,
					CID:       rec.CID,
					RKey:      rkey,
					Direction: rec.Value.Direction,
				}, nil
			}
		}

		// Check if there are more pages
		if result.Cursor == "" {
			break // No more pages
		}
		cursor = result.Cursor
	}

	// No vote found for this subject after checking all pages
	return nil, nil
}

// existingVote represents a vote record found on the PDS
type existingVote struct {
	URI       string
	CID       string
	RKey      string
	Direction string
}

// deleteVoteRecord removes a vote record from the user's PDS
func (s *voteService) deleteVoteRecord(ctx context.Context, session *oauth.ClientSessionData, rkey string) error {
	// Call com.atproto.repo.deleteRecord on the user's PDS
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.deleteRecord", strings.TrimSuffix(session.HostURL, "/"))

	payload := map[string]interface{}{
		"repo":       session.AccountDID.String(),
		"collection": "social.coves.feed.vote",
		"rkey":       rkey,
	}

	_, _, err := s.callPDSWithAuth(ctx, "POST", endpoint, payload, session.AccessToken)
	return err
}

// callPDSWithAuth makes an authenticated HTTP call to the PDS
// Returns URI and CID from the response (for create/update operations)
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

	// Add OAuth bearer token for authentication
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	// Set reasonable timeout for PDS operations
	timeout := 10 * time.Second
	if strings.Contains(endpoint, "createRecord") || strings.Contains(endpoint, "putRecord") {
		timeout = 15 * time.Second // Slightly longer for write operations
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to call PDS: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Warn("failed to close response body", "error", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response: %w", err)
	}

	// Handle auth errors - map to ErrNotAuthorized per lexicon
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		s.logger.Warn("PDS auth failure during write operation",
			"status", resp.StatusCode,
			"endpoint", endpoint)
		return "", "", ErrNotAuthorized
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response to extract URI and CID (for create/update operations)
	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// For delete operations, there might not be a response body with URI/CID
		if method == "POST" && strings.Contains(endpoint, "deleteRecord") {
			return "", "", nil
		}
		return "", "", fmt.Errorf("failed to parse PDS response: %w", err)
	}

	return result.URI, result.CID, nil
}

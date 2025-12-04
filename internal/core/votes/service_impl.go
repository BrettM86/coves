package votes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	oauthclient "Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
)

const (
	// voteCollection is the AT Protocol collection for vote records
	voteCollection = "social.coves.feed.vote"
)

// PDSClientFactory creates PDS clients from session data.
// Used to allow injection of different auth mechanisms (OAuth for production, password for tests).
type PDSClientFactory func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error)

// voteService implements the Service interface for vote operations
type voteService struct {
	repo             Repository
	oauthClient      *oauthclient.OAuthClient
	oauthStore       oauth.ClientAuthStore
	logger           *slog.Logger
	pdsClientFactory PDSClientFactory // Optional, for testing. If nil, uses OAuth.
}

// NewService creates a new vote service instance
func NewService(repo Repository, oauthClient *oauthclient.OAuthClient, oauthStore oauth.ClientAuthStore, logger *slog.Logger) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:        repo,
		oauthClient: oauthClient,
		oauthStore:  oauthStore,
		logger:      logger,
	}
}

// NewServiceWithPDSFactory creates a vote service with a custom PDS client factory.
// This is primarily for testing with password-based authentication.
func NewServiceWithPDSFactory(repo Repository, logger *slog.Logger, factory PDSClientFactory) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:             repo,
		logger:           logger,
		pdsClientFactory: factory,
	}
}

// getPDSClient creates a PDS client from an OAuth session.
// If a custom factory was provided (for testing), uses that.
// Otherwise, uses DPoP authentication via indigo's APIClient for proper OAuth token handling.
func (s *voteService) getPDSClient(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
	// Use custom factory if provided (e.g., for testing with password auth)
	if s.pdsClientFactory != nil {
		return s.pdsClientFactory(ctx, session)
	}

	// Production path: use OAuth with DPoP
	if s.oauthClient == nil || s.oauthClient.ClientApp == nil {
		return nil, fmt.Errorf("OAuth client not configured")
	}

	client, err := pds.NewFromOAuthSession(ctx, s.oauthClient.ClientApp, session)
	if err != nil {
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	return client, nil
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

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"voter", session.AccountDID)
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Note: We intentionally don't validate subject existence here.
	// The vote record goes to the user's PDS regardless. The Jetstream consumer
	// handles orphaned votes correctly by only updating counts for non-deleted subjects.
	// This avoids race conditions and eventual consistency issues.

	// Check for existing vote by querying PDS directly (source of truth)
	// This avoids eventual consistency issues with the AppView database
	existing, err := s.findExistingVote(ctx, pdsClient, req.Subject.URI)
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
			if err := pdsClient.DeleteRecord(ctx, voteCollection, existing.RKey); err != nil {
				s.logger.Error("failed to delete vote on PDS",
					"error", err,
					"voter", session.AccountDID,
					"rkey", existing.RKey)
				if pds.IsAuthError(err) {
					return nil, ErrNotAuthorized
				}
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
		if err := pdsClient.DeleteRecord(ctx, voteCollection, existing.RKey); err != nil {
			s.logger.Error("failed to delete existing vote on PDS",
				"error", err,
				"voter", session.AccountDID,
				"rkey", existing.RKey)
			if pds.IsAuthError(err) {
				return nil, ErrNotAuthorized
			}
			return nil, fmt.Errorf("failed to delete existing vote: %w", err)
		}

		s.logger.Info("deleted existing vote before creating new direction",
			"voter", session.AccountDID,
			"subject", req.Subject.URI,
			"old_direction", existing.Direction,
			"new_direction", req.Direction)
	}

	// Create new vote
	uri, cid, err := s.createVoteRecord(ctx, pdsClient, req)
	if err != nil {
		s.logger.Error("failed to create vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI,
			"direction", req.Direction)
		if pds.IsAuthError(err) {
			return nil, ErrNotAuthorized
		}
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

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"voter", session.AccountDID)
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Find existing vote by querying PDS directly (source of truth)
	// This avoids eventual consistency issues with the AppView database
	existing, err := s.findExistingVote(ctx, pdsClient, req.Subject.URI)
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
	if err := pdsClient.DeleteRecord(ctx, voteCollection, existing.RKey); err != nil {
		s.logger.Error("failed to delete vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"rkey", existing.RKey)
		if pds.IsAuthError(err) {
			return ErrNotAuthorized
		}
		return fmt.Errorf("failed to delete vote: %w", err)
	}

	s.logger.Info("vote deleted",
		"voter", session.AccountDID,
		"subject", req.Subject.URI,
		"uri", existing.URI)

	return nil
}

// createVoteRecord writes a vote record to the user's PDS using PDSClient
func (s *voteService) createVoteRecord(ctx context.Context, pdsClient pds.Client, req CreateVoteRequest) (string, string, error) {
	// Generate TID for the record key
	tid := syntax.NewTIDNow(0)

	// Build vote record following the lexicon schema
	record := VoteRecord{
		Type: voteCollection,
		Subject: StrongRef{
			URI: req.Subject.URI,
			CID: req.Subject.CID,
		},
		Direction: req.Direction,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	uri, cid, err := pdsClient.CreateRecord(ctx, voteCollection, tid.String(), record)
	if err != nil {
		return "", "", fmt.Errorf("createRecord failed: %w", err)
	}

	return uri, cid, nil
}

// existingVote represents a vote record found on the PDS
type existingVote struct {
	URI       string
	CID       string
	RKey      string
	Direction string
}

// findExistingVote queries the user's PDS directly to find an existing vote for a subject.
// This avoids eventual consistency issues with the AppView database populated by Jetstream.
// Paginates through all vote records to handle users with >100 votes.
// Returns the vote record with rkey, or nil if no vote exists for the subject.
func (s *voteService) findExistingVote(ctx context.Context, pdsClient pds.Client, subjectURI string) (*existingVote, error) {
	cursor := ""
	const pageSize = 100

	// Paginate through all vote records
	for {
		result, err := pdsClient.ListRecords(ctx, voteCollection, pageSize, cursor)
		if err != nil {
			// Check for auth errors using typed errors
			if pds.IsAuthError(err) {
				return nil, ErrNotAuthorized
			}
			return nil, fmt.Errorf("listRecords failed: %w", err)
		}

		// Search for the vote matching our subject in this page
		for _, rec := range result.Records {
			// Extract subject from record value
			subject, ok := rec.Value["subject"].(map[string]any)
			if !ok {
				continue
			}

			subjectURIValue, ok := subject["uri"].(string)
			if !ok {
				continue
			}

			if subjectURIValue == subjectURI {
				// Extract rkey from the URI (at://did/collection/rkey)
				parts := strings.Split(rec.URI, "/")
				if len(parts) < 5 {
					continue
				}
				rkey := parts[len(parts)-1]

				// Extract direction
				direction, _ := rec.Value["direction"].(string)

				return &existingVote{
					URI:       rec.URI,
					CID:       rec.CID,
					RKey:      rkey,
					Direction: direction,
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

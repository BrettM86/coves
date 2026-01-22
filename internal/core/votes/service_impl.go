package votes

import (
	"context"
	"errors"
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
	cache            *VoteCache       // In-memory cache of user votes from PDS
}

// NewService creates a new vote service instance
func NewService(repo Repository, oauthClient *oauthclient.OAuthClient, oauthStore oauth.ClientAuthStore, cache *VoteCache, logger *slog.Logger) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:        repo,
		oauthClient: oauthClient,
		oauthStore:  oauthStore,
		cache:       cache,
		logger:      logger,
	}
}

// NewServiceWithPDSFactory creates a vote service with a custom PDS client factory.
// This is primarily for testing with password-based authentication.
func NewServiceWithPDSFactory(repo Repository, cache *VoteCache, logger *slog.Logger, factory PDSClientFactory) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:             repo,
		cache:            cache,
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

	// Check for existing vote using cache with PDS fallback
	// First check populates cache from PDS, subsequent checks are O(1) lookups
	existing, err := s.findExistingVoteWithCache(ctx, pdsClient, session.AccountDID.String(), req.Subject.URI)
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

			// Update cache - remove the vote
			if s.cache != nil {
				s.cache.RemoveVote(session.AccountDID.String(), req.Subject.URI)
			}

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

	// Update cache - add the new vote
	if s.cache != nil {
		s.cache.SetVote(session.AccountDID.String(), req.Subject.URI, &CachedVote{
			Direction: req.Direction,
			URI:       uri,
			RKey:      extractRKeyFromURI(uri),
		})
	}

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

	// Find existing vote using cache with PDS fallback
	// First check populates cache from PDS, subsequent checks are O(1) lookups
	existing, err := s.findExistingVoteWithCache(ctx, pdsClient, session.AccountDID.String(), req.Subject.URI)
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

	// Update cache - remove the vote
	if s.cache != nil {
		s.cache.RemoveVote(session.AccountDID.String(), req.Subject.URI)
	}

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

// findExistingVoteWithCache uses the vote cache for O(1) lookups when available.
// Falls back to direct PDS pagination if cache is unavailable or cannot be populated.
func (s *voteService) findExistingVoteWithCache(ctx context.Context, pdsClient pds.Client, userDID string, subjectURI string) (*existingVote, error) {
	if s.cache != nil {
		if !s.cache.IsCached(userDID) {
			// Populate cache first (fetches all votes via pagination, then cached for subsequent O(1) lookups)
			if err := s.cache.FetchAndCacheFromPDS(ctx, pdsClient); err != nil {
				// Auth errors won't succeed on fallback either - propagate immediately
				if errors.Is(err, ErrNotAuthorized) {
					return nil, err
				}
				// Log warning for other errors and fall back to direct PDS query
				s.logger.Warn("failed to populate vote cache, falling back to PDS pagination",
					"error", err,
					"user", userDID,
					"subject", subjectURI)
			}
		}

		if s.cache.IsCached(userDID) {
			cached := s.cache.GetVote(userDID, subjectURI)
			if cached == nil {
				s.logger.Debug("vote existence check via cache: not found",
					"user", userDID,
					"subject", subjectURI)
				return nil, nil // No vote exists
			}
			s.logger.Debug("vote existence check via cache: found",
				"user", userDID,
				"subject", subjectURI,
				"direction", cached.Direction)
			return &existingVote{
				URI:       cached.URI,
				RKey:      cached.RKey,
				Direction: cached.Direction,
				// CID not cached - not needed for toggle/delete operations
			}, nil
		}
	}

	// Fallback: query PDS directly via pagination
	s.logger.Debug("vote existence check via PDS pagination (cache unavailable)",
		"user", userDID,
		"subject", subjectURI)
	return s.findExistingVoteFromPDS(ctx, pdsClient, subjectURI)
}

// findExistingVoteFromPDS queries the user's PDS directly to find an existing vote for a subject.
// This is the slow fallback path that paginates through all vote records.
// Prefer findExistingVoteWithCache for production use.
// Returns the vote record with rkey, or nil if no vote exists for the subject.
func (s *voteService) findExistingVoteFromPDS(ctx context.Context, pdsClient pds.Client, subjectURI string) (*existingVote, error) {
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

// EnsureCachePopulated fetches the user's votes from their PDS if not already cached.
func (s *voteService) EnsureCachePopulated(ctx context.Context, session *oauth.ClientSessionData) error {
	if s.cache == nil {
		return nil // No cache configured
	}

	// Check if already cached
	if s.cache.IsCached(session.AccountDID.String()) {
		return nil
	}

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client for cache population",
			"error", err,
			"user", session.AccountDID)
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Fetch and cache votes from PDS
	if err := s.cache.FetchAndCacheFromPDS(ctx, pdsClient); err != nil {
		s.logger.Error("failed to populate vote cache from PDS",
			"error", err,
			"user", session.AccountDID)
		return fmt.Errorf("failed to populate vote cache: %w", err)
	}

	return nil
}

// GetViewerVote returns the viewer's vote for a specific subject, or nil if not voted.
func (s *voteService) GetViewerVote(userDID, subjectURI string) *CachedVote {
	if s.cache == nil {
		return nil
	}
	return s.cache.GetVote(userDID, subjectURI)
}

// GetViewerVotesForSubjects returns the viewer's votes for multiple subjects.
func (s *voteService) GetViewerVotesForSubjects(userDID string, subjectURIs []string) map[string]*CachedVote {
	result := make(map[string]*CachedVote)
	if s.cache == nil {
		return result
	}

	allVotes := s.cache.GetVotesForUser(userDID)
	if allVotes == nil {
		return result
	}

	for _, uri := range subjectURIs {
		if vote, exists := allVotes[uri]; exists {
			result[uri] = vote
		}
	}

	return result
}

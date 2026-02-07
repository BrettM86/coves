package userblocks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	oauthclient "Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
	"Coves/internal/atproto/utils"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	// blockCollection is the AT Protocol collection for user block records
	blockCollection = "social.coves.actor.block"
)

// HandleResolver resolves AT Protocol handles to DIDs.
// This is a minimal interface satisfied by users.UserService.
type HandleResolver interface {
	ResolveHandleToDID(ctx context.Context, handle string) (string, error)
}

type userBlockService struct {
	repo             Repository
	handleResolver   HandleResolver
	oauthClient      *oauthclient.OAuthClient
	oauthStore       oauth.ClientAuthStore
	logger           *slog.Logger
	pdsClientFactory PDSClientFactory // Optional, for testing. If nil, uses OAuth.
}

// NewService creates a new user block service with OAuth client for production use.
func NewService(
	repo Repository,
	handleResolver HandleResolver,
	oauthClient *oauthclient.OAuthClient,
	oauthStore oauth.ClientAuthStore,
	logger *slog.Logger,
) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &userBlockService{
		repo:           repo,
		handleResolver: handleResolver,
		oauthClient:    oauthClient,
		oauthStore:     oauthStore,
		logger:         logger,
	}
}

// NewServiceWithPDSFactory creates a user block service with a custom PDS client factory.
// This is primarily for testing with password-based authentication.
func NewServiceWithPDSFactory(
	repo Repository,
	handleResolver HandleResolver,
	factory PDSClientFactory,
) Service {
	return &userBlockService{
		repo:             repo,
		handleResolver:   handleResolver,
		pdsClientFactory: factory,
		logger:           slog.Default(),
	}
}

// BlockUser creates a block against another user via write-forward to PDS.
// The identifier can be a DID (starts with "did:") or a handle.
// Returns a BlockResult with the record URI and CID from PDS.
// The block will be indexed asynchronously via the firehose consumer.
func (s *userBlockService) BlockUser(ctx context.Context, session *oauth.ClientSessionData, identifier string) (*BlockResult, error) {
	if session == nil {
		return nil, fmt.Errorf("session is required")
	}

	blockerDID := session.AccountDID.String()

	// Validate and normalize identifier
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, fmt.Errorf("block: identifier is required")
	}

	// Resolve identifier to DID
	blockedDID, err := s.resolveIdentifier(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("block: %w", err)
	}

	// Prevent self-blocking
	if blockerDID == blockedDID {
		s.logger.Warn("self-block attempt",
			"blockerDID", blockerDID)
		return nil, ErrCannotBlockSelf
	}

	// Get PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		return nil, err
	}

	// Generate TID for record key
	tid := syntax.NewTIDNow(0)

	// Build block record following atProto conventions
	blockRecord := map[string]interface{}{
		"$type":     blockCollection,
		"subject":   blockedDID,
		"createdAt": time.Now().Format(time.RFC3339),
	}

	// Write-forward: create block record in user's repo
	recordURI, recordCID, err := pdsClient.CreateRecord(ctx, blockCollection, tid.String(), blockRecord)
	if err != nil {
		// Check for auth errors
		if pds.IsAuthError(err) {
			s.logger.Warn("block auth failure",
				"blockerDID", blockerDID,
				"blockedDID", blockedDID,
				"error", err)
			return nil, fmt.Errorf("unauthorized: %w", err)
		}

		// Check for duplicate/conflict error from PDS
		if pds.IsConflictError(err) {
			existingBlock, getErr := s.repo.GetBlock(ctx, blockerDID, blockedDID)
			if getErr == nil {
				return &BlockResult{
					RecordURI: existingBlock.RecordURI,
					RecordCID: existingBlock.RecordCID,
				}, nil
			}
			if errors.Is(getErr, ErrBlockNotFound) {
				return nil, ErrBlockAlreadyExists
			}
			return nil, fmt.Errorf("PDS reported duplicate block but failed to fetch from index: %w", getErr)
		}
		return nil, fmt.Errorf("failed to create block on PDS: %w", err)
	}

	s.logger.Info("block created",
		"blockerDID", blockerDID,
		"blockedDID", blockedDID,
		"recordURI", recordURI)

	return &BlockResult{
		RecordURI: recordURI,
		RecordCID: recordCID,
	}, nil
}

// UnblockUser removes a block against another user via PDS delete.
func (s *userBlockService) UnblockUser(ctx context.Context, session *oauth.ClientSessionData, identifier string) error {
	if session == nil {
		return fmt.Errorf("session is required")
	}

	blockerDID := session.AccountDID.String()

	// Validate and normalize identifier
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return fmt.Errorf("unblock: identifier is required")
	}

	// Resolve identifier to DID
	blockedDID, err := s.resolveIdentifier(ctx, identifier)
	if err != nil {
		return fmt.Errorf("unblock: %w", err)
	}

	// Get the block from AppView to find the record key
	block, err := s.repo.GetBlock(ctx, blockerDID, blockedDID)
	if err != nil {
		return err
	}

	// Extract rkey from record URI (at://did/collection/rkey)
	rkey := utils.ExtractRKeyFromURI(block.RecordURI)
	if rkey == "" {
		return fmt.Errorf("invalid block record URI")
	}

	// Get PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Write-forward: delete record from PDS
	if err := pdsClient.DeleteRecord(ctx, blockCollection, rkey); err != nil {
		if pds.IsAuthError(err) {
			s.logger.Warn("unblock auth failure",
				"blockerDID", blockerDID,
				"blockedDID", blockedDID,
				"error", err)
			return fmt.Errorf("unauthorized: %w", err)
		}
		return fmt.Errorf("failed to delete block on PDS: %w", err)
	}

	s.logger.Info("block deleted",
		"blockerDID", blockerDID,
		"blockedDID", blockedDID)

	return nil
}

// GetBlockedUsers retrieves all users blocked by the given user, paginated.
func (s *userBlockService) GetBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	return s.repo.ListBlockedUsers(ctx, blockerDID, limit, offset)
}

// IsBlocked checks if blockerDID has blocked blockedDID.
func (s *userBlockService) IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error) {
	return s.repo.IsBlocked(ctx, blockerDID, blockedDID)
}

// resolveIdentifier resolves an identifier to a DID.
// If the identifier starts with "did:", it is validated as a proper DID.
// Otherwise, it is treated as a handle and resolved via the handle resolver.
func (s *userBlockService) resolveIdentifier(ctx context.Context, identifier string) (string, error) {
	if strings.HasPrefix(identifier, "did:") {
		// Validate DID using the syntax package
		if _, err := syntax.ParseDID(identifier); err != nil {
			return "", fmt.Errorf("invalid DID %q: %w", identifier, err)
		}
		return identifier, nil
	}
	return s.handleResolver.ResolveHandleToDID(ctx, identifier)
}

// getPDSClient creates a PDS client from an OAuth session.
// If a custom factory was provided (for testing), uses that.
// Otherwise, uses OAuth with DPoP authentication.
func (s *userBlockService) getPDSClient(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
	if s.pdsClientFactory != nil {
		return s.pdsClientFactory(ctx, session)
	}

	if s.oauthClient == nil || s.oauthClient.ClientApp == nil {
		return nil, fmt.Errorf("OAuth client not configured")
	}

	client, err := pds.NewFromOAuthSession(ctx, s.oauthClient.ClientApp, session)
	if err != nil {
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	return client, nil
}

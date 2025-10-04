package users

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// atProto handle validation regex
// Handles must: start/end with alphanumeric, contain only alphanumeric + hyphens, no consecutive hyphens
var handleRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

type userService struct {
	userRepo UserRepository
	pdsURL   string // TODO: Support federated PDS - different users may have different PDS hosts
}

// NewUserService creates a new user service
func NewUserService(userRepo UserRepository, pdsURL string) UserService {
	return &userService{
		userRepo: userRepo,
		pdsURL:   pdsURL,
	}
}

// CreateUser creates a new user in the AppView database
func (s *userService) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// Normalize handle
	req.Handle = strings.TrimSpace(strings.ToLower(req.Handle))
	req.DID = strings.TrimSpace(req.DID)

	user := &User{
		DID:    req.DID,
		Handle: req.Handle,
	}

	// Repository will handle duplicate constraint errors
	return s.userRepo.Create(ctx, user)
}

// GetUserByDID retrieves a user by their DID
func (s *userService) GetUserByDID(ctx context.Context, did string) (*User, error) {
	if strings.TrimSpace(did) == "" {
		return nil, fmt.Errorf("DID is required")
	}

	return s.userRepo.GetByDID(ctx, did)
}

// GetUserByHandle retrieves a user by their handle
func (s *userService) GetUserByHandle(ctx context.Context, handle string) (*User, error) {
	handle = strings.TrimSpace(strings.ToLower(handle))
	if handle == "" {
		return nil, fmt.Errorf("handle is required")
	}

	return s.userRepo.GetByHandle(ctx, handle)
}

// ResolveHandleToDID resolves a handle to a DID
// This is critical for login: users enter their handle, we resolve to DID
// TODO: Implement actual DNS/HTTPS resolution via atProto
func (s *userService) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	handle = strings.TrimSpace(strings.ToLower(handle))
	if handle == "" {
		return "", fmt.Errorf("handle is required")
	}

	// For now, check if user exists in our AppView database
	// Later: implement DNS TXT record lookup or HTTPS .well-known/atproto-did
	user, err := s.userRepo.GetByHandle(ctx, handle)
	if err != nil {
		return "", fmt.Errorf("failed to resolve handle %s: %w", handle, err)
	}

	return user.DID, nil
}

func (s *userService) validateCreateRequest(req CreateUserRequest) error {
	if strings.TrimSpace(req.DID) == "" {
		return fmt.Errorf("DID is required")
	}

	if strings.TrimSpace(req.Handle) == "" {
		return fmt.Errorf("handle is required")
	}

	// DID format validation
	if !strings.HasPrefix(req.DID, "did:") {
		return fmt.Errorf("invalid DID format: must start with 'did:'")
	}

	// atProto handle validation
	handle := strings.TrimSpace(strings.ToLower(req.Handle))

	// Length validation (1-253 characters per atProto spec)
	if len(handle) < 1 || len(handle) > 253 {
		return fmt.Errorf("handle must be between 1 and 253 characters")
	}

	// Regex validation: alphanumeric + hyphens + dots, no consecutive hyphens
	if !handleRegex.MatchString(handle) {
		return fmt.Errorf("invalid handle format: must contain only alphanumeric characters, hyphens, and dots; must start and end with alphanumeric; no consecutive hyphens")
	}

	// Check for consecutive hyphens (not allowed in atProto)
	if strings.Contains(handle, "--") {
		return fmt.Errorf("invalid handle format: consecutive hyphens not allowed")
	}

	return nil
}
package users

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// atProto handle validation regex (per official atProto spec: https://atproto.com/specs/handle)
// - Must have at least one dot (domain-like structure)
// - Each segment max 63 chars, total max 253 chars
// - Segments: alphanumeric start/end, hyphens allowed in middle
// - TLD (final segment) must start with letter (not digit)
// - Case-insensitive, normalized to lowercase
var handleRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// Disallowed TLDs per atProto spec
var disallowedTLDs = map[string]bool{
	".alt":       true,
	".arpa":      true,
	".example":   true,
	".internal":  true,
	".invalid":   true,
	".local":     true,
	".localhost": true,
	".onion":     true,
	// .test is allowed for development
}

const (
	minPasswordLength = 8 // Reasonable minimum, though PDS may enforce stricter rules
	maxHandleLength   = 253
)

type userService struct {
	userRepo   UserRepository
	defaultPDS string // Default PDS URL for this Coves instance (used when creating new local users via registration API)
}

// NewUserService creates a new user service
func NewUserService(userRepo UserRepository, defaultPDS string) UserService {
	return &userService{
		userRepo:   userRepo,
		defaultPDS: defaultPDS,
	}
}

// CreateUser creates a new user in the AppView database
// This method is idempotent: if a user with the same DID already exists, it returns the existing user
func (s *userService) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// Normalize handle
	req.Handle = strings.TrimSpace(strings.ToLower(req.Handle))
	req.DID = strings.TrimSpace(req.DID)
	req.PDSURL = strings.TrimSpace(req.PDSURL)

	user := &User{
		DID:    req.DID,
		Handle: req.Handle,
		PDSURL: req.PDSURL,
	}

	// Try to create the user
	createdUser, err := s.userRepo.Create(ctx, user)
	if err != nil {
		// If user with this DID already exists, fetch and return it (idempotent behavior)
		if strings.Contains(err.Error(), "user with DID already exists") {
			existingUser, getErr := s.userRepo.GetByDID(ctx, req.DID)
			if getErr != nil {
				return nil, fmt.Errorf("user exists but failed to fetch: %w", getErr)
			}
			return existingUser, nil
		}
		// For other errors (validation, handle conflict, etc.), return the error
		return nil, err
	}

	return createdUser, nil
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

// RegisterAccount creates a new account on the PDS via XRPC
// This is what a UI signup button would call - it handles the PDS account creation
func (s *userService) RegisterAccount(ctx context.Context, req RegisterAccountRequest) (*RegisterAccountResponse, error) {
	if err := s.validateRegisterRequest(req); err != nil {
		return nil, err
	}

	// Call PDS com.atproto.server.createAccount XRPC endpoint
	pdsURL := strings.TrimSuffix(s.defaultPDS, "/")
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.server.createAccount", pdsURL)

	payload := map[string]string{
		"handle":   req.Handle,
		"email":    req.Email,
		"password": req.Password,
	}
	if req.InviteCode != "" {
		payload["inviteCode"] = req.InviteCode
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Set timeout to prevent hanging on slow/unavailable PDS
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call PDS: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	var pdsResp RegisterAccountResponse
	if err := json.Unmarshal(body, &pdsResp); err != nil {
		return nil, fmt.Errorf("failed to parse PDS response: %w", err)
	}

	// Set the PDS URL in the response (PDS doesn't return this)
	pdsResp.PDSURL = s.defaultPDS

	return &pdsResp, nil
}

func (s *userService) validateCreateRequest(req CreateUserRequest) error {
	if strings.TrimSpace(req.DID) == "" {
		return fmt.Errorf("DID is required")
	}

	if strings.TrimSpace(req.Handle) == "" {
		return fmt.Errorf("handle is required")
	}

	if strings.TrimSpace(req.PDSURL) == "" {
		return fmt.Errorf("PDS URL is required")
	}

	// DID format validation
	if !strings.HasPrefix(req.DID, "did:") {
		return fmt.Errorf("invalid DID format: must start with 'did:'")
	}

	// Validate handle format
	if err := validateHandle(req.Handle); err != nil {
		return err
	}

	return nil
}

func (s *userService) validateRegisterRequest(req RegisterAccountRequest) error {
	if strings.TrimSpace(req.Handle) == "" {
		return fmt.Errorf("handle is required")
	}

	if strings.TrimSpace(req.Email) == "" {
		return &InvalidEmailError{Email: req.Email}
	}

	// Basic email validation
	if !strings.Contains(req.Email, "@") || !strings.Contains(req.Email, ".") {
		return &InvalidEmailError{Email: req.Email}
	}

	// Password validation
	if strings.TrimSpace(req.Password) == "" {
		return &WeakPasswordError{Reason: "password is required"}
	}

	if len(req.Password) < minPasswordLength {
		return &WeakPasswordError{Reason: fmt.Sprintf("password must be at least %d characters", minPasswordLength)}
	}

	// Validate handle format
	if err := validateHandle(req.Handle); err != nil {
		return err
	}

	return nil
}

// validateHandle validates handle per atProto spec: https://atproto.com/specs/handle
func validateHandle(handle string) error {
	// Normalize to lowercase (handles are case-insensitive)
	handle = strings.TrimSpace(strings.ToLower(handle))

	if handle == "" {
		return &InvalidHandleError{Handle: handle, Reason: "handle cannot be empty"}
	}

	// Check length
	if len(handle) > maxHandleLength {
		return &InvalidHandleError{Handle: handle, Reason: fmt.Sprintf("handle exceeds maximum length of %d characters", maxHandleLength)}
	}

	// Check regex pattern
	if !handleRegex.MatchString(handle) {
		return &InvalidHandleError{Handle: handle, Reason: "handle must be domain-like (e.g., user.bsky.social), with segments of alphanumeric/hyphens separated by dots"}
	}

	// Check for disallowed TLDs
	for tld := range disallowedTLDs {
		if strings.HasSuffix(handle, tld) {
			return &InvalidHandleError{Handle: handle, Reason: fmt.Sprintf("TLD %s is not allowed", tld)}
		}
	}

	return nil
}
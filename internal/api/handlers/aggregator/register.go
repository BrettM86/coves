package aggregator

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// maxWellKnownSize limits the response body size when fetching .well-known/atproto-did.
	// DIDs are typically ~60 characters. A 4KB limit leaves ample room for whitespace or
	// future metadata while still preventing attackers from streaming unbounded data.
	maxWellKnownSize = 4 * 1024 // bytes
)

// RegisterHandler handles aggregator registration
type RegisterHandler struct {
	userService      users.UserService
	identityResolver identity.Resolver
	httpClient       *http.Client // Allows test injection
}

// NewRegisterHandler creates a new registration handler
func NewRegisterHandler(userService users.UserService, identityResolver identity.Resolver) *RegisterHandler {
	return &RegisterHandler{
		userService:      userService,
		identityResolver: identityResolver,
		httpClient:       &http.Client{Timeout: 10 * time.Second},
	}
}

// SetHTTPClient allows overriding the HTTP client (for testing with self-signed certs)
func (h *RegisterHandler) SetHTTPClient(client *http.Client) {
	h.httpClient = client
}

// RegisterRequest represents the registration request
type RegisterRequest struct {
	DID    string `json:"did"`
	Domain string `json:"domain"`
}

// RegisterResponse represents the registration response
type RegisterResponse struct {
	DID     string `json:"did"`
	Handle  string `json:"handle"`
	Message string `json:"message"`
}

// HandleRegister handles aggregator registration
// POST /xrpc/social.coves.aggregator.register
//
// Architecture Note: This handler contains business logic for domain verification.
// This is intentional for the following reasons:
// 1. Registration is a one-time setup operation, not core aggregator business logic
// 2. It primarily delegates to UserService (proper service layer)
// 3. Domain verification is an infrastructure concern (like TLS verification)
// 4. Moving to AggregatorService would create circular dependency (aggregators table has FK to users)
// 5. Similar pattern used in Bluesky's PDS for account creation
func (h *RegisterHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidDID", "Invalid request body: JSON decode failed")
		return
	}

	// Validate input
	if err := validateRegistrationRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidDID", err.Error())
		return
	}

	// Normalize inputs
	req.DID = strings.TrimSpace(req.DID)
	req.Domain = strings.TrimSpace(req.Domain)

	// Reject HTTP explicitly (HTTPS required for domain verification)
	if strings.HasPrefix(req.Domain, "http://") {
		writeError(w, http.StatusBadRequest, "InvalidDID", "Domain must use HTTPS, not HTTP")
		return
	}

	req.Domain = strings.TrimPrefix(req.Domain, "https://")
	req.Domain = strings.TrimSuffix(req.Domain, "/")

	// Re-validate after normalization to catch edge cases like "   " or "https://"
	if req.Domain == "" {
		writeError(w, http.StatusBadRequest, "InvalidDID", "Domain cannot be empty")
		return
	}

	// Verify domain ownership via .well-known
	if err := h.verifyDomainOwnership(r.Context(), req.DID, req.Domain); err != nil {
		log.Printf("Domain verification failed for DID %s, domain %s: %v", req.DID, req.Domain, err)
		writeError(w, http.StatusUnauthorized, "DomainVerificationFailed",
			"Could not verify domain ownership. Ensure .well-known/atproto-did serves your DID over HTTPS")
		return
	}

	// Check if user already exists (before CreateUser since it's idempotent)
	existingUser, err := h.userService.GetUserByDID(r.Context(), req.DID)
	if err == nil && existingUser != nil {
		writeError(w, http.StatusConflict, "AlreadyRegistered",
			"This aggregator is already registered with this instance")
		return
	}

	// Resolve DID to get handle and PDS URL
	identityInfo, err := h.identityResolver.Resolve(r.Context(), req.DID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIDResolutionFailed",
			"Could not resolve DID. Please verify it exists in the PLC directory")
		return
	}

	// Register the aggregator in the users table
	createReq := users.CreateUserRequest{
		DID:    req.DID,
		Handle: identityInfo.Handle,
		PDSURL: identityInfo.PDSURL,
	}

	user, err := h.userService.CreateUser(r.Context(), createReq)
	if err != nil {
		log.Printf("Failed to create user for aggregator DID %s: %v", req.DID, err)
		writeError(w, http.StatusInternalServerError, "RegistrationFailed",
			"Failed to register aggregator")
		return
	}

	// Return success response
	response := RegisterResponse{
		DID:     user.DID,
		Handle:  user.Handle,
		Message: fmt.Sprintf("Aggregator registered successfully. Next step: create a service declaration record at at://%s/social.coves.aggregator.service/self", user.DID),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// validateRegistrationRequest validates the registration request
func validateRegistrationRequest(req RegisterRequest) error {
	// Validate DID format
	if req.DID == "" {
		return fmt.Errorf("did is required")
	}

	if !strings.HasPrefix(req.DID, "did:") {
		return fmt.Errorf("did must start with 'did:' prefix")
	}

	// We support did:plc for now (most common for aggregators)
	if !strings.HasPrefix(req.DID, "did:plc:") && !strings.HasPrefix(req.DID, "did:web:") {
		return fmt.Errorf("only did:plc and did:web formats are currently supported")
	}

	// Validate domain
	if req.Domain == "" {
		return fmt.Errorf("domain is required")
	}

	return nil
}

// verifyDomainOwnership verifies that the domain serves the correct DID in .well-known/atproto-did
func (h *RegisterHandler) verifyDomainOwnership(ctx context.Context, expectedDID, domain string) error {
	// Construct .well-known URL
	wellKnownURL := fmt.Sprintf("https://%s/.well-known/atproto-did", domain)

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnownURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Perform request
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch .well-known/atproto-did from %s: %w", domain, err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(".well-known/atproto-did returned status %d (expected 200)", resp.StatusCode)
	}

	// Read body with size limit to prevent DoS attacks from malicious servers
	// streaming arbitrarily large responses. Read one extra byte so we can detect
	// when the response exceeded the allowed size instead of silently truncating.
	limitedReader := io.LimitReader(resp.Body, maxWellKnownSize+1)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return fmt.Errorf("failed to read .well-known/atproto-did response: %w", err)
	}

	if len(body) > maxWellKnownSize {
		return fmt.Errorf(".well-known/atproto-did response exceeds %d bytes", maxWellKnownSize)
	}

	// Parse DID from response
	actualDID := strings.TrimSpace(string(body))

	// Verify DID matches
	if actualDID != expectedDID {
		return fmt.Errorf("DID mismatch: .well-known/atproto-did contains '%s', expected '%s'", actualDID, expectedDID)
	}

	return nil
}

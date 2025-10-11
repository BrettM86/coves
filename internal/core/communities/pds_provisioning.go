package communities

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"Coves/internal/core/users"
	"golang.org/x/crypto/bcrypt"
)

// CommunityPDSAccount represents PDS account credentials for a community
type CommunityPDSAccount struct {
	DID          string // Community's DID (owns the repository)
	Handle       string // Community's handle (e.g., gaming.coves.social)
	Email        string // System email for PDS account
	PasswordHash string // bcrypt hash of generated password
	AccessToken  string // JWT for making API calls as the community
	RefreshToken string // For refreshing sessions
	PDSURL       string // PDS hosting this community
}

// PDSAccountProvisioner creates PDS accounts for communities
type PDSAccountProvisioner struct {
	userService    users.UserService
	instanceDomain string
	pdsURL         string
}

// NewPDSAccountProvisioner creates a new provisioner
func NewPDSAccountProvisioner(userService users.UserService, instanceDomain string, pdsURL string) *PDSAccountProvisioner {
	return &PDSAccountProvisioner{
		userService:    userService,
		instanceDomain: instanceDomain,
		pdsURL:         pdsURL,
	}
}

// ProvisionCommunityAccount creates a real PDS account for a community
//
// This function:
// 1. Generates a unique handle (e.g., gaming.coves.social)
// 2. Generates a system email (e.g., community-gaming@system.coves.social)
// 3. Generates a secure random password
// 4. Calls com.atproto.server.createAccount via the PDS
// 5. The PDS automatically generates and stores the signing keypair
// 6. Returns credentials for Coves to act on behalf of the community
//
// V2 Architecture:
// - Community DID owns its own repository (at://community_did/...)
// - PDS manages signing keys (we never see them)
// - We store credentials to authenticate as the community
// - Future: Add rotation key management for true portability (V2.1)
func (p *PDSAccountProvisioner) ProvisionCommunityAccount(
	ctx context.Context,
	communityName string,
) (*CommunityPDSAccount, error) {
	if communityName == "" {
		return nil, fmt.Errorf("community name is required")
	}

	// 1. Generate unique handle for the community using subdomain
	// This makes it immediately clear these are communities, not user accounts
	// Format: {name}.communities.{instance-domain}
	handle := fmt.Sprintf("%s.communities.%s", strings.ToLower(communityName), p.instanceDomain)
	// Example: "gaming.communities.coves.social" (much cleaner!)

	// 2. Generate system email for PDS account management
	// This email is used for account operations, not for user communication
	email := fmt.Sprintf("community-%s@communities.%s", strings.ToLower(communityName), p.instanceDomain)
	// Example: "community-gaming@communities.coves.social"

	// 3. Generate secure random password (32 characters)
	// This password is never shown to users - it's for Coves to authenticate as the community
	password, err := generateSecurePassword(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate password: %w", err)
	}

	// 4. Call PDS com.atproto.server.createAccount
	// The PDS will:
	//   - Generate a signing keypair (we never see the private key)
	//   - Create a DID (did:plc:xxx)
	//   - Store the private signing key securely
	//   - Return DID, handle, and authentication tokens
	//
	// Note: No inviteCode needed for our local PDS (configure PDS with invites disabled)
	resp, err := p.userService.RegisterAccount(ctx, users.RegisterAccountRequest{
		Handle:   handle,
		Email:    email,
		Password: password,
		// InviteCode: "", // Not needed if PDS has open registration or we're admin
	})
	if err != nil {
		return nil, fmt.Errorf("PDS account creation failed for community %s: %w", communityName, err)
	}

	// 5. Hash the password for storage
	// We need to store the password hash so we can re-authenticate if tokens expire
	// This is secure - bcrypt is industry standard
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	// 6. Return account credentials
	return &CommunityPDSAccount{
		DID:          resp.DID,           // The community's DID - it owns its own repository!
		Handle:       resp.Handle,        // e.g., gaming.coves.social
		Email:        email,              // community-gaming@system.coves.social
		PasswordHash: string(passwordHash), // bcrypt hash for re-authentication
		AccessToken:  resp.AccessJwt,     // JWT for making API calls as the community
		RefreshToken: resp.RefreshJwt,    // For refreshing sessions when access token expires
		PDSURL:       resp.PDSURL,        // PDS hosting this community's repository
	}, nil
}

// generateSecurePassword creates a cryptographically secure random password
// Uses crypto/rand for security-critical randomness
func generateSecurePassword(length int) (string, error) {
	if length < 8 {
		return "", fmt.Errorf("password length must be at least 8 characters")
	}

	// Generate random bytes
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Encode as base64 URL-safe (no special chars that need escaping)
	password := base64.URLEncoding.EncodeToString(bytes)

	// Trim to exact length
	if len(password) > length {
		password = password[:length]
	}

	return password, nil
}

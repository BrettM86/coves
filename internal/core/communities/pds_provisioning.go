package communities

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/api/atproto"
	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/xrpc"
)

// CommunityPDSAccount represents PDS account credentials for a community
type CommunityPDSAccount struct {
	DID            string // Community's DID (owns the repository)
	Handle         string // Community's handle (e.g., gaming.communities.coves.social)
	Email          string // System email for PDS account
	Password       string // Cleartext password (MUST be encrypted before database storage)
	AccessToken    string // JWT for making API calls as the community
	RefreshToken   string // For refreshing sessions
	PDSURL         string // PDS hosting this community
	RotationKeyPEM string // PEM-encoded rotation key (for portability)
	SigningKeyPEM  string // PEM-encoded signing key (for atproto operations)
}

// PDSAccountProvisioner creates PDS accounts for communities with PDS-managed DIDs
type PDSAccountProvisioner struct {
	instanceDomain string
	pdsURL         string // URL to call PDS (e.g., http://localhost:3001)
}

// NewPDSAccountProvisioner creates a new provisioner for V2.0 (PDS-managed keys)
func NewPDSAccountProvisioner(instanceDomain, pdsURL string) *PDSAccountProvisioner {
	return &PDSAccountProvisioner{
		instanceDomain: instanceDomain,
		pdsURL:         pdsURL,
	}
}

// ProvisionCommunityAccount creates a real PDS account for a community with PDS-managed keys
//
// V2.0 Architecture (PDS-Managed Keys):
// 1. Generates community handle and credentials
// 2. Calls com.atproto.server.createAccount (PDS generates DID and keys)
// 3. Returns credentials for storage
//
// V2.0 Design Philosophy:
// - PDS manages ALL cryptographic keys (signing + rotation)
// - Communities can migrate between Coves-controlled PDSs using standard atProto migration
// - Simpler, faster, ships immediately
// - Migration uses com.atproto.server.getServiceAuth + standard migration endpoints
//
// Future V2.1 (Optional Portability Enhancement):
// - Add Coves-controlled rotation key alongside PDS rotation key
// - Enables migration to non-Coves PDSs
// - Implement when actual external migration is needed
//
// SECURITY: The returned credentials MUST be encrypted before database storage
func (p *PDSAccountProvisioner) ProvisionCommunityAccount(
	ctx context.Context,
	communityName string,
) (*CommunityPDSAccount, error) {
	if communityName == "" {
		return nil, fmt.Errorf("community name is required")
	}

	// 1. Generate unique handle for the community
	// Format: {name}.communities.{instance-domain}
	// Example: "gaming.communities.coves.social"
	handle := fmt.Sprintf("%s.communities.%s", strings.ToLower(communityName), p.instanceDomain)

	// 2. Generate system email for PDS account management
	// This email is used for account operations, not for user communication
	email := fmt.Sprintf("community-%s@communities.%s", strings.ToLower(communityName), p.instanceDomain)

	// 3. Generate secure random password (32 characters)
	// This password is never shown to users - it's for Coves to authenticate as the community
	password, err := generateSecurePassword(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate password: %w", err)
	}

	// 4. Create PDS account - let PDS generate DID and all keys
	// The PDS will:
	//   1. Generate a signing keypair (stored in PDS, never exported)
	//   2. Generate rotation keys (stored in PDS)
	//   3. Create a DID (did:plc:xxx)
	//   4. Register DID with PLC directory
	//   5. Return credentials (DID, handle, tokens)
	client := &xrpc.Client{
		Host: p.pdsURL,
	}

	emailStr := email
	passwordStr := password

	input := &atproto.ServerCreateAccount_Input{
		Handle:   handle,
		Email:    &emailStr,
		Password: &passwordStr,
		// No Did parameter - let PDS generate it
		// No RecoveryKey - PDS manages rotation keys
	}

	output, err := atproto.ServerCreateAccount(ctx, client, input)
	if err != nil {
		return nil, fmt.Errorf("PDS account creation failed for community %s: %w", communityName, err)
	}

	// 5. Return account credentials with cleartext password
	// CRITICAL: The password MUST be encrypted (not hashed) before database storage
	// We need to recover the plaintext password to call com.atproto.server.createSession
	// when access/refresh tokens expire (90-day window on refresh tokens)
	// The repository layer handles encryption using pgp_sym_encrypt()
	return &CommunityPDSAccount{
		DID:            output.Did,        // The community's DID (PDS-generated)
		Handle:         output.Handle,     // e.g., gaming.communities.coves.social
		Email:          email,             // community-gaming@communities.coves.social
		Password:       password,          // Cleartext - will be encrypted by repository
		AccessToken:    output.AccessJwt,  // JWT for making API calls
		RefreshToken:   output.RefreshJwt, // For refreshing sessions
		PDSURL:         p.pdsURL,          // PDS hosting this community
		RotationKeyPEM: "",                // Empty - PDS manages keys (V2.1: add Coves rotation key)
		SigningKeyPEM:  "",                // Empty - PDS manages keys
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

// FetchPDSDID queries the PDS to get its DID via com.atproto.server.describeServer
// This is the proper way to get the PDS DID rather than hardcoding it
// Works in both development (did:web:localhost) and production (did:web:pds.example.com)
func FetchPDSDID(ctx context.Context, pdsURL string) (string, error) {
	client := &xrpc.Client{
		Host: pdsURL,
	}

	resp, err := comatproto.ServerDescribeServer(ctx, client)
	if err != nil {
		return "", fmt.Errorf("failed to describe server at %s: %w", pdsURL, err)
	}

	if resp.Did == "" {
		return "", fmt.Errorf("PDS at %s did not return a DID", pdsURL)
	}

	return resp.Did, nil
}

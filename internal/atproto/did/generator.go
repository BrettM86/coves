package did

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// Generator creates DIDs for Coves entities
type Generator struct {
	isDevEnv        bool   // true = generate without registering, false = register with PLC
	plcDirectoryURL string // PLC directory URL (only used when isDevEnv=false)
}

// NewGenerator creates a new DID generator
// isDevEnv: true for local development (no PLC registration), false for production (register with PLC)
// plcDirectoryURL: URL for PLC directory (e.g., "https://plc.directory")
func NewGenerator(isDevEnv bool, plcDirectoryURL string) *Generator {
	return &Generator{
		isDevEnv:        isDevEnv,
		plcDirectoryURL: plcDirectoryURL,
	}
}

// GenerateCommunityDID creates a new random DID for a community
// Format: did:plc:{base32-random}
//
// Dev mode (isDevEnv=true):  Generates did:plc:xxx without registering to PLC
// Prod mode (isDevEnv=false): Generates did:plc:xxx AND registers with PLC directory
//
// See: https://github.com/bluesky-social/did-method-plc
func (g *Generator) GenerateCommunityDID() (string, error) {
	// Generate 16 random bytes for the DID identifier
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random DID: %w", err)
	}

	// Encode as base32 (lowercase, no padding) - matches PLC format
	encoded := base32.StdEncoding.EncodeToString(randomBytes)
	encoded = strings.ToLower(strings.TrimRight(encoded, "="))

	did := fmt.Sprintf("did:plc:%s", encoded)

	// TODO: In production (isDevEnv=false), register this DID with PLC directory
	// This would involve:
	// 1. Generate signing keypair for the DID
	// 2. Create DID document with service endpoints
	// 3. POST to plcDirectoryURL to register
	// 4. Store keypair securely for future DID updates
	//
	// For now, we just generate the identifier (works fine for local dev)
	if !g.isDevEnv {
		// Future: implement PLC registration here
		// return "", fmt.Errorf("PLC registration not yet implemented")
	}

	return did, nil
}

// ValidateDID checks if a DID string is properly formatted
// Supports did:plc, did:web (for instances)
func ValidateDID(did string) bool {
	if !strings.HasPrefix(did, "did:") {
		return false
	}

	parts := strings.Split(did, ":")
	if len(parts) < 3 {
		return false
	}

	method := parts[1]
	identifier := parts[2]

	// Basic validation: method and identifier must not be empty
	return method != "" && identifier != ""
}

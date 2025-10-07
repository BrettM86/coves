package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/lestrrat-go/jwx/v2/jwk"
)

// genjwks generates an ES256 keypair for OAuth client authentication
// The private key is stored in the config/env, public key is served at /oauth/jwks.json
//
// Usage:
//   go run cmd/genjwks/main.go
//
// This will output a JSON private key that should be stored in OAUTH_PRIVATE_JWK
func main() {
	fmt.Println("Generating ES256 keypair for OAuth client authentication...")

	// Generate ES256 (NIST P-256) private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("Failed to generate private key: %v", err)
	}

	// Convert to JWK
	jwkKey, err := jwk.FromRaw(privateKey)
	if err != nil {
		log.Fatalf("Failed to create JWK from private key: %v", err)
	}

	// Set key parameters
	if err := jwkKey.Set(jwk.KeyIDKey, "oauth-client-key"); err != nil {
		log.Fatalf("Failed to set kid: %v", err)
	}
	if err := jwkKey.Set(jwk.AlgorithmKey, "ES256"); err != nil {
		log.Fatalf("Failed to set alg: %v", err)
	}
	if err := jwkKey.Set(jwk.KeyUsageKey, "sig"); err != nil {
		log.Fatalf("Failed to set use: %v", err)
	}

	// Marshal to JSON
	jsonData, err := json.MarshalIndent(jwkKey, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal JWK: %v", err)
	}

	// Output instructions
	fmt.Println("\n✅ ES256 keypair generated successfully!")
	fmt.Println("\n📝 Add this to your .env.dev file:")
	fmt.Println("\nOAUTH_PRIVATE_JWK='" + string(jsonData) + "'")
	fmt.Println("\n⚠️  IMPORTANT:")
	fmt.Println("   - Keep this private key SECRET")
	fmt.Println("   - Never commit it to version control")
	fmt.Println("   - Generate a new key for production")
	fmt.Println("   - The public key will be automatically derived and served at /oauth/jwks.json")

	// Optionally write to a file (not committed)
	if len(os.Args) > 1 && os.Args[1] == "--save" {
		filename := "oauth-private-key.json"
		if err := os.WriteFile(filename, jsonData, 0600); err != nil {
			log.Fatalf("Failed to write key file: %v", err)
		}
		fmt.Printf("\n💾 Private key saved to %s (remember to add to .gitignore!)\n", filename)
	}
}

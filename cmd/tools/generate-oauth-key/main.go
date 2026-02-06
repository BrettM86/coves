// Package main provides a utility to generate P-256 private keys for OAuth confidential clients.
// Run with: go run ./cmd/tools/generate-oauth-key
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

func main() {
	priv, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating key: %v\n", err)
		os.Exit(1)
	}

	// Output in format suitable for .env file
	fmt.Printf("# OAuth Confidential Client Key\n")
	fmt.Printf("# Generated: %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("# WARNING: Keep this private key secure! Never commit to version control.\n")
	fmt.Printf("OAUTH_CLIENT_PRIVATE_KEY=%s\n", priv.Multibase())
	fmt.Printf("OAUTH_CLIENT_KEY_ID=coves-key-%d\n", time.Now().Unix())
}

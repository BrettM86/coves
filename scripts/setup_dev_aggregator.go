// setup_dev_aggregator.go - Creates a local test aggregator on the local PDS
//
// This script creates an aggregator account on the local PDS for development testing.
// After running, you'll need to:
// 1. Register the aggregator via OAuth UI
// 2. Generate an API key via the createApiKey endpoint
//
// Usage: go run scripts/setup_dev_aggregator.go
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	_ "github.com/lib/pq"
)

const (
	PDSURL      = "http://localhost:3001"
	DatabaseURL = "postgres://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"
)

type CreateAccountRequest struct {
	Email    string `json:"email"`
	Handle   string `json:"handle"`
	Password string `json:"password"`
}

type CreateAccountResponse struct {
	DID       string `json:"did"`
	Handle    string `json:"handle"`
	AccessJWT string `json:"accessJwt"`
}

type CreateSessionRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

type CreateSessionResponse struct {
	DID       string `json:"did"`
	Handle    string `json:"handle"`
	AccessJWT string `json:"accessJwt"`
}

func main() {
	ctx := context.Background()

	// Configuration
	handle := "test-aggregator.local.coves.dev"
	email := "test-aggregator@example.com"
	password := "test-password-12345"
	displayName := "Test Aggregator (Dev)"

	log.Printf("Setting up dev aggregator: %s", handle)

	// Connect to database
	db, err := sql.Open("postgres", DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Step 1: Try to create account on PDS (or get existing session)
	log.Printf("Creating account on PDS: %s", PDSURL)

	var did string

	// First try to create account
	createResp, err := createAccount(handle, email, password)
	if err != nil {
		log.Printf("Account creation failed (may already exist): %v", err)
		log.Printf("Trying to create session with existing account...")

		// Try to login instead
		sessionResp, err := createSession(handle, password)
		if err != nil {
			log.Fatalf("Failed to create session: %v", err)
		}
		did = sessionResp.DID
		log.Printf("Logged in as existing account: %s", did)
	} else {
		did = createResp.DID
		log.Printf("Created new account: %s", did)
	}

	// Step 2: Check if already in users table
	var existingHandle string
	err = db.QueryRowContext(ctx, "SELECT handle FROM users WHERE did = $1", did).Scan(&existingHandle)
	if err == nil {
		log.Printf("User already exists in users table: %s", existingHandle)
	} else {
		// Insert into users table
		log.Printf("Inserting user into users table...")
		_, err = db.ExecContext(ctx, `
			INSERT INTO users (did, handle, pds_url)
			VALUES ($1, $2, $3)
			ON CONFLICT (did) DO UPDATE SET handle = $2
		`, did, handle, PDSURL)
		if err != nil {
			log.Fatalf("Failed to insert user: %v", err)
		}
	}

	// Step 3: Check if already in aggregators table
	var existingAggDID string
	err = db.QueryRowContext(ctx, "SELECT did FROM aggregators WHERE did = $1", did).Scan(&existingAggDID)
	if err == nil {
		log.Printf("Aggregator already exists in aggregators table")
	} else {
		// Insert into aggregators table
		log.Printf("Inserting aggregator into aggregators table...")
		recordURI := fmt.Sprintf("at://%s/social.coves.aggregator.declaration/self", did)
		recordCID := "dev-placeholder-cid"

		_, err = db.ExecContext(ctx, `
			INSERT INTO aggregators (did, display_name, description, record_uri, record_cid, created_at, indexed_at)
			VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		`, did, displayName, "Development test aggregator", recordURI, recordCID)
		if err != nil {
			log.Fatalf("Failed to insert aggregator: %v", err)
		}
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("  DEV AGGREGATOR ACCOUNT CREATED")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Printf("  DID:      %s\n", did)
	fmt.Printf("  Handle:   %s\n", handle)
	fmt.Printf("  Password: %s\n", password)
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Println("  1. Start Coves server: make run")
	fmt.Println("  2. Authenticate as this account via OAuth UI")
	fmt.Println("  3. Call POST /xrpc/social.coves.aggregator.createApiKey")
	fmt.Println("  4. Save the API key and add to aggregators/kagi-news/.env")
	fmt.Println()
	fmt.Println("========================================")
}

func createAccount(handle, email, password string) (*CreateAccountResponse, error) {
	reqBody := CreateAccountRequest{
		Email:    email,
		Handle:   handle,
		Password: password,
	}

	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(PDSURL+"/xrpc/com.atproto.server.createAccount", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var result CreateAccountResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &result, nil
}

func createSession(identifier, password string) (*CreateSessionResponse, error) {
	reqBody := CreateSessionRequest{
		Identifier: identifier,
		Password:   password,
	}

	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(PDSURL+"/xrpc/com.atproto.server.createSession", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var result CreateSessionResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &result, nil
}

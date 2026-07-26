package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// pdsAuthTimeout bounds the instance's own PDS login at startup.
//
// Without it this used http.Post, which uses http.DefaultClient — and
// http.DefaultClient has no timeout at all. A PDS that accepted the connection
// but never responded would hang the boot indefinitely, with the process
// looking alive to the orchestrator but serving nothing.
const pdsAuthTimeout = 15 * time.Second

// maxPDSErrorBodyBytes bounds how much of a PDS error body is read into the
// returned error, so a misbehaving server cannot force an unbounded read.
const maxPDSErrorBodyBytes = 4 << 10

// authenticateWithPDS creates a session on the PDS and returns an access
// token. The instance needs its own PDS account to write the community records
// it owns.
func authenticateWithPDS(ctx context.Context, pdsURL, handle, password string) (string, error) {
	type createSessionRequest struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}

	type createSessionResponse struct {
		DID       string `json:"did"`
		Handle    string `json:"handle"`
		AccessJwt string `json:"accessJwt"`
	}

	reqBody, err := json.Marshal(createSessionRequest{
		Identifier: handle,
		Password:   password,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, pdsAuthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pdsURL+"/xrpc/com.atproto.server.createSession",
		bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed to build PDS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call PDS: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Error("failed to close PDS response body", "error", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPDSErrorBodyBytes))
		if readErr != nil {
			return "", fmt.Errorf("PDS returned status %d and failed to read body: %w", resp.StatusCode, readErr)
		}
		// The password is never echoed back by createSession, so the body is
		// safe to surface here; it is the only useful diagnostic for a
		// misconfigured instance account.
		return "", fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	var session createSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if session.AccessJwt == "" {
		return "", errors.New("PDS returned a session with no access token")
	}

	return session.AccessJwt, nil
}

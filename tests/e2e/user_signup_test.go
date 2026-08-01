//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"Coves/tests/testkit"
)

// TestE2E_UserSignup is the API contract (§3.4b) for social.coves.actor.signup:
// the one write surface a client reaches before it has any credentials at all.
//
// # WHY IT IS AN API CONTRACT AND NOT A PIPELINE PROOF
//
// Signup indexes SYNCHRONOUSLY. The endpoint creates the account on the PDS and
// writes the user row itself, in the same request, so every assertion below
// would hold with the firehose completely dead — which is exactly the trap
// §3.4 was written around. Nothing here may be read as evidence that ingestion
// works. The pipeline proof for this domain is TestActorProfileIngestion, and
// the standing rule the whole tier depends on is that a DID enters the index
// ONLY through this endpoint: every ingestion contract signs an account up here
// first, which is what makes this surface worth its own contract.
//
// # WHY IT SPEAKS RAW HTTP
//
// Deliberately, and it is the one file in the tier that should. The claim is
// about what a THIRD-PARTY client experiences — a client that has no Go, no
// testkit and no session, only the wire format. Routing it through the shared
// XRPC client would test the client's marshalling as much as the endpoint's.
// The URLs still come from testkit.Endpoints(), because §3.7 forbids a test
// constructing a base address no matter how it then dials it.
func TestE2E_UserSignup(t *testing.T) {
	// No availability probes and no skips. TestMain's Require floor already
	// proved the AppView, the PDS and Jetstream are reachable and failed the
	// whole package with the address it could not reach if they were not
	// (§3.1: asking for -tags e2e IS asking for the stack). The three skip
	// calls that used to stand here could only ever turn a broken stack into a
	// silent pass.

	t.Run("Create account on PDS and verify indexing", func(t *testing.T) {
		handle, email := signupAccount(t, "alice")

		t.Logf("Creating account: %s", handle)
		did, err := createPDSAccount(t, handle, email, "test1234")
		if err != nil {
			t.Fatalf("Failed to create PDS account: %v", err)
		}
		t.Logf("Account created with DID: %s", did)

		userDID, userHandle := awaitProfile(t, did)

		if userHandle != handle {
			t.Errorf("Expected handle %s, got %s", handle, userHandle)
		}
		if userDID != did {
			t.Errorf("Expected DID %s, got %s", did, userDID)
		}
	})

	// Named for what it does, after a rename: it used to be called "Idempotent
	// indexing on duplicate events" and it delivers no duplicate event. Two GETs
	// prove the read is stable and the signup produced ONE addressable actor,
	// which is worth having — a signup that wrote two rows shows up here as a
	// lookup that starts matching two — but it is not the consumer's
	// duplicate-delivery guarantee and must not be read as covering it.
	//
	// Real duplicate delivery is unreachable from this surface: signup is an
	// endpoint, and calling it twice with the same handle is refused by the PDS
	// long before any consumer sees anything. The guarantee is proven where the
	// duplicate can actually be constructed — at T1 in
	// internal/core/users/user_identity_consumer_test.go, which delivers one
	// event twice and asserts the row after each — and, for the delivery
	// mechanism that produces duplicates in production, by
	// TestReliabilityRewindReplaysExactlyOnce in reliability_test.go.
	t.Run("Signup produces exactly one addressable actor", func(t *testing.T) {
		handle, email := signupAccount(t, "bob")

		did, err := createPDSAccount(t, handle, email, "test1234")
		if err != nil {
			t.Fatalf("Failed to create PDS account: %v", err)
		}

		userDID1, _ := awaitProfile(t, did)

		userDID2, _, err := getProfileViaAPI(did)
		if err != nil {
			t.Fatalf("Failed to get user on second query: %v", err)
		}

		if userDID1 != userDID2 {
			t.Errorf("Got different DIDs on repeated queries: %s vs %s", userDID1, userDID2)
		}
	})

	t.Run("Index multiple users", func(t *testing.T) {
		// Sequential, despite the name this subtest used to carry: the accounts
		// are created one after another and the endpoint is what is under test,
		// not the AppView's concurrency. The 500ms "small delay between
		// creations" that used to sit in this loop was doing nothing — signup is
		// synchronous, so there is nothing to wait for between two of them.
		const numUsers = 3
		dids := make([]string, numUsers)

		for i := 0; i < numUsers; i++ {
			handle, email := signupAccount(t, fmt.Sprintf("user%d", i))
			did, err := createPDSAccount(t, handle, email, "test1234")
			if err != nil {
				t.Fatalf("Failed to create account %d: %v", i, err)
			}
			dids[i] = did
		}

		for i, did := range dids {
			_, userHandle := awaitProfile(t, did)
			t.Logf("user %d indexed: %s", i, userHandle)
		}
	})
}

// signupAccount mints a handle and email for a signup this tier is about to
// perform.
//
// The local label comes from testkit — run-prefixed and inside the PDS'
// 18-character cap — and the handle domain comes from the stack rather than a
// literal, so this works against whatever domain the PDS is configured to
// serve. Both matter for the same reason: the PDS keeps accounts between runs,
// so a handle that is not unique per run is a "Handle already taken" failure on
// the second invocation.
func signupAccount(t *testing.T, prefix string) (handle, email string) {
	t.Helper()
	label := testkit.UniqueIDWithPrefix(t, prefix)
	return testkit.Endpoints().PDS.Handle(label), label + "@test.com"
}

// awaitProfile waits for social.coves.actor.getProfile to serve the DID that was
// just signed up, and returns what it served.
//
// Signup indexes synchronously, so in practice this returns on the first probe.
// It is a wait rather than a bare read because the endpoint is HTTP and the
// process is shared: a 502 while the AppView is momentarily busy is not the
// answer to the question being asked. It is emphatically NOT a wait for the
// firehose — see this file's doc comment.
func awaitProfile(t *testing.T, did string) (userDID, userHandle string) {
	t.Helper()

	testkit.WaitFor(t, contractBudget, func() (bool, error) {
		var err error
		userDID, userHandle, err = getProfileViaAPI(did)
		if err != nil {
			return false, nil
		}
		return true, nil
	},
		testkit.WithPollInterval(contractPollInterval),
		testkit.WithDescription("social.coves.actor.getProfile to serve the freshly signed-up %s", did))

	return userDID, userHandle
}

// generateInviteCode generates a single-use invite code via PDS admin API
func generateInviteCode(t *testing.T) (string, error) {
	payload := map[string]int{
		"useCount": 1,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest(
		"POST",
		testkit.Endpoints().PDS.BaseURL+"/xrpc/com.atproto.server.createInviteCode",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// PDS admin authentication
	req.SetBasicAuth("admin", "admin")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create invite code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&errorResp); err != nil {
			return "", fmt.Errorf("PDS admin API returned status %d (failed to decode error: %w)", resp.StatusCode, err)
		}
		return "", fmt.Errorf("PDS admin API returned status %d: %v", resp.StatusCode, errorResp)
	}

	var result struct {
		Code string `json:"code"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	t.Logf("Generated invite code: %s", result.Code)
	return result.Code, nil
}

// createPDSAccount creates an account via the coves.user.signup XRPC endpoint
// This is the same code path that a third-party client or UI would use
func createPDSAccount(t *testing.T, handle, email, password string) (string, error) {
	// Generate fresh invite code for each account
	inviteCode, err := generateInviteCode(t)
	if err != nil {
		return "", fmt.Errorf("failed to generate invite code: %w", err)
	}

	// Call our XRPC endpoint (what a third-party client would call)
	payload := map[string]string{
		"handle":     handle,
		"email":      email,
		"password":   password,
		"inviteCode": inviteCode,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post(
		testkit.Endpoints().AppView.BaseURL+"/xrpc/social.coves.actor.signup",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to call signup endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&errorResp); err != nil {
			return "", fmt.Errorf("signup endpoint returned status %d (failed to decode error: %w)", resp.StatusCode, err)
		}
		return "", fmt.Errorf("signup endpoint returned status %d: %v", resp.StatusCode, errorResp)
	}

	var result struct {
		DID        string `json:"did"`
		Handle     string `json:"handle"`
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	t.Logf("Account created via XRPC endpoint: %s → %s", result.Handle, result.DID)

	return result.DID, nil
}

// getProfileViaAPI queries the AppView API to get a user profile by DID
func getProfileViaAPI(did string) (string, string, error) {
	resp, err := http.Get(fmt.Sprintf("%s/xrpc/social.coves.actor.getProfile?actor=%s",
		testkit.Endpoints().AppView.BaseURL, did))
	if err != nil {
		return "", "", fmt.Errorf("failed to call getProfile: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("getProfile returned status %d", resp.StatusCode)
	}

	var result struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to decode response: %w", err)
	}

	return result.DID, result.Handle, nil
}

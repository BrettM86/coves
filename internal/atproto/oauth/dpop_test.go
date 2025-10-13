package oauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestCreateDPoPProof tests DPoP proof generation and structure
func TestCreateDPoPProof(t *testing.T) {
	// Generate a test DPoP key
	dpopKey, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("Failed to generate DPoP key: %v", err)
	}

	// Create a DPoP proof
	proof, err := CreateDPoPProof(dpopKey, "POST", "https://example.com/token", "", "")
	if err != nil {
		t.Fatalf("Failed to create DPoP proof: %v", err)
	}

	// DPoP proof should be a JWT in form: header.payload.signature
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("Expected 3 parts in JWT, got %d", len(parts))
	}

	// Decode and inspect the header
	headerJSON, decodeErr := base64.RawURLEncoding.DecodeString(parts[0])
	if decodeErr != nil {
		t.Fatalf("Failed to decode header: %v", decodeErr)
	}

	var header map[string]interface{}
	if unmarshalErr := json.Unmarshal(headerJSON, &header); unmarshalErr != nil {
		t.Fatalf("Failed to unmarshal header: %v", unmarshalErr)
	}

	t.Logf("DPoP Header: %s", string(headerJSON))

	// Verify required header fields
	if header["alg"] != "ES256" {
		t.Errorf("Expected alg=ES256, got %v", header["alg"])
	}
	if header["typ"] != "dpop+jwt" {
		t.Errorf("Expected typ=dpop+jwt, got %v", header["typ"])
	}

	// Verify JWK is present and is a JSON object
	jwkValue, hasJWK := header["jwk"]
	if !hasJWK {
		t.Fatal("Header missing 'jwk' field")
	}

	// JWK should be a map/object, not a string
	jwkMap, ok := jwkValue.(map[string]interface{})
	if !ok {
		t.Fatalf("JWK is not a JSON object, got type: %T, value: %v", jwkValue, jwkValue)
	}

	// Verify JWK has required fields for EC key
	if jwkMap["kty"] != "EC" {
		t.Errorf("Expected kty=EC, got %v", jwkMap["kty"])
	}
	if jwkMap["crv"] != "P-256" {
		t.Errorf("Expected crv=P-256, got %v", jwkMap["crv"])
	}
	if _, hasX := jwkMap["x"]; !hasX {
		t.Error("JWK missing 'x' coordinate")
	}
	if _, hasY := jwkMap["y"]; !hasY {
		t.Error("JWK missing 'y' coordinate")
	}

	// Verify private key is NOT in the public JWK
	if _, hasD := jwkMap["d"]; hasD {
		t.Error("SECURITY: JWK contains private key component 'd'!")
	}

	// Decode and inspect the payload
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("Failed to decode payload: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	t.Logf("DPoP Payload: %s", string(payloadJSON))

	// Verify required payload claims
	if payload["htm"] != "POST" {
		t.Errorf("Expected htm=POST, got %v", payload["htm"])
	}
	if payload["htu"] != "https://example.com/token" {
		t.Errorf("Expected htu=https://example.com/token, got %v", payload["htu"])
	}
	if _, hasIAT := payload["iat"]; !hasIAT {
		t.Error("Payload missing 'iat' (issued at)")
	}
	if _, hasJTI := payload["jti"]; !hasJTI {
		t.Error("Payload missing 'jti' (JWT ID)")
	}
}

// TestDPoPProofWithNonce tests DPoP proof with nonce
func TestDPoPProofWithNonce(t *testing.T) {
	dpopKey, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("Failed to generate DPoP key: %v", err)
	}

	testNonce := "test-nonce-12345"
	proof, err := CreateDPoPProof(dpopKey, "POST", "https://example.com/token", testNonce, "")
	if err != nil {
		t.Fatalf("Failed to create DPoP proof: %v", err)
	}

	// Decode payload
	parts := strings.Split(proof, ".")
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("Failed to decode payload: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	if payload["nonce"] != testNonce {
		t.Errorf("Expected nonce=%s, got %v", testNonce, payload["nonce"])
	}
}

// TestDPoPProofWithAccessToken tests DPoP proof with access token hash
func TestDPoPProofWithAccessToken(t *testing.T) {
	dpopKey, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("Failed to generate DPoP key: %v", err)
	}

	testToken := "test-access-token"
	proof, err := CreateDPoPProof(dpopKey, "GET", "https://example.com/resource", "", testToken)
	if err != nil {
		t.Fatalf("Failed to create DPoP proof: %v", err)
	}

	// Decode payload
	parts := strings.Split(proof, ".")
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("Failed to decode payload: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	ath, hasATH := payload["ath"]
	if !hasATH {
		t.Fatal("Payload missing 'ath' (access token hash)")
	}
	if ath == "" {
		t.Error("Access token hash is empty")
	}

	t.Logf("Access token hash: %v", ath)
}

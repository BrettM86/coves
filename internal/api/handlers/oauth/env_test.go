package oauth

import (
	"encoding/base64"
	"os"
	"testing"
)

func TestGetEnvBase64OrPlain(t *testing.T) {
	tests := []struct {
		name      string
		envKey    string
		envValue  string
		want      string
		wantError bool
	}{
		{
			name:      "plain JSON value",
			envKey:    "TEST_PLAIN_JSON",
			envValue:  `{"alg":"ES256","kty":"EC"}`,
			want:      `{"alg":"ES256","kty":"EC"}`,
			wantError: false,
		},
		{
			name:      "base64 encoded value",
			envKey:    "TEST_BASE64_JSON",
			envValue:  "base64:" + base64.StdEncoding.EncodeToString([]byte(`{"alg":"ES256","kty":"EC"}`)),
			want:      `{"alg":"ES256","kty":"EC"}`,
			wantError: false,
		},
		{
			name:      "empty value",
			envKey:    "TEST_EMPTY",
			envValue:  "",
			want:      "",
			wantError: false,
		},
		{
			name:      "invalid base64",
			envKey:    "TEST_INVALID_BASE64",
			envValue:  "base64:not-valid-base64!!!",
			want:      "",
			wantError: true,
		},
		{
			name:      "plain string with special chars",
			envKey:    "TEST_SPECIAL_CHARS",
			envValue:  "secret-with-dashes_and_underscores",
			want:      "secret-with-dashes_and_underscores",
			wantError: false,
		},
		{
			name:      "base64 encoded hex string",
			envKey:    "TEST_BASE64_HEX",
			envValue:  "base64:" + base64.StdEncoding.EncodeToString([]byte("f1132c01b1a625a865c6c455a75ee793")),
			want:      "f1132c01b1a625a865c6c455a75ee793",
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable
			if tt.envValue != "" {
				os.Setenv(tt.envKey, tt.envValue)
				defer os.Unsetenv(tt.envKey)
			}

			got, err := GetEnvBase64OrPlain(tt.envKey)

			if (err != nil) != tt.wantError {
				t.Errorf("GetEnvBase64OrPlain() error = %v, wantError %v", err, tt.wantError)
				return
			}

			if got != tt.want {
				t.Errorf("GetEnvBase64OrPlain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetEnvBase64OrPlain_RealWorldJWK(t *testing.T) {
	// Test with a real JWK (the one from .env.dev)
	realJWK := `{"alg":"ES256","crv":"P-256","d":"9tCMceYSgyZfO5KYOCm3rWEhXLqq2l4LjP7-PJtJKyk","kid":"oauth-client-key","kty":"EC","use":"sig","x":"EOYWEgZ2d-smTO6jh0f-9B7YSFYdlrvlryjuXTCrOjE","y":"_FR2jBcWNxoJl5cd1eq9sYtAs33No9AVtd42UyyWYi4"}`

	tests := []struct {
		name     string
		envValue string
		want     string
	}{
		{
			name:     "plain JWK",
			envValue: realJWK,
			want:     realJWK,
		},
		{
			name:     "base64 encoded JWK",
			envValue: "base64:" + base64.StdEncoding.EncodeToString([]byte(realJWK)),
			want:     realJWK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_REAL_JWK", tt.envValue)
			defer os.Unsetenv("TEST_REAL_JWK")

			got, err := GetEnvBase64OrPlain("TEST_REAL_JWK")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tt.want {
				t.Errorf("GetEnvBase64OrPlain() = %v, want %v", got, tt.want)
			}
		})
	}
}

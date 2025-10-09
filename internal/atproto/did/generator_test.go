package did

import (
	"strings"
	"testing"
)

func TestGenerateCommunityDID(t *testing.T) {
	tests := []struct {
		name            string
		isDevEnv        bool
		plcDirectoryURL string
		want            string // prefix we expect
	}{
		{
			name:            "generates did:plc in dev mode",
			isDevEnv:        true,
			plcDirectoryURL: "https://plc.directory",
			want:            "did:plc:",
		},
		{
			name:            "generates did:plc in prod mode",
			isDevEnv:        false,
			plcDirectoryURL: "https://plc.directory",
			want:            "did:plc:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGenerator(tt.isDevEnv, tt.plcDirectoryURL)
			did, err := g.GenerateCommunityDID()

			if err != nil {
				t.Fatalf("GenerateCommunityDID() error = %v", err)
			}

			if !strings.HasPrefix(did, tt.want) {
				t.Errorf("GenerateCommunityDID() = %v, want prefix %v", did, tt.want)
			}

			// Verify it's a valid DID
			if !ValidateDID(did) {
				t.Errorf("Generated DID failed validation: %v", did)
			}
		})
	}
}

func TestGenerateCommunityDID_Uniqueness(t *testing.T) {
	g := NewGenerator(true, "https://plc.directory")

	// Generate 100 DIDs and ensure they're all unique
	dids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		did, err := g.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("GenerateCommunityDID() error = %v", err)
		}

		if dids[did] {
			t.Errorf("Duplicate DID generated: %v", did)
		}
		dids[did] = true
	}
}

func TestValidateDID(t *testing.T) {
	tests := []struct {
		name string
		did  string
		want bool
	}{
		{
			name: "valid did:plc",
			did:  "did:plc:z72i7hdynmk6r22z27h6tvur",
			want: true,
		},
		{
			name: "valid did:plc with base32",
			did:  "did:plc:abc123xyz",
			want: true,
		},
		{
			name: "valid did:web",
			did:  "did:web:coves.social",
			want: true,
		},
		{
			name: "valid did:web with path",
			did:  "did:web:coves.social:community:gaming",
			want: true,
		},
		{
			name: "invalid: missing prefix",
			did:  "plc:abc123",
			want: false,
		},
		{
			name: "invalid: missing method",
			did:  "did::abc123",
			want: false,
		},
		{
			name: "invalid: missing identifier",
			did:  "did:plc:",
			want: false,
		},
		{
			name: "invalid: only did",
			did:  "did:",
			want: false,
		},
		{
			name: "invalid: empty string",
			did:  "",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateDID(tt.did); got != tt.want {
				t.Errorf("ValidateDID(%v) = %v, want %v", tt.did, got, tt.want)
			}
		})
	}
}

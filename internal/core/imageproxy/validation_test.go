package imageproxy

import (
	"errors"
	"testing"
)

func TestValidateDID(t *testing.T) {
	tests := []struct {
		name    string
		did     string
		wantErr error
	}{
		// Valid DIDs - uses Indigo's syntax.ParseDID for consistency with codebase
		{
			name:    "valid did:plc",
			did:     "did:plc:z72i7hdynmk6r22z27h6tvur",
			wantErr: nil,
		},
		{
			name:    "valid did:web simple",
			did:     "did:web:example.com",
			wantErr: nil,
		},
		{
			name:    "valid did:web with subdomain",
			did:     "did:web:bsky.social",
			wantErr: nil,
		},
		{
			name:    "valid did:web with path",
			did:     "did:web:example.com:user:alice",
			wantErr: nil,
		},
		// did:key is valid per Indigo library (used in other atproto contexts)
		{
			name:    "valid did:key",
			did:     "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK",
			wantErr: nil,
		},
		// Invalid DIDs
		{
			name:    "empty string",
			did:     "",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "missing did: prefix",
			did:     "plc:z72i7hdynmk6r22z27h6tvur",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "path traversal attempt in did",
			did:     "did:plc:../../../etc/passwd",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "null byte injection",
			did:     "did:plc:abc\x00def",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "forward slash injection",
			did:     "did:plc:abc/def",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "backslash injection",
			did:     "did:plc:abc\\def",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "just did prefix",
			did:     "did:",
			wantErr: ErrInvalidDID,
		},
		{
			name:    "random gibberish",
			did:     "not-a-did-at-all",
			wantErr: ErrInvalidDID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDID(tt.did)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateDID(%q) = %v, want %v", tt.did, err, tt.wantErr)
			}
		})
	}
}

func TestValidateCID(t *testing.T) {
	tests := []struct {
		name    string
		cid     string
		wantErr error
	}{
		// Valid CIDs
		{
			name:    "valid CIDv1 base32 bafy",
			cid:     "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
			wantErr: nil,
		},
		{
			name:    "valid CIDv1 base32 bafk",
			cid:     "bafkreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
			wantErr: nil,
		},
		{
			name:    "valid CIDv0",
			cid:     "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG",
			wantErr: nil,
		},
		// Invalid CIDs
		{
			name:    "empty string",
			cid:     "",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "too short",
			cid:     "bafyabc",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "path traversal attempt",
			cid:     "../../../etc/passwd",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "contains slash",
			cid:     "bafyrei/abc/def",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "contains backslash",
			cid:     "bafyrei\\abc",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "contains double dot",
			cid:     "bafyrei..abc",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "invalid base32 chars",
			cid:     "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi!@#",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "random string not matching any CID pattern",
			cid:     "this_is_not_a_valid_cid_at_all_12345",
			wantErr: ErrInvalidCID,
		},
		{
			name:    "too long",
			cid:     "bafyrei" + string(make([]byte, 200)),
			wantErr: ErrInvalidCID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCID(tt.cid)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateCID(%q) = %v, want %v", tt.cid, err, tt.wantErr)
			}
		})
	}
}

func TestSanitizePathComponent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean string unchanged",
			input: "abc123",
			want:  "abc123",
		},
		{
			name:  "forward slashes removed",
			input: "path/to/file",
			want:  "path_to_file",
		},
		{
			name:  "backslashes removed",
			input: "path\\to\\file",
			want:  "path_to_file",
		},
		{
			name:  "path traversal removed",
			input: "../../../etc/passwd",
			want:  "___etc_passwd",
		},
		{
			name:  "colons replaced",
			input: "did:plc:abc123",
			want:  "did_plc_abc123",
		},
		{
			name:  "null bytes removed",
			input: "abc\x00def",
			want:  "abcdef",
		},
		{
			name:  "multiple dangerous chars",
			input: "../path:to\\file\x00.txt",
			want:  "_path_to_file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizePathComponent(tt.input)
			if got != tt.want {
				t.Errorf("SanitizePathComponent(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMakeDIDSafe_PathTraversal(t *testing.T) {
	tests := []struct {
		name  string
		did   string
		check func(result string) bool
	}{
		{
			name: "normal did:plc is safe",
			did:  "did:plc:abc123",
			check: func(r string) bool {
				return r == "did_plc_abc123"
			},
		},
		{
			name: "path traversal sequences removed",
			did:  "did:plc:../../../etc/passwd",
			check: func(r string) bool {
				// Should not contain .. or /
				return !contains(r, "..") && !contains(r, "/") && !contains(r, "\\")
			},
		},
		{
			name: "forward slashes removed",
			did:  "did:plc:abc/def",
			check: func(r string) bool {
				return !contains(r, "/")
			},
		},
		{
			name: "backslashes removed",
			did:  "did:plc:abc\\def",
			check: func(r string) bool {
				return !contains(r, "\\")
			},
		},
		{
			name: "null bytes removed",
			did:  "did:plc:abc\x00def",
			check: func(r string) bool {
				return !contains(r, "\x00")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := makeDIDSafe(tt.did)
			if !tt.check(result) {
				t.Errorf("makeDIDSafe(%q) = %q, failed safety check", tt.did, result)
			}
		})
	}
}

func TestMakeCIDSafe_PathTraversal(t *testing.T) {
	tests := []struct {
		name  string
		cid   string
		check func(result string) bool
	}{
		{
			name: "normal CID unchanged",
			cid:  "bafyreiabc123",
			check: func(r string) bool {
				return r == "bafyreiabc123"
			},
		},
		{
			name: "path traversal removed",
			cid:  "../../../etc/passwd",
			check: func(r string) bool {
				return !contains(r, "..") && !contains(r, "/")
			},
		},
		{
			name: "forward slashes removed",
			cid:  "abc/def/ghi",
			check: func(r string) bool {
				return !contains(r, "/")
			},
		},
		{
			name: "backslashes removed",
			cid:  "abc\\def\\ghi",
			check: func(r string) bool {
				return !contains(r, "\\")
			},
		},
		{
			name: "null bytes removed",
			cid:  "abc\x00def",
			check: func(r string) bool {
				return !contains(r, "\x00")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := makeCIDSafe(tt.cid)
			if !tt.check(result) {
				t.Errorf("makeCIDSafe(%q) = %q, failed safety check", tt.cid, result)
			}
		})
	}
}

// helper function for checking string containment
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

package imageproxy

import (
	"errors"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/require"
)

func TestValidateOwnerDID(t *testing.T) {
	for _, test := range []struct {
		name, did string
		accepted  bool
	}{
		{name: "canonical plc", did: "did:plc:z72i7hdynmk6r22z27h6tvur", accepted: true},
		{name: "fixture plc", did: "did:plc:testauthor1", accepted: true},
		{name: "canonical web", did: "did:web:example.test", accepted: true},
		{name: "subdomain and hyphen", did: "did:web:sub.example-host.test", accepted: true},
		{name: "web port", did: "did:web:localhost%3A8080", accepted: true},
		{name: "uppercase plc identifier", did: "did:plc:testAuthor1"},
		{name: "uppercase web host", did: "did:web:Example.test"},
		{name: "escaped web host letter", did: "did:web:%65xample.test"},
		{name: "escaped plc identifier", did: "did:plc:%61bc"},
		{name: "lowercase port escape", did: "did:web:localhost%3a8080"},
		{name: "port escape without digits", did: "did:web:localhost%3A"},
		{name: "web port leading zero", did: "did:web:localhost%3A08080"},
		{name: "web port zero", did: "did:web:localhost%3A0"},
		{name: "web path segments", did: "did:web:example.test:a:b"},
		{name: "web path underscore", did: "did:web:example.test:a_b"},
		{name: "web host underscore", did: "did:web:exa_mple.test"},
		{name: "empty web host", did: "did:web:"},
		{name: "empty plc identifier", did: "did:plc:"},
		{name: "key method", did: "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"},
		{name: "uppercase method", did: "did:PLC:abc"},
		{name: "plc traversal", did: "did:plc:../../../etc/passwd"},
		{name: "plc path", did: "did:plc:abc/def"},
		// The pattern alone accepts these; the disk cache strips ".." from the
		// directory name, so each would share did:plc:abcd's (or did:plc:ab.cd's)
		// cache entries. Only ValidateDID's ".." guard refuses them.
		{name: "plc double dot", did: "did:plc:ab..cd"},
		{name: "plc triple dot", did: "did:plc:ab...cd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateOwnerDID(test.did)
			if test.accepted {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrInvalidDID)
			}
		})
	}
}

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

func TestCanonicalCID(t *testing.T) {
	const canonical = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	parsed, err := cid.Decode(canonical)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	base58, err := parsed.StringOfBase(multibase.Base58BTC)
	if err != nil {
		t.Fatalf("re-encode fixture: %v", err)
	}
	base16, err := parsed.StringOfBase(multibase.Base16)
	if err != nil {
		t.Fatalf("re-encode fixture: %v", err)
	}
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr error
	}{
		{name: "canonical CIDv1 is unchanged", value: canonical, want: canonical},
		{name: "base58btc CIDv1 is canonicalized", value: base58, want: canonical},
		{name: "base16 CIDv1 is canonicalized", value: base16, want: canonical},
		{name: "CIDv0 keeps its canonical form", value: "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", want: "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"},
		{name: "syntax-valid string that is not a CID", value: "bafybeimockimagetest123", wantErr: ErrInvalidCID},
		{name: "path traversal", value: "../../../etc/passwd", wantErr: ErrInvalidCID},
		{name: "empty", value: "", wantErr: ErrInvalidCID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalCID(tt.value)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CanonicalCID(%q) error = %v, want %v", tt.value, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("CanonicalCID(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

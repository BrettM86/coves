package blueskypost

import (
	"Coves/internal/atproto/identity"
	"context"
	"errors"
	"testing"
)

// mockIdentityResolver implements identity.Resolver for testing
type mockIdentityResolver struct {
	handleToDID map[string]string
	err         error
}

func (m *mockIdentityResolver) ResolveHandle(ctx context.Context, handle string) (string, string, error) {
	if m.err != nil {
		return "", "", m.err
	}
	did, ok := m.handleToDID[handle]
	if !ok {
		return "", "", errors.New("handle not found")
	}
	return did, "", nil
}

func (m *mockIdentityResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	if m.err != nil {
		return nil, m.err
	}
	did, _, err := m.ResolveHandle(ctx, identifier)
	if err != nil {
		return nil, err
	}
	return &identity.Identity{
		DID:    did,
		Handle: identifier,
	}, nil
}

func (m *mockIdentityResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	return nil, errors.New("not implemented")
}

func (m *mockIdentityResolver) Purge(ctx context.Context, identifier string) error {
	return nil
}

func TestIsBlueskyURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{
			name:     "valid bsky.app URL with handle",
			url:      "https://bsky.app/profile/user.bsky.social/post/abc123xyz",
			expected: true,
		},
		{
			name:     "valid bsky.app URL with DID",
			url:      "https://bsky.app/profile/did:plc:abc123/post/xyz789",
			expected: true,
		},
		{
			name:     "valid bsky.app URL with alphanumeric rkey",
			url:      "https://bsky.app/profile/alice.example/post/3k2j4h5g6f7d8s9a",
			expected: true,
		},
		{
			name:     "wrong domain",
			url:      "https://twitter.com/profile/user/post/abc123",
			expected: false,
		},
		{
			name:     "wrong path format - missing post",
			url:      "https://bsky.app/profile/user.bsky.social/abc123",
			expected: false,
		},
		{
			name:     "wrong path format - extra segments",
			url:      "https://bsky.app/profile/user.bsky.social/post/abc123/extra",
			expected: false,
		},
		{
			name:     "missing handle",
			url:      "https://bsky.app/profile//post/abc123",
			expected: false,
		},
		{
			name:     "missing rkey",
			url:      "https://bsky.app/profile/user.bsky.social/post/",
			expected: false,
		},
		{
			name:     "empty string",
			url:      "",
			expected: false,
		},
		{
			name:     "malformed URL",
			url:      "not-a-url",
			expected: false,
		},
		{
			name:     "http instead of https",
			url:      "http://bsky.app/profile/user.bsky.social/post/abc123",
			expected: false,
		},
		{
			name:     "AT-URI instead of bsky.app URL",
			url:      "at://did:plc:abc123/app.bsky.feed.post/xyz789",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsBlueskyURL(tt.url)
			if result != tt.expected {
				t.Errorf("IsBlueskyURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

func TestParseBlueskyURL(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		resolver    *mockIdentityResolver
		name        string
		url         string
		expectedURI string
		errContains string
		wantErr     bool
	}{
		{
			name: "valid URL with handle",
			url:  "https://bsky.app/profile/alice.bsky.social/post/abc123xyz",
			resolver: &mockIdentityResolver{
				handleToDID: map[string]string{
					"alice.bsky.social": "did:plc:alice123",
				},
			},
			expectedURI: "at://did:plc:alice123/app.bsky.feed.post/abc123xyz",
			wantErr:     false,
		},
		{
			name: "valid URL with DID (no resolution needed)",
			url:  "https://bsky.app/profile/did:plc:bob456/post/xyz789",
			resolver: &mockIdentityResolver{
				handleToDID: map[string]string{},
			},
			expectedURI: "at://did:plc:bob456/app.bsky.feed.post/xyz789",
			wantErr:     false,
		},
		{
			name: "handle resolution fails",
			url:  "https://bsky.app/profile/unknown.bsky.social/post/abc123",
			resolver: &mockIdentityResolver{
				handleToDID: map[string]string{},
			},
			wantErr:     true,
			errContains: "failed to resolve handle",
		},
		{
			name: "resolver returns error",
			url:  "https://bsky.app/profile/error.bsky.social/post/abc123",
			resolver: &mockIdentityResolver{
				err: errors.New("network error"),
			},
			wantErr:     true,
			errContains: "failed to resolve handle",
		},
		{
			name:        "invalid URL - wrong scheme",
			url:         "http://bsky.app/profile/alice.bsky.social/post/abc123",
			resolver:    &mockIdentityResolver{},
			wantErr:     true,
			errContains: "must use HTTPS scheme",
		},
		{
			name:        "invalid URL - wrong host",
			url:         "https://twitter.com/profile/alice/post/abc123",
			resolver:    &mockIdentityResolver{},
			wantErr:     true,
			errContains: "must be from bsky.app",
		},
		{
			name:        "invalid URL - wrong path format",
			url:         "https://bsky.app/feed/alice.bsky.social/post/abc123",
			resolver:    &mockIdentityResolver{},
			wantErr:     true,
			errContains: "invalid bsky.app URL format",
		},
		{
			name:        "invalid URL - missing rkey",
			url:         "https://bsky.app/profile/alice.bsky.social/post/",
			resolver:    &mockIdentityResolver{},
			wantErr:     true,
			errContains: "invalid bsky.app URL format",
		},
		{
			name:        "invalid URL - empty string",
			url:         "",
			resolver:    &mockIdentityResolver{},
			wantErr:     true,
			errContains: "HTTPS scheme",
		},
		{
			name:        "malformed URL",
			url:         "not-a-valid-url",
			resolver:    &mockIdentityResolver{},
			wantErr:     true,
			errContains: "HTTPS scheme",
		},
		{
			name: "valid URL with complex handle",
			url:  "https://bsky.app/profile/user.subdomain.example.com/post/3k2j4h5g",
			resolver: &mockIdentityResolver{
				handleToDID: map[string]string{
					"user.subdomain.example.com": "did:plc:complex789",
				},
			},
			expectedURI: "at://did:plc:complex789/app.bsky.feed.post/3k2j4h5g",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseBlueskyURL(ctx, tt.url, tt.resolver)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseBlueskyURL() expected error containing %q, got nil", tt.errContains)
					return
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("ParseBlueskyURL() error = %q, expected to contain %q", err.Error(), tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseBlueskyURL() unexpected error: %v", err)
				return
			}

			if result != tt.expectedURI {
				t.Errorf("ParseBlueskyURL() = %q, want %q", result, tt.expectedURI)
			}
		})
	}
}

func TestParseBlueskyURL_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("rkey with special characters should still match pattern", func(t *testing.T) {
		// Bluesky rkeys are base32-like and should be alphanumeric
		// but let's test that our regex handles them
		resolver := &mockIdentityResolver{
			handleToDID: map[string]string{
				"alice.bsky.social": "did:plc:alice123",
			},
		}

		url := "https://bsky.app/profile/alice.bsky.social/post/3km3l4n5m6k7j8h9"
		result, err := ParseBlueskyURL(ctx, url, resolver)
		if err != nil {
			t.Errorf("ParseBlueskyURL() with alphanumeric rkey failed: %v", err)
		}

		expected := "at://did:plc:alice123/app.bsky.feed.post/3km3l4n5m6k7j8h9"
		if result != expected {
			t.Errorf("ParseBlueskyURL() = %q, want %q", result, expected)
		}
	})

	t.Run("handle that looks like DID should not be resolved", func(t *testing.T) {
		// If handle starts with "did:", treat it as DID
		resolver := &mockIdentityResolver{
			handleToDID: map[string]string{
				// Empty map - should not be called
			},
		}

		url := "https://bsky.app/profile/did:plc:direct123/post/abc123"
		result, err := ParseBlueskyURL(ctx, url, resolver)
		if err != nil {
			t.Errorf("ParseBlueskyURL() with DID should not need resolution: %v", err)
		}

		expected := "at://did:plc:direct123/app.bsky.feed.post/abc123"
		if result != expected {
			t.Errorf("ParseBlueskyURL() = %q, want %q", result, expected)
		}
	})

	t.Run("empty handle after split should fail", func(t *testing.T) {
		// This is caught by the regex, but testing defensive validation
		resolver := &mockIdentityResolver{}
		url := "https://bsky.app/profile//post/abc123"

		_, err := ParseBlueskyURL(ctx, url, resolver)
		if err == nil {
			t.Error("ParseBlueskyURL() with empty handle should fail")
		}
	})
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && containsHelper(s, substr)))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

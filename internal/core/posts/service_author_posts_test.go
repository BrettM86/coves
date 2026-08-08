package posts

import (
	"context"
	"testing"
)

// mockRepository implements Repository for testing
type mockRepository struct {
	getByAuthorFunc    func(ctx context.Context, req GetAuthorPostsRequest) ([]*PostView, *string, error)
	getViewsByURIsFunc func(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error)

	// rawRows backs both raw lookups; rawRowsErr makes them fail, which is how a
	// test drives the "we could not find out" path.
	rawRows    map[string]*Post
	rawRowsErr error

	// gotViewsViewerDID records the viewer the service threaded into the
	// visibility gate — "" is a real value here (the anonymous read), so the
	// separate flag is what distinguishes it from "never called".
	gotViewsViewerDID   string
	getViewsByURIsCalls int
	rawBatchCalls       int
}

func (m *mockRepository) Create(ctx context.Context, post *Post) error {
	return nil
}

func (m *mockRepository) GetRawIndexedRow(ctx context.Context, uri string) (*Post, error) {
	if m.rawRowsErr != nil {
		return nil, m.rawRowsErr
	}
	if p, ok := m.rawRows[uri]; ok {
		return p, nil
	}
	return nil, ErrNotFound
}

func (m *mockRepository) GetRawIndexedRowsByURIs(ctx context.Context, uris []string) (map[string]*Post, error) {
	m.rawBatchCalls++
	if m.rawRowsErr != nil {
		return nil, m.rawRowsErr
	}
	out := make(map[string]*Post, len(uris))
	for _, uri := range uris {
		if p, ok := m.rawRows[uri]; ok {
			out[uri] = p
		}
	}
	return out, nil
}

func (m *mockRepository) GetViewsByURIs(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error) {
	m.getViewsByURIsCalls++
	m.gotViewsViewerDID = viewerDID
	if m.getViewsByURIsFunc != nil {
		return m.getViewsByURIsFunc(ctx, uris, viewerDID)
	}
	return map[string]*PostView{}, nil
}

func (m *mockRepository) GetByAuthor(ctx context.Context, req GetAuthorPostsRequest) ([]*PostView, *string, error) {
	if m.getByAuthorFunc != nil {
		return m.getByAuthorFunc(ctx, req)
	}
	return []*PostView{}, nil, nil
}

func (m *mockRepository) SoftDelete(ctx context.Context, uri string) error {
	return nil
}

func (m *mockRepository) Update(ctx context.Context, post *Post) error {
	return nil
}

func (m *mockRepository) UpdateVoteCounts(ctx context.Context, uri string, upvotes, downvotes int) error {
	return nil
}

func TestValidateDIDFormat(t *testing.T) {
	tests := []struct {
		name    string
		did     string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid did:plc",
			did:     "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
			wantErr: false,
		},
		{
			name:    "valid did:web",
			did:     "did:web:example.com",
			wantErr: false,
		},
		{
			name:    "valid did:web with subdomain",
			did:     "did:web:bsky.social",
			wantErr: false,
		},
		{
			name:    "valid did:web localhost",
			did:     "did:web:localhost",
			wantErr: false,
		},
		{
			name:    "invalid - missing method",
			did:     "did:",
			wantErr: true,
			errMsg:  "unsupported DID method",
		},
		{
			name:    "invalid - unsupported method",
			did:     "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK",
			wantErr: true,
			errMsg:  "unsupported DID method",
		},
		{
			name:    "invalid did:plc - empty identifier",
			did:     "did:plc:",
			wantErr: true,
			errMsg:  "missing identifier",
		},
		{
			name:    "invalid did:plc - uppercase chars",
			did:     "did:plc:UPPERCASE",
			wantErr: true,
			errMsg:  "invalid characters",
		},
		{
			name:    "invalid did:plc - numbers outside base32",
			did:     "did:plc:abc0189",
			wantErr: true,
			errMsg:  "invalid characters",
		},
		{
			name:    "invalid did:web - empty domain",
			did:     "did:web:",
			wantErr: true,
			errMsg:  "missing domain",
		},
		{
			name:    "invalid did:web - no dot in domain",
			did:     "did:web:nodot",
			wantErr: true,
			errMsg:  "invalid domain",
		},
		{
			name:    "invalid - not a DID",
			did:     "notadid",
			wantErr: true,
			errMsg:  "unsupported DID method",
		},
		{
			name:    "invalid - too long",
			did:     "did:plc:" + string(make([]byte, 2100)),
			wantErr: true,
			errMsg:  "exceeds maximum length",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDIDFormat(tt.did)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateDIDFormat(%q) = nil, want error containing %q", tt.did, tt.errMsg)
				} else if tt.errMsg != "" && !testContains(err.Error(), tt.errMsg) {
					t.Errorf("validateDIDFormat(%q) = %v, want error containing %q", tt.did, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateDIDFormat(%q) = %v, want nil", tt.did, err)
				}
			}
		})
	}
}

// helper function for contains check (named testContains to avoid conflict with package function)
func testContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestValidateGetAuthorPostsRequest(t *testing.T) {
	// Create a minimal service for testing validation
	// We only need to test the validation logic, not the full service

	tests := []struct {
		name    string
		req     GetAuthorPostsRequest
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid request - minimal",
			req: GetAuthorPostsRequest{
				ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
			},
			wantErr: false,
		},
		{
			name: "valid request - with filter",
			req: GetAuthorPostsRequest{
				ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
				Filter:   FilterPostsWithMedia,
			},
			wantErr: false,
		},
		{
			name: "valid request - with limit",
			req: GetAuthorPostsRequest{
				ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
				Limit:    25,
			},
			wantErr: false,
		},
		{
			name: "invalid - empty actor",
			req: GetAuthorPostsRequest{
				ActorDID: "",
			},
			wantErr: true,
			errMsg:  "actor is required",
		},
		{
			name: "invalid - bad DID format",
			req: GetAuthorPostsRequest{
				ActorDID: "notadid",
			},
			wantErr: true,
			errMsg:  "unsupported DID method",
		},
		{
			name: "invalid - unknown filter",
			req: GetAuthorPostsRequest{
				ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
				Filter:   "unknown_filter",
			},
			wantErr: true,
			errMsg:  "filter must be one of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create service with nil dependencies - we only test validation
			s := &postService{}
			err := s.validateGetAuthorPostsRequest(&tt.req)

			if tt.wantErr {
				if err == nil {
					t.Errorf("validateGetAuthorPostsRequest() = nil, want error containing %q", tt.errMsg)
				} else if tt.errMsg != "" && !testContains(err.Error(), tt.errMsg) {
					t.Errorf("validateGetAuthorPostsRequest() = %v, want error containing %q", err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateGetAuthorPostsRequest() = %v, want nil", err)
				}
			}
		})
	}
}

func TestValidateGetAuthorPostsRequest_DefaultsSet(t *testing.T) {
	s := &postService{}

	// Test that defaults are set
	t.Run("filter defaults to posts_with_replies", func(t *testing.T) {
		req := GetAuthorPostsRequest{
			ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
			Filter:   "", // empty
		}
		err := s.validateGetAuthorPostsRequest(&req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Filter != FilterPostsWithReplies {
			t.Errorf("Filter = %q, want %q", req.Filter, FilterPostsWithReplies)
		}
	})

	t.Run("limit defaults to 50 when 0", func(t *testing.T) {
		req := GetAuthorPostsRequest{
			ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
			Limit:    0,
		}
		err := s.validateGetAuthorPostsRequest(&req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Limit != 50 {
			t.Errorf("Limit = %d, want 50", req.Limit)
		}
	})

	t.Run("limit capped at 100", func(t *testing.T) {
		req := GetAuthorPostsRequest{
			ActorDID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz",
			Limit:    200,
		}
		err := s.validateGetAuthorPostsRequest(&req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Limit != 100 {
			t.Errorf("Limit = %d, want 100", req.Limit)
		}
	})
}

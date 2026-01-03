package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"testing"
	"time"

	"Coves/internal/core/posts"
)

func TestParseAuthorPostsCursor(t *testing.T) {
	repo := &postgresPostRepo{db: nil} // db not needed for cursor parsing

	// Helper to create a valid cursor
	makeCursor := func(timestamp, uri string) string {
		return base64.URLEncoding.EncodeToString([]byte(timestamp + "|" + uri))
	}

	validTimestamp := time.Now().Format(time.RFC3339Nano)
	validURI := "at://did:plc:test123/social.coves.community.post/abc123"

	tests := []struct {
		name       string
		cursor     *string
		wantFilter bool
		wantErr    bool
		errMsg     string
	}{
		{
			name:       "nil cursor returns empty filter",
			cursor:     nil,
			wantFilter: false,
			wantErr:    false,
		},
		{
			name:       "empty cursor returns empty filter",
			cursor:     strPtr(""),
			wantFilter: false,
			wantErr:    false,
		},
		{
			name:       "valid cursor",
			cursor:     strPtr(makeCursor(validTimestamp, validURI)),
			wantFilter: true,
			wantErr:    false,
		},
		{
			name:       "cursor too long",
			cursor:     strPtr(makeCursor(validTimestamp, string(make([]byte, 600)))),
			wantFilter: false,
			wantErr:    true,
			errMsg:     "exceeds maximum length",
		},
		{
			name:       "invalid base64",
			cursor:     strPtr("not-valid-base64!!!"),
			wantFilter: false,
			wantErr:    true,
			errMsg:     "invalid base64",
		},
		{
			name:       "missing pipe delimiter",
			cursor:     strPtr(base64.URLEncoding.EncodeToString([]byte("no-pipe-here"))),
			wantFilter: false,
			wantErr:    true,
			errMsg:     "malformed cursor format",
		},
		{
			name:       "invalid timestamp",
			cursor:     strPtr(base64.URLEncoding.EncodeToString([]byte("not-a-timestamp|" + validURI))),
			wantFilter: false,
			wantErr:    true,
			errMsg:     "invalid timestamp",
		},
		{
			name:       "invalid URI format",
			cursor:     strPtr(base64.URLEncoding.EncodeToString([]byte(validTimestamp + "|not-an-at-uri"))),
			wantFilter: false,
			wantErr:    true,
			errMsg:     "invalid URI format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, args, err := repo.parseAuthorPostsCursor(tt.cursor, 1)

			if tt.wantErr {
				if err == nil {
					t.Errorf("parseAuthorPostsCursor() = nil error, want error containing %q", tt.errMsg)
				} else if !posts.IsValidationError(err) && err != posts.ErrInvalidCursor {
					// Check if error wraps ErrInvalidCursor
					if tt.errMsg != "" && !containsStr(err.Error(), tt.errMsg) {
						t.Errorf("parseAuthorPostsCursor() error = %v, want error containing %q", err, tt.errMsg)
					}
				}
			} else {
				if err != nil {
					t.Errorf("parseAuthorPostsCursor() = %v, want nil error", err)
				}
			}

			if tt.wantFilter {
				if filter == "" {
					t.Error("parseAuthorPostsCursor() filter = empty, want non-empty filter")
				}
				if len(args) == 0 {
					t.Error("parseAuthorPostsCursor() args = empty, want non-empty args")
				}
			} else if !tt.wantErr {
				if filter != "" {
					t.Errorf("parseAuthorPostsCursor() filter = %q, want empty", filter)
				}
			}
		})
	}
}

func TestBuildAuthorPostsCursor(t *testing.T) {
	repo := &postgresPostRepo{db: nil}

	now := time.Now()
	post := &posts.PostView{
		URI:       "at://did:plc:test123/social.coves.community.post/abc123",
		CreatedAt: now,
	}

	cursor := repo.buildAuthorPostsCursor(post)

	// Decode and verify cursor
	decoded, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("Failed to decode cursor: %v", err)
	}

	// Should contain timestamp|uri
	decodedStr := string(decoded)
	if !containsStr(decodedStr, "|") {
		t.Errorf("Cursor should contain '|' delimiter, got %q", decodedStr)
	}
	if !containsStr(decodedStr, post.URI) {
		t.Errorf("Cursor should contain URI, got %q", decodedStr)
	}
	if !containsStr(decodedStr, now.Format(time.RFC3339Nano)) {
		t.Errorf("Cursor should contain timestamp, got %q", decodedStr)
	}
}

func TestBuildAndParseCursorRoundTrip(t *testing.T) {
	repo := &postgresPostRepo{db: nil}

	now := time.Now()
	post := &posts.PostView{
		URI:       "at://did:plc:test123/social.coves.community.post/abc123",
		CreatedAt: now,
	}

	// Build cursor
	cursor := repo.buildAuthorPostsCursor(post)

	// Parse it back
	filter, args, err := repo.parseAuthorPostsCursor(&cursor, 1)

	if err != nil {
		t.Fatalf("Failed to parse cursor: %v", err)
	}

	if filter == "" {
		t.Error("Expected non-empty filter")
	}

	if len(args) != 2 {
		t.Errorf("Expected 2 args, got %d", len(args))
	}

	// First arg should be timestamp string
	if ts, ok := args[0].(string); ok {
		parsedTime, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			t.Errorf("First arg is not a valid timestamp: %v", err)
		}
		if !parsedTime.Equal(now) {
			t.Errorf("Timestamp mismatch: got %v, want %v", parsedTime, now)
		}
	} else {
		t.Errorf("First arg should be string, got %T", args[0])
	}

	// Second arg should be URI
	if uri, ok := args[1].(string); ok {
		if uri != post.URI {
			t.Errorf("URI mismatch: got %q, want %q", uri, post.URI)
		}
	} else {
		t.Errorf("Second arg should be string, got %T", args[1])
	}
}

// Helper functions
func strPtr(s string) *string {
	return &s
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure the mock repository satisfies the interface
var _ posts.Repository = (*mockPostRepository)(nil)

type mockPostRepository struct {
	db *sql.DB
}

func (m *mockPostRepository) Create(ctx context.Context, post *posts.Post) error {
	return nil
}

func (m *mockPostRepository) GetByURI(ctx context.Context, uri string) (*posts.Post, error) {
	return nil, nil
}

func (m *mockPostRepository) GetByAuthor(ctx context.Context, req posts.GetAuthorPostsRequest) ([]*posts.PostView, *string, error) {
	return nil, nil, nil
}

func (m *mockPostRepository) SoftDelete(ctx context.Context, uri string) error {
	return nil
}

func (m *mockPostRepository) Update(ctx context.Context, post *posts.Post) error {
	return nil
}

func (m *mockPostRepository) UpdateVoteCounts(ctx context.Context, uri string, upvotes, downvotes int) error {
	return nil
}

package posts

import (
	"context"
	"testing"
)

const testCommunityDID = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"

func didPostURI(rkey string) string {
	return "at://" + testCommunityDID + "/" + postCollection + "/" + rkey
}

func TestParsePostURIParts(t *testing.T) {
	// parsePostURIParts is the pure splitter: it validates scheme/structure/collection/rkey
	// but does NOT check whether the authority is a DID or a handle.
	tests := []struct {
		name          string
		uri           string
		wantAuthority string
		wantRKey      string
		wantErr       bool
	}{
		{
			name:          "valid DID authority",
			uri:           didPostURI("abc123"),
			wantAuthority: testCommunityDID,
			wantRKey:      "abc123",
		},
		{
			name:          "handle authority parses (DID check happens in validatePostURI)",
			uri:           "at://c-test-community/" + postCollection + "/abc123",
			wantAuthority: "c-test-community",
			wantRKey:      "abc123",
		},
		{
			name:    "missing at:// scheme",
			uri:     testCommunityDID + "/" + postCollection + "/abc123",
			wantErr: true,
		},
		{
			name:    "too few segments",
			uri:     "at://" + testCommunityDID + "/" + postCollection,
			wantErr: true,
		},
		{
			name:    "wrong collection",
			uri:     "at://" + testCommunityDID + "/app.bsky.feed.post/abc123",
			wantErr: true,
		},
		{
			name:    "missing rkey",
			uri:     "at://" + testCommunityDID + "/" + postCollection + "/",
			wantErr: true,
		},
		{
			name:    "empty authority",
			uri:     "at:///" + postCollection + "/abc123",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authority, rkey, err := parsePostURIParts(tt.uri, "uris")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePostURIParts(%q) = nil error, want error", tt.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePostURIParts(%q) unexpected error: %v", tt.uri, err)
			}
			if authority != tt.wantAuthority {
				t.Errorf("authority = %q, want %q", authority, tt.wantAuthority)
			}
			if rkey != tt.wantRKey {
				t.Errorf("rkey = %q, want %q", rkey, tt.wantRKey)
			}
		})
	}
}

func TestValidatePostURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr bool
	}{
		{name: "valid DID-based URI", uri: didPostURI("abc123"), wantErr: false},
		{name: "handle authority is rejected", uri: "at://c-test-community/" + postCollection + "/abc123", wantErr: true},
		{name: "missing scheme", uri: testCommunityDID + "/" + postCollection + "/abc", wantErr: true},
		{name: "wrong collection", uri: "at://" + testCommunityDID + "/app.bsky.feed.post/abc", wantErr: true},
		{name: "missing rkey", uri: "at://" + testCommunityDID + "/" + postCollection + "/", wantErr: true},
		{name: "malformed DID authority", uri: "at://did:plc:UPPERCASE/" + postCollection + "/abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePostURI(tt.uri)
			if tt.wantErr && err == nil {
				t.Fatalf("validatePostURI(%q) = nil, want error", tt.uri)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validatePostURI(%q) = %v, want nil", tt.uri, err)
			}
		})
	}
}

func TestGetPosts_Validation(t *testing.T) {
	s := &postService{}

	t.Run("empty uris is rejected", func(t *testing.T) {
		_, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: nil})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for empty uris, got %v", err)
		}
	})

	t.Run("too many uris is rejected", func(t *testing.T) {
		uris := make([]string, MaxGetPostsURIs+1)
		for i := range uris {
			uris[i] = didPostURI("rkey")
		}
		_, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: uris})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for too many uris, got %v", err)
		}
	})
}

// TestGetPosts_RejectsNonCanonicalURI verifies that malformed and handle-based URIs are
// rejected with a clear error (rather than silently degrading to notFound, which would
// hide client bugs and break on community handle changes).
func TestGetPosts_RejectsNonCanonicalURI(t *testing.T) {
	s := &postService{} // repo not reached: validation happens before the fetch

	t.Run("handle-based URI is rejected", func(t *testing.T) {
		_, err := s.GetPosts(context.Background(), GetPostsRequest{
			URIs: []string{"at://c-test-community/" + postCollection + "/abc"},
		})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for handle-based URI, got %v", err)
		}
	})

	t.Run("malformed URI is rejected", func(t *testing.T) {
		_, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{"not-a-uri"}})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for malformed URI, got %v", err)
		}
	})
}

// TestGetPosts_OrderingAndNotFound verifies request order is preserved and that valid
// DID-based URIs whose post is absent come back as notFoundPost markers.
func TestGetPosts_OrderingAndNotFound(t *testing.T) {
	found := didPostURI("found1")
	missing := didPostURI("missing1")

	repo := &mockRepository{
		getViewsByURIsFunc: func(ctx context.Context, uris []string) (map[string]*PostView, error) {
			// Only the "found" URI exists in the AppView
			return map[string]*PostView{
				found: {URI: found, CID: "cid-found"},
			}, nil
		},
	}
	s := &postService{repo: repo}

	results, err := s.GetPosts(context.Background(), GetPostsRequest{
		URIs: []string{found, missing},
	})
	if err != nil {
		t.Fatalf("GetPosts returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (input order preserved), got %d", len(results))
	}

	// [0] found -> postView
	if results[0].Post == nil || results[0].NotFound != nil {
		t.Fatalf("results[0] expected a Post, got %+v", results[0])
	}
	if results[0].Post.URI != found {
		t.Errorf("results[0].Post.URI = %q, want %q", results[0].Post.URI, found)
	}

	// [1] missing (valid URI, absent in repo) -> notFoundPost echoing the requested URI
	if results[1].Post != nil || results[1].NotFound == nil {
		t.Fatalf("results[1] expected NotFound, got %+v", results[1])
	}
	if results[1].NotFound.URI != missing || !results[1].NotFound.NotFound {
		t.Errorf("results[1].NotFound = %+v, want {URI:%q, NotFound:true}", results[1].NotFound, missing)
	}
}

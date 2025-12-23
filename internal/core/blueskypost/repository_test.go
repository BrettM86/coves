package blueskypost

import (
	"testing"
)

func TestValidateATURI(t *testing.T) {
	tests := []struct {
		name    string
		atURI   string
		wantErr bool
	}{
		{
			name:    "valid AT-URI",
			atURI:   "at://did:plc:abc123/app.bsky.feed.post/xyz789",
			wantErr: false,
		},
		{
			name:    "missing at:// prefix",
			atURI:   "did:plc:abc123/app.bsky.feed.post/xyz789",
			wantErr: true,
		},
		{
			name:    "missing /app.bsky.feed.post/",
			atURI:   "at://did:plc:abc123/some.other.collection/xyz789",
			wantErr: true,
		},
		{
			name:    "empty string",
			atURI:   "",
			wantErr: true,
		},
		{
			name:    "http URL instead of AT-URI",
			atURI:   "https://bsky.app/profile/user.bsky.social/post/abc123",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateATURI(tt.atURI)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateATURI() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

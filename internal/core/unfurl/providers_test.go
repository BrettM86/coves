package unfurl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "protocol-relative URL",
			input:    "//cdn.example.com/image.jpg",
			expected: "https://cdn.example.com/image.jpg",
		},
		{
			name:     "https URL unchanged",
			input:    "https://example.com/image.jpg",
			expected: "https://example.com/image.jpg",
		},
		{
			name:     "http URL unchanged",
			input:    "http://example.com/image.jpg",
			expected: "http://example.com/image.jpg",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "protocol-relative with query params",
			input:    "//cdn.example.com/image.jpg?width=500&height=300",
			expected: "https://cdn.example.com/image.jpg?width=500&height=300",
		},
		{
			name:     "real Streamable URL",
			input:    "//cdn-cf-east.streamable.com/image/7kpdft.jpg?Expires=1762932720",
			expected: "https://cdn-cf-east.streamable.com/image/7kpdft.jpg?Expires=1762932720",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}


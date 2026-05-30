package users

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// PDS error bodies can contain server internals. NewInviteMintError must
// truncate them at construction so logs (and any future surface) stay bounded.
func TestNewInviteMintError_TruncatesLargeBody(t *testing.T) {
	huge := strings.Repeat("X", 10*1024)
	err := NewInviteMintError(500, huge)
	assert.LessOrEqual(t, len(err.Body()), 600, "truncated body should be near 512+suffix, not 10 KB")
	assert.True(t, strings.HasSuffix(err.Body(), "...[truncated]"), "must mark truncation")
}

func TestNewInviteMintError_PreservesSmallBody(t *testing.T) {
	small := "small error message"
	err := NewInviteMintError(500, small)
	assert.Equal(t, small, err.Body(), "small body must not be truncated")
}

func TestNewInviteMintError_StatusCodeAccessor(t *testing.T) {
	err := NewInviteMintError(403, "forbidden")
	assert.Equal(t, 403, err.StatusCode())
}

package postgres

// CursorSigner signs and verifies opaque cursor payloads with the same
// HMAC-SHA256 format the feed repositories use for their own cursors
// (feedRepoBase.signCursorPayload / verifyCursorPayload). The getFeed service
// wraps generator cursors in an envelope and signs it through this type, so
// there is one cursor HMAC implementation.
type CursorSigner struct {
	base feedRepoBase
}

// NewCursorSigner returns a signer keyed by secret, the configured cursor
// secret shared with the other feeds.
func NewCursorSigner(secret string) *CursorSigner {
	return &CursorSigner{base: feedRepoBase{cursorSecret: secret}}
}

// Sign returns the signed, base64-encoded cursor for payload.
func (s *CursorSigner) Sign(payload string) string {
	return s.base.signCursorPayload(payload)
}

// Verify checks a cursor's signature and returns its payload.
func (s *CursorSigner) Verify(cursor string) (string, error) {
	return s.base.verifyCursorPayload(cursor)
}

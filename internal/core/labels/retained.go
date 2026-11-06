package labels

import (
	"context"
	"errors"
	"fmt"
)

// ErrSigningKeyRequired means label publication was on once (a published
// label is stored or the label export completed) but no label signing key is
// configured.
var ErrSigningKeyRequired = errors.New("label signing key required")

// LabelPublicationReader reports whether label publication was ever on.
type LabelPublicationReader interface {
	// HasPublicationState reports whether any published label is stored or
	// the label export has completed.
	HasPublicationState(ctx context.Context) (bool, error)
}

// RequireNoLabelPublication returns ErrSigningKeyRequired when reader shows
// label publication was on once, even if that boot had no decision to export.
func RequireNoLabelPublication(ctx context.Context, reader LabelPublicationReader) error {
	published, err := reader.HasPublicationState(ctx)
	if err != nil {
		return fmt.Errorf("checking for published labels: %w", err)
	}
	if published {
		return ErrSigningKeyRequired
	}
	return nil
}

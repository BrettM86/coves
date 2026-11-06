package labels

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxQueryURIPatterns and MaxQuerySources bound how many values one
	// queryLabels request may filter on.
	MaxQueryURIPatterns = 50
	MaxQuerySources     = 50

	// MaxQueryValueBytes bounds the length of one URI pattern or source DID.
	MaxQueryValueBytes = 2048
)

// Label is one stored, signed com.atproto.label.defs#label.
type Label struct {
	Seq       int64
	Src       string
	URI       string
	CID       *string
	Val       string
	Neg       bool
	CreatedAt string
	ExpiresAt *string
	Sig       []byte
	Version   int64
	ActionID  string
	SignerKey string
}

// Query selects labels in ascending seq order.
type Query struct {
	URIPatterns []string
	Sources     []string
	Cursor      string
	Limit       int
}

// ValidateValues reports ErrInvalidQuery when q filters on too many URI
// patterns or sources, or when one of them is longer than MaxQueryValueBytes,
// is not valid UTF-8, or contains NUL.
func (q Query) ValidateValues() error {
	if len(q.URIPatterns) > MaxQueryURIPatterns {
		return fmt.Errorf("%w: at most %d uriPatterns are allowed", ErrInvalidQuery, MaxQueryURIPatterns)
	}
	if len(q.Sources) > MaxQuerySources {
		return fmt.Errorf("%w: at most %d sources are allowed", ErrInvalidQuery, MaxQuerySources)
	}
	for _, value := range q.URIPatterns {
		if err := validateQueryValue("uriPatterns", value); err != nil {
			return err
		}
	}
	for _, value := range q.Sources {
		if err := validateQueryValue("sources", value); err != nil {
			return err
		}
	}
	return nil
}

func validateQueryValue(parameter, value string) error {
	if len(value) > MaxQueryValueBytes {
		return fmt.Errorf("%w: each %s value must be at most %d bytes", ErrInvalidQuery, parameter, MaxQueryValueBytes)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: each %s value must be valid UTF-8 without NUL", ErrInvalidQuery, parameter)
	}
	return nil
}

// Page is one page of labels and the cursor that follows it.
type Page struct {
	Labels []Label
	Cursor string
}

// Reader reads published labels.
type Reader interface {
	Query(ctx context.Context, q Query) (Page, error)
}

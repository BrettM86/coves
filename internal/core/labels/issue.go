package labels

import (
	"errors"
	"fmt"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/labeling"
)

const (
	// Version is the com.atproto.label.defs#label ver this labeler signs.
	Version = 1

	// CreatedAtLayout is the cts format: RFC 3339 UTC with millisecond precision.
	CreatedAtLayout = "2006-01-02T15:04:05.000Z"

	// TakedownValue is the label a removal publishes; a restore publishes it negated.
	TakedownValue = "!takedown"

	// MaxQueryLimit and DefaultQueryLimit bound a queryLabels page.
	MaxQueryLimit     = 250
	DefaultQueryLimit = 50
)

// ErrInvalidQuery means a label query's parameters are malformed.
var ErrInvalidQuery = errors.New("invalid label query")

// Issue is one label a moderation decision publishes. The store assigns its
// cts under the label allocation lock and signs it with Key.
type Issue struct {
	SourceDID string
	URI       string
	Val       string
	Neg       bool
	ActionID  string
	Key       atcrypto.PrivateKey
	Now       time.Time
	// ExpiresAt is the label's signed exp, formatted with CreatedAtLayout;
	// nil for a label with no expiry.
	ExpiresAt *time.Time
}

// NextCreatedAt returns the cts for a label issued at now after a label whose
// cts was previous (empty for the first label): now at millisecond precision,
// or previous plus one millisecond when that is later, so cts strictly
// increases in seq order.
func NextCreatedAt(now time.Time, previous string) (time.Time, error) {
	createdAt := now.UTC().Truncate(time.Millisecond)
	if previous == "" {
		return createdAt, nil
	}
	previousTime, err := time.Parse(time.RFC3339Nano, previous)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing previous label cts %q: %w", previous, err)
	}
	if floor := previousTime.UTC().Truncate(time.Millisecond).Add(time.Millisecond); createdAt.Before(floor) {
		createdAt = floor
	}
	return createdAt, nil
}

// Sign builds the signed label for issue with the given cts.
func Sign(issue Issue, createdAt time.Time) (Label, error) {
	if issue.Key == nil {
		return Label{}, errors.New("label issue has no signing key")
	}
	publicKey, err := issue.Key.PublicKey()
	if err != nil {
		return Label{}, fmt.Errorf("deriving label signing public key: %w", err)
	}
	label := Label{
		Src:       issue.SourceDID,
		URI:       issue.URI,
		Val:       issue.Val,
		Neg:       issue.Neg,
		CreatedAt: createdAt.UTC().Format(CreatedAtLayout),
		Version:   Version,
		ActionID:  issue.ActionID,
		SignerKey: publicKey.Multibase(),
	}
	if issue.ExpiresAt != nil {
		expiresAt := issue.ExpiresAt.UTC().Format(CreatedAtLayout)
		label.ExpiresAt = &expiresAt
	}
	signed := label.unsigned()
	if err := signed.Sign(issue.Key); err != nil {
		return Label{}, fmt.Errorf("signing label: %w", err)
	}
	label.Sig = signed.Sig
	return label, nil
}

// Resign returns label signed by key, with every signed field unchanged and
// SignerKey set to key's public multibase.
func Resign(label Label, key atcrypto.PrivateKey) (Label, error) {
	publicKey, err := key.PublicKey()
	if err != nil {
		return Label{}, fmt.Errorf("deriving label signing public key: %w", err)
	}
	signed := label.unsigned()
	if err := signed.Sign(key); err != nil {
		return Label{}, fmt.Errorf("re-signing label %d: %w", label.Seq, err)
	}
	label.Sig = signed.Sig
	label.SignerKey = publicKey.Multibase()
	return label, nil
}

// unsigned is the indigo form of l without its signature. A non-negated label
// omits neg.
func (l Label) unsigned() labeling.Label {
	signed := labeling.Label{
		CID: l.CID, CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt,
		SourceDID: l.Src, URI: l.URI, Val: l.Val, Version: l.Version,
	}
	if l.Neg {
		negated := true
		signed.Negated = &negated
	}
	return signed
}

// Lexicon returns l as a com.atproto.label.defs#label.
func (l Label) Lexicon() comatproto.LabelDefs_Label {
	signed := l.unsigned()
	signed.Sig = l.Sig
	return signed.ToLexicon()
}

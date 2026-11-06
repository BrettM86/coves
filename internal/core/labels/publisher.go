// Package labels publishes this instance's signed moderation labels.
package labels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ErrSigningKeyMismatch means stored labels are signed by a key that is neither
// the configured signing key nor the declared retiring key.
var ErrSigningKeyMismatch = errors.New("label signing key does not match stored labels")

// Store is the durable label state a Publisher checks, rotates and exports at boot.
type Store interface {
	// SignerKeys returns the distinct public multibases that signed stored labels.
	SignerKeys(ctx context.Context) ([]string, error)
	// RotateSigningKey re-signs, in one transaction holding the label
	// allocation lock, every label signed by retiringKey with key, keeping
	// seq, cts and every other field, and records retiringKey as retired and
	// key as current. It returns ErrSigningKeyMismatch, changing nothing, when
	// a label is signed by a key other than key or retiringKey.
	RotateSigningKey(ctx context.Context, key atcrypto.PrivateKey, retiringKey string) error
	// RecordSigningKey records publicKey as the current signing key.
	RecordSigningKey(ctx context.Context, publicKey string) error
	// ExportActiveDecisions publishes, once ever, one current label for each
	// active instance decision by sourceDID whose subject is still indexed.
	ExportActiveDecisions(ctx context.Context, sourceDID string, key atcrypto.PrivateKey, now time.Time) error
}

// SigningKeyRecord is one label signing key this labeler has used.
type SigningKeyRecord struct {
	PublicKey string
	BeganAt   time.Time
	RetiredAt *time.Time
}

// Config is what a Publisher needs to describe and sign for this labeler.
type Config struct {
	SourceDID       string
	LabelerEndpoint string
	AtprotoKey      string
	Key             atcrypto.PrivateKey
	RetiringKey     string
}

// Publisher owns the label signing key and the labeler identity it implies.
type Publisher struct {
	sourceDID          syntax.DID
	labelerEndpoint    string
	atprotoKey         string
	key                atcrypto.PrivateKey
	keyPublicMultibase string
	retiringKey        string
}

// NewPublisher builds a Publisher from cfg. It validates cfg and touches no
// storage; Start runs the boot sequence against the store.
func NewPublisher(cfg Config) (*Publisher, error) {
	if cfg.Key == nil {
		return nil, errors.New("label publisher: a signing key is required")
	}
	sourceDID, err := syntax.ParseDID(cfg.SourceDID)
	if err != nil {
		return nil, fmt.Errorf("label publisher: source DID %q: %w", cfg.SourceDID, err)
	}
	if strings.TrimSpace(cfg.LabelerEndpoint) == "" {
		return nil, errors.New("label publisher: a labeler endpoint is required")
	}
	publicKey, err := cfg.Key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("label publisher: deriving the signing public key: %w", err)
	}
	keyPublicMultibase := publicKey.Multibase()
	if cfg.AtprotoKey != "" {
		atprotoKey, err := atcrypto.ParsePublicMultibase(cfg.AtprotoKey)
		if err != nil {
			return nil, fmt.Errorf("label publisher: #atproto key: %w", err)
		}
		if atprotoKey.Multibase() == keyPublicMultibase {
			return nil, errors.New("label publisher: the #atproto key must differ from the label signing key")
		}
	}
	if cfg.RetiringKey != "" {
		if _, err := atcrypto.ParsePublicMultibase(cfg.RetiringKey); err != nil {
			return nil, fmt.Errorf("label publisher: retiring key: %w", err)
		}
	}
	return &Publisher{
		sourceDID:          sourceDID,
		labelerEndpoint:    cfg.LabelerEndpoint,
		atprotoKey:         cfg.AtprotoKey,
		key:                cfg.Key,
		keyPublicMultibase: keyPublicMultibase,
		retiringKey:        cfg.RetiringKey,
	}, nil
}

// DIDDocument is the instance DID document this labeler serves: the instance
// #atproto key when configured, the #atproto_label key the Publisher signs
// with, and the #atproto_labeler service. It lists no #atproto_pds service:
// the static production did.json (static/.well-known/did.json.template) does
// list the instance DID's PDS, so this document is not a drop-in replacement
// for it. Production keeps serving the static file, updated by hand.
func (p *Publisher) DIDDocument() identity.DIDDocument {
	did := p.sourceDID.String()
	document := identity.DIDDocument{DID: p.sourceDID}
	if p.sourceDID.Method() == "web" {
		document.AlsoKnownAs = []string{"at://" + p.sourceDID.Identifier()}
	}
	if p.atprotoKey != "" {
		document.VerificationMethod = append(document.VerificationMethod, identity.DocVerificationMethod{
			ID:                 did + "#atproto",
			Type:               "Multikey",
			Controller:         did,
			PublicKeyMultibase: p.atprotoKey,
		})
	}
	document.VerificationMethod = append(document.VerificationMethod, identity.DocVerificationMethod{
		ID:                 did + "#atproto_label",
		Type:               "Multikey",
		Controller:         did,
		PublicKeyMultibase: p.keyPublicMultibase,
	})
	document.Service = []identity.DocService{{
		ID:              "#atproto_labeler",
		Type:            "AtprotoLabeler",
		ServiceEndpoint: p.labelerEndpoint,
	}}
	return document
}

// Start runs the boot sequence: signing key check, rotation re-sign, then the
// one-time export of active decisions.
func (p *Publisher) Start(ctx context.Context, store Store) error {
	signers, err := store.SignerKeys(ctx)
	if err != nil {
		return fmt.Errorf("label publisher: reading label signers: %w", err)
	}
	var otherSigners []string
	for _, signer := range signers {
		if signer != p.keyPublicMultibase {
			otherSigners = append(otherSigners, signer)
		}
	}
	switch {
	case len(otherSigners) == 0:
	case len(otherSigners) == 1 && otherSigners[0] == p.retiringKey:
		if err := store.RotateSigningKey(ctx, p.key, p.retiringKey); err != nil {
			return fmt.Errorf("label publisher: rotating from %s to %s: %w", p.retiringKey, p.keyPublicMultibase, err)
		}
	default:
		return fmt.Errorf("%w: configured key %s, stored labels signed by %s (set LABEL_SIGNING_KEY_RETIRING to the retiring key to rotate)",
			ErrSigningKeyMismatch, p.keyPublicMultibase, strings.Join(otherSigners, ", "))
	}
	if err := store.RecordSigningKey(ctx, p.keyPublicMultibase); err != nil {
		return fmt.Errorf("label publisher: recording the signing key: %w", err)
	}
	if err := store.ExportActiveDecisions(ctx, p.sourceDID.String(), p.key, time.Now()); err != nil {
		return fmt.Errorf("label publisher: exporting active decisions: %w", err)
	}
	return nil
}

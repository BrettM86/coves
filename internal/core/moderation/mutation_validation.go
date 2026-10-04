package moderation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/rivo/uniseg"
)

func validOpaqueField(value string) bool {
	return len(value) > 0 && len(value) <= 128 && utf8.ValidString(value)
}

func validateMutationFields(key, expectedVersion, reason, privateNote string) error {
	if err := validateMutationBaseFields(key, expectedVersion, privateNote); err != nil {
		return err
	}
	if reason == "" || len(reason) > 640 || !utf8.ValidString(reason) {
		return fmt.Errorf("%w: reason must be 1-640 UTF-8 bytes", ErrInvalidRequest)
	}
	if _, ok := removeReasons[reason]; !ok {
		return fmt.Errorf("%w: unsupported moderation reason", ErrUnsupportedReason)
	}
	return nil
}

// validateOptionalReason checks the reason of a classification mutation
// (labelContent, retractContentLabel). The reason may be empty. The doxing and
// illegal-content reasons are only for removal: content left readable under
// either would point readers at it.
func validateOptionalReason(reason string) error {
	if reason == "" {
		return nil
	}
	if len(reason) > 640 || !utf8.ValidString(reason) {
		return fmt.Errorf("%w: reason must be at most 640 UTF-8 bytes", ErrInvalidRequest)
	}
	if _, ok := removeReasons[reason]; !ok {
		return fmt.Errorf("%w: unsupported moderation reason", ErrUnsupportedReason)
	}
	if reason == illegalContentReason || reason == doxingReason {
		return fmt.Errorf("%w: the illegal-content and doxing reasons are only for removal", ErrUnsupportedReason)
	}
	return nil
}

func validateMutationBaseFields(key, expectedVersion, privateNote string) error {
	if !validOpaqueField(key) || !validOpaqueField(expectedVersion) {
		return fmt.Errorf("%w: idempotencyKey and expectedVersion must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	if !utf8.ValidString(privateNote) || len(privateNote) > 10000 || uniseg.GraphemeClusterCount(privateNote) > 1000 {
		return fmt.Errorf("%w: privateNote exceeds its length limit", ErrInvalidRequest)
	}
	return nil
}

func validContentStrongRef(ref StrongRef) bool {
	uri, err := syntax.ParseATURI(ref.URI)
	if err != nil || !uri.Authority().IsDID() || uri.RecordKey().String() == "" {
		return false
	}
	switch uri.Collection().String() {
	case CommentCollection, PostV2Collection, LegacyPostCollection:
	default:
		return false
	}
	_, err = syntax.ParseCID(ref.CID)
	return err == nil
}

// A fixed ordered tuple makes request fingerprints unambiguous even if a
// caller includes delimiters in opaque fields or private notes.
func mutationFingerprint(operation, actionID, subjectURI, subjectCID, expectedVersion, reason, labelValue, privateNote string) string {
	payload, _ := json.Marshal([8]string{
		operation, actionID, subjectURI, subjectCID, expectedVersion, reason, labelValue, privateNote,
	})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

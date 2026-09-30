package moderation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"time"
)

const (
	actionCursorVersion = byte(1)
	actionCursorPublic  = byte(0)
	actionCursorAdmin   = byte(1)
	// actionCursorHeader is the version byte, the context byte and the
	// big-endian createdAt microseconds that precede the action ID.
	actionCursorHeader = 1 + 1 + 8
	// actionCursorLabel separates action cursor MACs from every other use of
	// the cursor secret.
	actionCursorLabel = "social.coves.moderation.actionCursor\x00"
)

// actionCursor is a structurally valid cursor whose signature is not yet
// verified: the signature binds the filter digest, which is recomputed only
// after the filter targets resolve.
type actionCursor struct {
	payload   []byte
	signature []byte
	key       ActionKey
}

func actionCursorContext(admin bool) byte {
	if admin {
		return actionCursorAdmin
	}
	return actionCursorPublic
}

func writeActionFilter(hasher hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write([]byte(value))
}

func (s *service) actionFilterDigest(params ListActionsParams, actionID string, query ActionListQuery) [sha256.Size]byte {
	hasher := sha256.New()
	for _, value := range []string{
		s.config.InstanceDID,
		params.Subject, params.Collection, params.Action, params.Origin,
		params.Authority, params.Actor, params.Community, params.Since, params.Until,
		actionID,
		query.AuthorityDID, query.ActorDID, query.CommunityDID,
	} {
		writeActionFilter(hasher, value)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest
}

// actionCursorMAC signs a cursor payload (version, context, createdAt, action
// ID) together with the filter digest, which the cursor never carries.
func (s *service) actionCursorMAC(payload []byte, digest [sha256.Size]byte) []byte {
	mac := hmac.New(sha256.New, []byte(s.config.CursorSecret))
	_, _ = mac.Write([]byte(actionCursorLabel))
	_, _ = mac.Write(payload[:2])
	_, _ = mac.Write(digest[:])
	_, _ = mac.Write(payload[2:])
	return mac.Sum(nil)
}

func (s *service) signActionCursor(admin bool, digest [sha256.Size]byte, last ActionKey) string {
	payload := make([]byte, actionCursorHeader, actionCursorHeader+len(last.ID)+sha256.Size)
	payload[0] = actionCursorVersion
	payload[1] = actionCursorContext(admin)
	binary.BigEndian.PutUint64(payload[2:actionCursorHeader], uint64(last.CreatedAt.UnixMicro()))
	payload = append(payload, last.ID...)
	payload = append(payload, s.actionCursorMAC(payload, digest)...)
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeActionCursor checks a cursor's encoding, length, version and context.
// Its signature is checked later by verifyActionCursor.
func decodeActionCursor(encoded string, admin bool) (*actionCursor, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) <= actionCursorHeader+sha256.Size || len(decoded) > actionCursorHeader+maxActionIDLength+sha256.Size ||
		decoded[0] != actionCursorVersion || decoded[1] != actionCursorContext(admin) {
		return nil, fmt.Errorf("%w: malformed action cursor", ErrInvalidCursor)
	}
	payload := decoded[:len(decoded)-sha256.Size]
	return &actionCursor{
		payload:   payload,
		signature: decoded[len(payload):],
		key: ActionKey{
			CreatedAt: time.UnixMicro(int64(binary.BigEndian.Uint64(payload[2:actionCursorHeader]))).UTC(),
			ID:        string(payload[actionCursorHeader:]),
		},
	}, nil
}

// verifyActionCursor rejects a cursor not signed for this request's filter
// digest: a forged cursor, or one replayed with changed filters or with
// targets that now resolve to different DIDs.
func (s *service) verifyActionCursor(cursor *actionCursor, digest [sha256.Size]byte) error {
	if !hmac.Equal(cursor.signature, s.actionCursorMAC(cursor.payload, digest)) {
		return fmt.Errorf("%w: action cursor signature does not match the filters and resolved targets", ErrInvalidCursor)
	}
	return nil
}

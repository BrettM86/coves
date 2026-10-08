package notifications

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxEncodedCursorLength bounds the encoded cursor before decoding. The longest
// canonical cursor (year 9999, nanosecond fraction, minimum int64 ID) encodes to 68 characters.
const maxEncodedCursorLength = 96

// ErrInvalidCursor means a listNotifications cursor could not be decoded.
var ErrInvalidCursor = errors.New("invalid notification cursor")

// Cursor is the keyset position (sort_at, id) of the last listed row.
type Cursor struct {
	SortAt time.Time
	ID     int64
}

// EncodeCursor encodes a keyset position in its canonical UTC wire format.
func EncodeCursor(cursor Cursor) string {
	position := cursor.SortAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(cursor.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(position))
}

// DecodeCursor rejects malformed keyset positions with ErrInvalidCursor.
func DecodeCursor(encoded string) (Cursor, error) {
	if encoded == "" {
		return Cursor{}, fmt.Errorf("decode notification cursor: %w: empty value", ErrInvalidCursor)
	}
	if len(encoded) > maxEncodedCursorLength {
		return Cursor{}, fmt.Errorf("decode notification cursor: %w: longer than %d characters", ErrInvalidCursor, maxEncodedCursorLength)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Cursor{}, fmt.Errorf("decode notification cursor: %w: %v", ErrInvalidCursor, err)
	}
	fields := strings.SplitN(string(decoded), "|", 3)
	if len(fields) != 2 {
		return Cursor{}, fmt.Errorf("decode notification cursor: %w: expected two fields", ErrInvalidCursor)
	}
	sortAt, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return Cursor{}, fmt.Errorf("decode notification cursor timestamp: %w: %v", ErrInvalidCursor, err)
	}
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("decode notification cursor ID: %w: %v", ErrInvalidCursor, err)
	}
	return Cursor{SortAt: sortAt.UTC(), ID: id}, nil
}

package crypto

import (
	"encoding/hex"
	"errors"
	"fmt"
)

// IDSize is the length of an ID in bytes.
const IDSize = 32

// An ID identifies a piece of content in the repository: a chunk, a pack, an
// index, or a snapshot. Chunk IDs are keyed HMACs of the content, so they double
// as the deduplication key; other IDs are random.
type ID [IDSize]byte

// String returns the hexadecimal representation of the ID.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Short returns a shortened, human-friendly form of the ID for display.
func (id ID) Short() string { return hex.EncodeToString(id[:4]) }

// IsZero reports whether the ID is the zero value.
func (id ID) IsZero() bool { return id == ID{} }

// MarshalText implements encoding.TextMarshaler so IDs serialize as hex in JSON.
func (id ID) MarshalText() ([]byte, error) {
	out := make([]byte, hex.EncodedLen(IDSize))
	hex.Encode(out, id[:])
	return out, nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *ID) UnmarshalText(text []byte) error {
	parsed, err := ParseID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// ParseID decodes an ID from its hexadecimal representation.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != hex.EncodedLen(IDSize) {
		return id, fmt.Errorf("crypto: invalid ID %q: want %d hex characters, got %d",
			s, hex.EncodedLen(IDSize), len(s))
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, fmt.Errorf("crypto: invalid ID %q: %w", s, err)
	}
	return id, nil
}

// IDFromBytes builds an ID from a byte slice, which must be exactly IDSize long.
func IDFromBytes(b []byte) (ID, error) {
	var id ID
	if len(b) != IDSize {
		return id, errors.New("crypto: ID must be exactly 32 bytes")
	}
	copy(id[:], b)
	return id, nil
}

// Package pack implements gobackup's pack object format.
//
// # Why packs exist
//
// Content-defined chunking produces roughly one chunk per megabyte, and S3
// charges per request. Storing every chunk as its own object would mean one PUT
// per megabyte backed up, one GET per megabyte restored, and a LIST that has to
// page through millions of keys. Aggregating chunks into ~16 MB packs cuts the
// request count by more than an order of magnitude and makes both backup and
// restore dramatically faster, at the cost of needing an index to find a chunk
// again.
//
// # Layout
//
//	+---------------------+
//	| sealed blob 0       |
//	| sealed blob 1       |
//	| ...                 |
//	+---------------------+
//	| sealed header       |  entry per blob: type, ID, offset, length, sizes
//	+---------------------+
//	| header length (u32) |
//	| magic  (4 bytes)    |  footer, fixed 12 bytes
//	| version (u32)       |
//	+---------------------+
//
// Each blob is sealed independently, so restoring a single chunk costs one
// ranged GET covering just that blob rather than the whole pack.
//
// The header sits at the tail behind a fixed-size footer. That placement is what
// makes a pack self-describing: reading the last few kilobytes of an object is
// enough to recover the full list of chunks it contains, so the index can be
// rebuilt from the packs alone if it is ever lost.
package pack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

// TargetSize is the pack size the archiver aims for before starting a new pack.
const TargetSize = 16 << 20

// Format constants. These are part of the repository format.
const (
	magic         = "GBPK"
	formatVersion = 1
	footerSize    = 12 // header length (4) + magic (4) + version (4)
	entrySize     = 46 // type (1) + compression (1) + ID (32) + three u32 (12)
)

// MaxHeaderSize bounds the header the reader is willing to decode, so a
// corrupted length field cannot make the reader allocate arbitrary memory.
const MaxHeaderSize = 64 << 20

// BlobType distinguishes the kinds of content stored in a pack.
type BlobType uint8

const (
	// BlobData is file content.
	BlobData BlobType = 1
	// BlobTree is directory metadata. Trees live in packs alongside file data
	// so they are deduplicated and encrypted the same way: an unchanged
	// directory costs nothing to back up again.
	BlobTree BlobType = 2
)

func (t BlobType) String() string {
	switch t {
	case BlobData:
		return "data"
	case BlobTree:
		return "tree"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Valid reports whether t is a known blob type.
func (t BlobType) Valid() bool { return t == BlobData || t == BlobTree }

// Compression identifies how a blob's plaintext was compressed before sealing.
type Compression uint8

const (
	// CompressionNone means the blob was sealed as-is, either because
	// compression was disabled or because it did not help.
	CompressionNone Compression = 0
	// CompressionZstd means the blob was compressed with zstd.
	CompressionZstd Compression = 1
)

func (c Compression) String() string {
	switch c {
	case CompressionNone:
		return "none"
	case CompressionZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(c))
	}
}

// Valid reports whether c is a known compression algorithm.
func (c Compression) Valid() bool { return c == CompressionNone || c == CompressionZstd }

// An Entry describes one blob's placement inside a pack.
type Entry struct {
	Type BlobType
	ID   crypto.ID
	// Offset and Length locate the sealed bytes within the pack, which is
	// exactly the byte range a restore must fetch.
	Offset uint32
	Length uint32
	// PlainLength is the size of the original, uncompressed chunk.
	PlainLength uint32
	Compression Compression
}

// Header is the decoded list of blobs in a pack.
type Header struct {
	Entries []Entry
}

// Find returns the entry for the given blob ID.
func (h Header) Find(id crypto.ID) (Entry, bool) {
	for _, e := range h.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Errors returned when decoding malformed packs.
var (
	ErrBadMagic  = errors.New("pack: not a pack object, or it is truncated")
	ErrBadFormat = errors.New("pack: malformed pack object")
)

// blobAAD returns the additional authenticated data binding a sealed blob to its
// identity. Without it, an attacker holding the repository could move a valid
// sealed blob to a different entry and have it decrypt successfully under the
// wrong ID.
func blobAAD(t BlobType, id crypto.ID) []byte {
	aad := make([]byte, 0, 1+crypto.IDSize)
	aad = append(aad, byte(t))
	return append(aad, id[:]...)
}

// headerAAD binds a sealed header to the pack it belongs to, so headers cannot
// be swapped between packs.
func headerAAD(packID crypto.ID) []byte {
	return append([]byte("gobackup v1 pack header"), packID[:]...)
}

// encodeHeader serializes the entry list.
func encodeHeader(entries []Entry) []byte {
	buf := make([]byte, 4+len(entries)*entrySize)
	binary.LittleEndian.PutUint32(buf, uint32(len(entries)))

	off := 4
	for _, e := range entries {
		buf[off] = byte(e.Type)
		buf[off+1] = byte(e.Compression)
		copy(buf[off+2:], e.ID[:])
		binary.LittleEndian.PutUint32(buf[off+34:], e.Offset)
		binary.LittleEndian.PutUint32(buf[off+38:], e.Length)
		binary.LittleEndian.PutUint32(buf[off+42:], e.PlainLength)
		off += entrySize
	}
	return buf
}

// decodeHeader parses a header plaintext.
func decodeHeader(data []byte) (Header, error) {
	if len(data) < 4 {
		return Header{}, fmt.Errorf("%w: header is only %d bytes", ErrBadFormat, len(data))
	}
	count := binary.LittleEndian.Uint32(data)

	want := 4 + int(count)*entrySize
	if want < 0 || len(data) != want {
		return Header{}, fmt.Errorf("%w: header declares %d entries, which needs %d bytes, but it has %d",
			ErrBadFormat, count, want, len(data))
	}

	entries := make([]Entry, count)
	off := 4
	for i := range entries {
		e := Entry{
			Type:        BlobType(data[off]),
			Compression: Compression(data[off+1]),
			Offset:      binary.LittleEndian.Uint32(data[off+34:]),
			Length:      binary.LittleEndian.Uint32(data[off+38:]),
			PlainLength: binary.LittleEndian.Uint32(data[off+42:]),
		}
		copy(e.ID[:], data[off+2:off+34])
		if !e.Type.Valid() {
			return Header{}, fmt.Errorf("%w: entry %d has unknown blob type %d", ErrBadFormat, i, e.Type)
		}
		if !e.Compression.Valid() {
			return Header{}, fmt.Errorf("%w: entry %d has unknown compression %d", ErrBadFormat, i, e.Compression)
		}
		entries[i] = e
		off += entrySize
	}
	return Header{Entries: entries}, nil
}

// encodeFooter builds the fixed-size trailer that locates the header.
func encodeFooter(headerLen uint32) []byte {
	buf := make([]byte, footerSize)
	binary.LittleEndian.PutUint32(buf[0:], headerLen)
	copy(buf[4:], magic)
	binary.LittleEndian.PutUint32(buf[8:], formatVersion)
	return buf
}

// decodeFooter parses the trailer and returns the sealed header's length.
func decodeFooter(buf []byte) (uint32, error) {
	if len(buf) != footerSize {
		return 0, fmt.Errorf("%w: footer is %d bytes, want %d", ErrBadFormat, len(buf), footerSize)
	}
	if !bytes.Equal(buf[4:8], []byte(magic)) {
		return 0, fmt.Errorf("%w: footer magic is %q, want %q", ErrBadMagic, buf[4:8], magic)
	}
	if v := binary.LittleEndian.Uint32(buf[8:]); v != formatVersion {
		return 0, fmt.Errorf("pack: unsupported pack format version %d, this build understands version %d",
			v, formatVersion)
	}

	headerLen := binary.LittleEndian.Uint32(buf[0:])
	if headerLen > MaxHeaderSize {
		return 0, fmt.Errorf("%w: header claims to be %d bytes, which exceeds the %d byte limit",
			ErrBadFormat, headerLen, MaxHeaderSize)
	}
	return headerLen, nil
}

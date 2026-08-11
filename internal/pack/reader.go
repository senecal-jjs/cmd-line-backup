package pack

import (
	"context"
	"fmt"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

// tailReadSize is how many bytes are fetched from the end of a pack when
// recovering its header.
//
// A 16 MB pack of megabyte-sized chunks has a header of well under a kilobyte,
// so one 128 KB read almost always captures the footer and the entire header in
// a single request. The reader falls back to a second request for the rare pack
// full of small blobs.
const tailReadSize = 128 << 10

// LoadHeader reads a pack's header directly from storage.
//
// This is the operation that makes packs self-describing. Only the tail of the
// object is fetched, so rebuilding a lost index, or verifying that a pack holds
// what the index claims, costs a few kilobytes per pack instead of downloading
// everything.
func LoadHeader(ctx context.Context, be backend.Backend, key *crypto.Key, packID crypto.ID) (Header, error) {
	h := backend.Handle{Type: backend.TypePack, Name: packID.String()}

	info, err := be.Stat(ctx, h)
	if err != nil {
		return Header{}, err
	}
	if info.Size < footerSize {
		return Header{}, fmt.Errorf("%w: pack %s is only %d bytes", ErrBadFormat, packID.Short(), info.Size)
	}

	readLen := int64(tailReadSize)
	if readLen > info.Size {
		readLen = info.Size
	}
	tail, err := backend.LoadRange(ctx, be, h, info.Size-readLen, readLen)
	if err != nil {
		return Header{}, err
	}

	headerLen, err := decodeFooter(tail[len(tail)-footerSize:])
	if err != nil {
		return Header{}, fmt.Errorf("pack %s: %w", packID.Short(), err)
	}

	sealedHeaderEnd := info.Size - footerSize
	sealedHeaderStart := sealedHeaderEnd - int64(headerLen)
	if sealedHeaderStart < 0 {
		return Header{}, fmt.Errorf("%w: pack %s declares a %d byte header but the object is only %d bytes",
			ErrBadFormat, packID.Short(), headerLen, info.Size)
	}

	var sealedHeader []byte
	if available := int64(len(tail)) - footerSize; available >= int64(headerLen) {
		start := int64(len(tail)) - footerSize - int64(headerLen)
		sealedHeader = tail[start : int64(len(tail))-footerSize]
	} else {
		// The header is larger than the tail read; fetch it exactly.
		sealedHeader, err = backend.LoadRange(ctx, be, h, sealedHeaderStart, int64(headerLen))
		if err != nil {
			return Header{}, err
		}
	}

	return DecodeHeader(key, packID, sealedHeader)
}

// DecodeHeader decrypts and parses a sealed pack header.
func DecodeHeader(key *crypto.Key, packID crypto.ID, sealedHeader []byte) (Header, error) {
	plaintext, err := key.OpenMeta(sealedHeader, headerAAD(packID))
	if err != nil {
		return Header{}, fmt.Errorf("pack %s: opening header: %w", packID.Short(), err)
	}
	header, err := decodeHeader(plaintext)
	if err != nil {
		return Header{}, fmt.Errorf("pack %s: %w", packID.Short(), err)
	}
	return header, nil
}

// DecodeBlob decrypts, decompresses, and verifies a single blob that was read
// from a pack.
//
// The recomputed chunk ID is compared against the expected one, which catches
// silent corruption in storage and an index that points at the wrong offset.
// Tree blobs are not content-addressed by their own ID in the same way, so only
// data blobs are verified this way.
func DecodeBlob(key *crypto.Key, entry Entry, sealed []byte) ([]byte, error) {
	if len(sealed) != int(entry.Length) {
		return nil, fmt.Errorf("%w: blob %s is %d bytes, but the index says %d",
			ErrBadFormat, entry.ID.Short(), len(sealed), entry.Length)
	}

	stored, err := key.OpenData(sealed, blobAAD(entry.Type, entry.ID))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", entry.ID.Short(), err)
	}

	plaintext, err := decompress(stored, entry.Compression, int(entry.PlainLength))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", entry.ID.Short(), err)
	}

	if !key.VerifyChunkID(entry.ID, plaintext) {
		return nil, fmt.Errorf("blob %s: content does not match its ID; the repository is corrupt",
			entry.ID.Short())
	}
	return plaintext, nil
}

// LoadBlob fetches and decodes a single blob with one ranged read.
func LoadBlob(ctx context.Context, be backend.Backend, key *crypto.Key, packID crypto.ID, entry Entry) ([]byte, error) {
	h := backend.Handle{Type: backend.TypePack, Name: packID.String()}

	sealed, err := backend.LoadRange(ctx, be, h, int64(entry.Offset), int64(entry.Length))
	if err != nil {
		return nil, err
	}
	return DecodeBlob(key, entry, sealed)
}

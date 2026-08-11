package pack

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// compressionMinGain is the fraction of the original size that compression must
// save before the compressed form is kept.
//
// Backups are full of data that is already compressed -- photos, video,
// archives, encrypted files. On that input zstd spends CPU to produce output the
// same size or slightly larger. Requiring a real saving means such chunks are
// stored verbatim and restore skips the decompression step entirely.
const compressionMinGain = 0.05

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
	encoderErr  error

	decoderOnce sync.Once
	decoder     *zstd.Decoder
	decoderErr  error
)

// getEncoder returns a shared zstd encoder. Encoders are expensive to construct
// but safe for concurrent use through EncodeAll, so one is shared process-wide.
func getEncoder() (*zstd.Encoder, error) {
	encoderOnce.Do(func() {
		encoder, encoderErr = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
		)
	})
	return encoder, encoderErr
}

func getDecoder() (*zstd.Decoder, error) {
	decoderOnce.Do(func() {
		decoder, decoderErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
	return decoder, decoderErr
}

// compress applies the requested algorithm, falling back to storing the data
// uncompressed when compression does not pay off. It returns the bytes to store
// and the algorithm actually used.
func compress(plaintext []byte, want Compression) ([]byte, Compression, error) {
	if want == CompressionNone || len(plaintext) == 0 {
		return plaintext, CompressionNone, nil
	}

	enc, err := getEncoder()
	if err != nil {
		return nil, 0, fmt.Errorf("pack: initializing compressor: %w", err)
	}

	compressed := enc.EncodeAll(plaintext, nil)
	if float64(len(compressed)) > float64(len(plaintext))*(1-compressionMinGain) {
		return plaintext, CompressionNone, nil
	}
	return compressed, CompressionZstd, nil
}

// decompress reverses compress. plainLength is the expected output size, taken
// from the pack header, and is checked so that a corrupted or malicious blob
// cannot expand without bound.
func decompress(data []byte, c Compression, plainLength int) ([]byte, error) {
	switch c {
	case CompressionNone:
		if len(data) != plainLength {
			return nil, fmt.Errorf("%w: uncompressed blob is %d bytes but the header says %d",
				ErrBadFormat, len(data), plainLength)
		}
		return data, nil

	case CompressionZstd:
		dec, err := getDecoder()
		if err != nil {
			return nil, fmt.Errorf("pack: initializing decompressor: %w", err)
		}
		out, err := dec.DecodeAll(data, make([]byte, 0, plainLength))
		if err != nil {
			return nil, fmt.Errorf("pack: decompressing blob: %w", err)
		}
		if len(out) != plainLength {
			return nil, fmt.Errorf("%w: blob decompressed to %d bytes but the header says %d",
				ErrBadFormat, len(out), plainLength)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("%w: unknown compression %d", ErrBadFormat, c)
	}
}

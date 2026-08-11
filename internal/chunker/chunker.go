// Package chunker implements FastCDC, a content-defined chunking algorithm.
//
// Content-defined chunking splits a byte stream at boundaries determined by the
// content itself rather than at fixed offsets. Inserting or removing bytes in
// the middle of a file therefore only disturbs the chunks around the edit; every
// later chunk keeps its original boundaries and stays deduplicable.
//
// The implementation follows Xia et al., "FastCDC: a Fast and Efficient
// Content-Defined Chunking Approach for Data Deduplication" (USENIX ATC '16),
// including the two optimizations that give FastCDC its speed:
//
//   - a gear hash, which updates the rolling fingerprint with a single shift and
//     add per byte;
//   - normalized chunking, which uses a strict mask before the target average
//     size and a lenient mask after it, concentrating the chunk-size
//     distribution around the average and reducing the number of chunks that hit
//     the minimum or maximum size limits.
//
// Boundaries produced by this package are part of the on-disk format: changing
// the gear table, the masks, or the default sizes would silently stop new
// backups from deduplicating against existing repositories. The golden-vector
// tests exist to catch exactly that.
package chunker

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
)

// Default chunk size parameters. The average is the target; the actual mean
// chunk size for random data lands somewhat above it because of the minimum
// size floor.
const (
	DefaultMinSize = 256 * 1024
	DefaultAvgSize = 1024 * 1024
	DefaultMaxSize = 4 * 1024 * 1024
)

// normalization controls how much stricter the pre-average mask is than the
// post-average one, in bits. Level 2 is the value the FastCDC paper found best.
const normalization = 2

// Config holds the chunk size parameters. The zero value is invalid; use
// DefaultConfig.
type Config struct {
	MinSize int // no chunk is smaller than this, except the final one
	AvgSize int // target average chunk size; must be a power of two
	MaxSize int // no chunk is larger than this
}

// DefaultConfig returns the standard chunk size parameters.
func DefaultConfig() Config {
	return Config{MinSize: DefaultMinSize, AvgSize: DefaultAvgSize, MaxSize: DefaultMaxSize}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	switch {
	case c.MinSize <= 0:
		return errors.New("chunker: MinSize must be positive")
	case c.AvgSize <= c.MinSize:
		return errors.New("chunker: AvgSize must be greater than MinSize")
	case c.MaxSize <= c.AvgSize:
		return errors.New("chunker: MaxSize must be greater than AvgSize")
	case bits.OnesCount64(uint64(c.AvgSize)) != 1:
		return fmt.Errorf("chunker: AvgSize must be a power of two, got %d", c.AvgSize)
	}
	// Each mask bit costs a factor of two in expected chunk length, and the
	// lenient mask must keep at least one bit to remain a probabilistic test.
	if bits.TrailingZeros64(uint64(c.AvgSize)) <= normalization {
		return fmt.Errorf("chunker: AvgSize %d is too small for normalized chunking", c.AvgSize)
	}
	return nil
}

// Chunk is a single content-defined chunk of the input stream.
//
// Data aliases the chunker's internal buffer and is only valid until the next
// call to Next. Callers that retain a chunk beyond that point must copy it.
type Chunk struct {
	Offset int64 // byte offset of the chunk within the stream
	Data   []byte
}

// Length returns the size of the chunk in bytes.
func (c Chunk) Length() int { return len(c.Data) }

// Chunker splits an io.Reader into content-defined chunks.
type Chunker struct {
	cfg   Config
	rd    io.Reader
	maskS uint64 // strict mask, used before AvgSize bytes
	maskL uint64 // lenient mask, used after AvgSize bytes

	buf   []byte
	start int   // offset of unconsumed data within buf
	end   int   // offset just past valid data within buf
	pos   int64 // stream offset of buf[start]
	eof   bool
}

// New returns a Chunker that splits r using cfg.
func New(r io.Reader, cfg Config) (*Chunker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	avgBits := bits.TrailingZeros64(uint64(cfg.AvgSize))
	return &Chunker{
		cfg:   cfg,
		rd:    r,
		maskS: deriveMask(avgBits + normalization),
		maskL: deriveMask(avgBits - normalization),
		// Holding twice the maximum chunk size means the buffer only has to be
		// compacted once per MaxSize bytes consumed, so the amortized cost of
		// moving unconsumed bytes to the front is well under one byte moved per
		// byte chunked.
		buf: make([]byte, 2*cfg.MaxSize),
	}, nil
}

// Next returns the next chunk, or io.EOF when the stream is exhausted.
func (c *Chunker) Next() (Chunk, error) {
	if err := c.fill(); err != nil {
		return Chunk{}, err
	}
	avail := c.end - c.start
	if avail == 0 {
		return Chunk{}, io.EOF
	}

	n := c.cut(c.buf[c.start:c.end])
	chunk := Chunk{Offset: c.pos, Data: c.buf[c.start : c.start+n]}
	c.start += n
	c.pos += int64(n)
	return chunk, nil
}

// fill ensures at least MaxSize bytes are buffered, unless the stream ended
// first, compacting the buffer when necessary.
func (c *Chunker) fill() error {
	if c.eof || c.end-c.start >= c.cfg.MaxSize {
		return nil
	}

	if c.start > 0 {
		c.end = copy(c.buf, c.buf[c.start:c.end])
		c.start = 0
	}

	for c.end < len(c.buf) && c.end-c.start < c.cfg.MaxSize {
		n, err := c.rd.Read(c.buf[c.end:])
		c.end += n
		if err == io.EOF {
			c.eof = true
			break
		}
		if err != nil {
			// Fail fast rather than emitting the chunks buffered so far: a
			// file that cannot be read in full will be abandoned by the
			// caller anyway, and storing its partial chunks would waste
			// space in the repository.
			return fmt.Errorf("chunker: reading input: %w", err)
		}
	}
	return nil
}

// cut returns the length of the next chunk within data.
//
// The fingerprint deliberately starts accumulating at MinSize: bytes before the
// minimum chunk size can never produce a boundary, so hashing them would be
// wasted work. Because the gear hash's effective window is the preceding ~64
// bytes, skipping ahead does not weaken the content-defined property.
func (c *Chunker) cut(data []byte) int {
	n := len(data)
	if n <= c.cfg.MinSize {
		return n
	}
	if n > c.cfg.MaxSize {
		n = c.cfg.MaxSize
	}
	normal := c.cfg.AvgSize
	if normal > n {
		normal = n
	}

	var fp uint64
	i := c.cfg.MinSize

	// Below the target average, demand a rarer fingerprint pattern so that
	// short chunks are less likely.
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&c.maskS == 0 {
			return i + 1
		}
	}
	// Above it, accept a more common pattern so that long chunks are cut before
	// they reach MaxSize.
	for ; i < n; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}

// deriveMask returns a mask with the requested number of bits set.
//
// Only the high half of the gear fingerprint is usable. In a gear hash a byte's
// contribution is shifted left once per subsequent byte, so bit j of the
// fingerprint depends on at most the last j+1 bytes; masking low bits would
// shrink the content window to a handful of bytes and make boundaries fragile.
// Bits are therefore taken from bit 63 downwards, spaced two apart so they stay
// spread across the well-mixed region.
func deriveMask(n int) uint64 {
	var m uint64
	for i := 0; i < n; i++ {
		m |= 1 << uint(63-2*i)
	}
	return m
}

// gear is the FastCDC substitution table: one random 64-bit value per possible
// input byte.
var gear = func() [256]uint64 {
	// Generated with splitmix64 from a fixed seed rather than math/rand so the
	// table is identical on every platform and Go version. These values are
	// part of the repository format.
	var t [256]uint64
	state := uint64(0x3C79AC492BA7B653)
	for i := range t {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		t[i] = z ^ (z >> 31)
	}
	return t
}()

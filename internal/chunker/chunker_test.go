package chunker

import (
	"bytes"
	"errors"
	"io"
	"math/bits"
	"testing"
)

// testConfig uses small sizes so tests stay fast while exercising the same code
// paths as the production configuration.
func testConfig() Config {
	return Config{MinSize: 512, AvgSize: 2048, MaxSize: 8192}
}

// deterministicData returns pseudo-random but reproducible bytes, so tests do
// not depend on math/rand's algorithm remaining stable.
func deterministicData(n int, seed uint64) []byte {
	out := make([]byte, n)
	state := seed
	for i := 0; i < n; i += 8 {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		for j := 0; j < 8 && i+j < n; j++ {
			out[i+j] = byte(z >> (8 * j))
		}
	}
	return out
}

// chunkAll splits data and returns every chunk's contents.
func chunkAll(t *testing.T, data []byte, cfg Config) [][]byte {
	t.Helper()
	c, err := New(bytes.NewReader(data), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var chunks [][]byte
	var offset int64
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.Offset != offset {
			t.Fatalf("chunk offset = %d, want %d", chunk.Offset, offset)
		}
		offset += int64(chunk.Length())
		chunks = append(chunks, bytes.Clone(chunk.Data))
	}
	return chunks
}

func TestRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 511, 512, 513, 8192, 100000, 1 << 20} {
		data := deterministicData(size, 42)
		chunks := chunkAll(t, data, testConfig())

		var got []byte
		for _, c := range chunks {
			got = append(got, c...)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("size %d: reassembled data does not match input", size)
		}
	}
}

func TestEmptyInputProducesNoChunks(t *testing.T) {
	if chunks := chunkAll(t, nil, testConfig()); len(chunks) != 0 {
		t.Errorf("got %d chunks for empty input, want 0", len(chunks))
	}
}

func TestSizeBounds(t *testing.T) {
	cfg := testConfig()
	data := deterministicData(1<<20, 7)
	chunks := chunkAll(t, data, cfg)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > cfg.MaxSize {
			t.Errorf("chunk %d has size %d, exceeds MaxSize %d", i, len(c), cfg.MaxSize)
		}
		// Only the final chunk may be shorter than the minimum, because the
		// stream simply ran out.
		if i < len(chunks)-1 && len(c) < cfg.MinSize {
			t.Errorf("chunk %d has size %d, below MinSize %d", i, len(c), cfg.MinSize)
		}
	}
}

func TestAverageSizeNearTarget(t *testing.T) {
	cfg := DefaultConfig()
	data := deterministicData(64<<20, 99)
	chunks := chunkAll(t, data, cfg)

	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	avg := len(data) / len(chunks)
	// Normalized chunking plus the minimum-size floor pulls the mean somewhat
	// above the target, so this asserts the right order of magnitude rather
	// than an exact figure.
	if avg < cfg.AvgSize/2 || avg > 2*cfg.AvgSize {
		t.Errorf("average chunk size = %d, want within [%d, %d]", avg, cfg.AvgSize/2, 2*cfg.AvgSize)
	}
}

func TestDeterministic(t *testing.T) {
	data := deterministicData(1<<20, 3)
	first := chunkAll(t, data, testConfig())
	second := chunkAll(t, data, testConfig())

	if len(first) != len(second) {
		t.Fatalf("chunk counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

// TestChunkingIsIndependentOfReadSize guards against the buffering logic
// influencing boundaries: a reader that dribbles out a few bytes at a time must
// produce exactly the same chunks as one that returns everything at once.
func TestChunkingIsIndependentOfReadSize(t *testing.T) {
	data := deterministicData(1<<20, 11)
	want := chunkAll(t, data, testConfig())

	c, err := New(&iotest{data: data, max: 7}, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var got [][]byte
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, bytes.Clone(chunk.Data))
	}

	if len(got) != len(want) {
		t.Fatalf("chunk counts differ: %d with small reads vs %d with full reads", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("chunk %d differs when input is read in small pieces", i)
		}
	}
}

// iotest is a reader that returns at most max bytes per Read call.
type iotest struct {
	data []byte
	max  int
	off  int
}

func (r *iotest) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.max {
		n = r.max
	}
	if n > len(r.data)-r.off {
		n = len(r.data) - r.off
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

// TestShiftResistance is the property that justifies content-defined chunking
// over fixed-size blocks: prepending bytes must not invalidate the whole
// stream. With fixed-size chunking every chunk would change; here all but a few
// chunks near the edit must survive and stay deduplicable.
func TestShiftResistance(t *testing.T) {
	cfg := testConfig()
	original := deterministicData(1<<20, 5)
	shifted := append(deterministicData(100, 123), original...)

	before := chunkAll(t, original, cfg)
	after := chunkAll(t, shifted, cfg)

	index := make(map[string]bool, len(before))
	for _, c := range before {
		index[string(c)] = true
	}

	var shared int
	for _, c := range after {
		if index[string(c)] {
			shared++
		}
	}

	// Only the chunks overlapping the insertion point should be lost.
	minShared := len(before) * 9 / 10
	if shared < minShared {
		t.Errorf("only %d of %d chunks survived a 100-byte prefix insertion, want at least %d",
			shared, len(before), minShared)
	}
}

// TestMiddleEditIsLocalized checks that an edit in the middle of a stream only
// disturbs nearby chunks, leaving both the preceding and following chunks
// intact.
func TestMiddleEditIsLocalized(t *testing.T) {
	cfg := testConfig()
	original := deterministicData(1<<20, 17)

	mid := len(original) / 2
	edited := make([]byte, 0, len(original)+50)
	edited = append(edited, original[:mid]...)
	edited = append(edited, deterministicData(50, 555)...)
	edited = append(edited, original[mid:]...)

	before := chunkAll(t, original, cfg)
	after := chunkAll(t, edited, cfg)

	index := make(map[string]bool, len(before))
	for _, c := range before {
		index[string(c)] = true
	}
	var shared int
	for _, c := range after {
		if index[string(c)] {
			shared++
		}
	}

	minShared := len(before) * 9 / 10
	if shared < minShared {
		t.Errorf("only %d of %d chunks survived a mid-stream insertion, want at least %d",
			shared, len(before), minShared)
	}
}

// TestGoldenBoundaries pins the chunk boundaries produced for a fixed input.
//
// These offsets are part of the repository format: if a change to the gear
// table, the masks, or the cut logic moves them, existing backups stop
// deduplicating against new ones. A failure here means the format changed and
// needs a version bump, not that the expectations should be updated.
func TestGoldenBoundaries(t *testing.T) {
	data := deterministicData(1<<18, 20240101)
	chunks := chunkAll(t, data, testConfig())

	var got []int
	for _, c := range chunks {
		got = append(got, len(c))
	}

	want := []int{
		2050, 2799, 2082, 2151, 2354, 2658, 2190, 2063, 2671, 2690,
		2166, 2160, 3919, 2450, 2411, 2049, 2363, 2233, 2928, 1244,
		3957, 932, 3723, 2633, 2361, 2628, 5210, 2307, 2085, 2067,
		2290, 2355, 2096, 921, 2862, 2634, 2148, 2212, 2443, 2432,
		2163, 3292, 2881, 1626, 2299, 2580, 2289, 3135, 2482, 3027,
		2495, 1626, 523, 675, 2116, 2478, 2077, 3355, 1476, 925,
		2234, 2417, 2504, 1304, 3297, 2498, 3163, 834, 873, 3276,
		4006, 2519, 2801, 2353, 2416, 2781, 2676, 2321, 2166, 1719,
		2574, 3022, 3147, 2152, 2258, 2108, 625, 2359, 2444, 2336,
		2338, 5147, 3212, 2203, 1808, 2610, 2247, 3584, 614, 3257,
		2478, 2405, 2220, 2514, 2762, 791, 3169, 2191, 1145, 2396,
		423,
	}

	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d\nsizes: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk %d has size %d, want %d\nfull sizes: %v", i, got[i], want[i], got)
		}
	}
}

func TestDeriveMaskBitCount(t *testing.T) {
	for n := 1; n <= 30; n++ {
		if c := bits.OnesCount64(deriveMask(n)); c != n {
			t.Errorf("deriveMask(%d) has %d bits set, want %d", n, c, n)
		}
	}
}

// TestLenientMaskIsSubsetOfStrict documents the normalization invariant: any
// position that satisfies the strict mask also satisfies the lenient one, so
// switching masks at the average size can only make boundaries more likely, not
// less.
func TestLenientMaskIsSubsetOfStrict(t *testing.T) {
	c, err := New(bytes.NewReader(nil), DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.maskL&^c.maskS != 0 {
		t.Errorf("lenient mask %#x is not a subset of strict mask %#x", c.maskL, c.maskS)
	}
	if bits.OnesCount64(c.maskS) <= bits.OnesCount64(c.maskL) {
		t.Errorf("strict mask must have more bits: %d vs %d",
			bits.OnesCount64(c.maskS), bits.OnesCount64(c.maskL))
	}
}

func TestGearTableIsWellDistributed(t *testing.T) {
	seen := make(map[uint64]bool, len(gear))
	for _, v := range gear {
		if seen[v] {
			t.Fatalf("gear table contains duplicate value %#x", v)
		}
		seen[v] = true
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"default", DefaultConfig(), false},
		{"zero", Config{}, true},
		{"min not positive", Config{MinSize: 0, AvgSize: 1024, MaxSize: 4096}, true},
		{"avg below min", Config{MinSize: 2048, AvgSize: 1024, MaxSize: 4096}, true},
		{"max below avg", Config{MinSize: 256, AvgSize: 4096, MaxSize: 1024}, true},
		{"avg not power of two", Config{MinSize: 256, AvgSize: 3000, MaxSize: 8192}, true},
		{"avg too small for normalization", Config{MinSize: 1, AvgSize: 4, MaxSize: 8}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	if _, err := New(bytes.NewReader(nil), Config{}); err == nil {
		t.Error("New accepted an invalid config")
	}
}

// TestReadErrorFailsFast checks that a mid-stream read failure is reported
// rather than being silently treated as end of input, which would otherwise
// store a truncated file as if it were complete.
func TestReadErrorFailsFast(t *testing.T) {
	data := deterministicData(4096, 1)
	c, err := New(&errReader{data: data}, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var lastErr error
	for {
		if _, err := c.Next(); err != nil {
			lastErr = err
			break
		}
	}

	if lastErr == io.EOF {
		t.Fatal("got io.EOF, want the underlying read error")
	}
	if !errors.Is(lastErr, io.ErrUnexpectedEOF) {
		t.Errorf("got error %v, want it to wrap io.ErrUnexpectedEOF", lastErr)
	}
}

// errReader returns all of its data and then a non-EOF error.
type errReader struct {
	data []byte
	off  int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func BenchmarkChunker(b *testing.B) {
	data := deterministicData(32<<20, 1)
	cfg := DefaultConfig()

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := New(bytes.NewReader(data), cfg)
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := c.Next(); err == io.EOF {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// Package backendtest provides a conformance suite that every backend.Backend
// implementation must pass.
//
// Sharing one suite between the local and S3 backends is what keeps their
// semantics interchangeable: a repository written by one must be readable by the
// other, and the layers above them are written against a single set of rules for
// ranged reads, missing objects, and listing.
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
)

// Factory creates a fresh, empty backend for a single test.
type Factory func(t *testing.T) backend.Backend

// RunSuite runs the full conformance suite against the backend produced by newBackend.
func RunSuite(t *testing.T, newBackend Factory) {
	t.Helper()

	tests := []struct {
		name string
		fn   func(t *testing.T, b backend.Backend)
	}{
		{"SaveLoadRoundTrip", testSaveLoadRoundTrip},
		{"SaveEmptyObject", testSaveEmptyObject},
		{"SaveLargeObject", testSaveLargeObject},
		{"SaveRejectsWrongSize", testSaveRejectsWrongSize},
		{"SaveIsIdempotent", testSaveIsIdempotent},
		{"RangedLoad", testRangedLoad},
		{"RangedLoadOutOfBounds", testRangedLoadOutOfBounds},
		{"LoadMissing", testLoadMissing},
		{"Stat", testStat},
		{"StatMissing", testStatMissing},
		{"List", testList},
		{"ListEmpty", testListEmpty},
		{"ListStopsOnError", testListStopsOnError},
		{"Remove", testRemove},
		{"RemoveMissing", testRemoveMissing},
		{"ConfigObject", testConfigObject},
		{"InvalidHandles", testInvalidHandles},
		{"ConcurrentSaveAndLoad", testConcurrentSaveAndLoad},
		{"AllFileTypes", testAllFileTypes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBackend(t)
			defer b.Close()
			tt.fn(t, b)
		})
	}
}

func packHandle(name string) backend.Handle {
	return backend.Handle{Type: backend.TypePack, Name: name}
}

func save(t *testing.T, b backend.Backend, h backend.Handle, data []byte) {
	t.Helper()
	if err := b.Save(context.Background(), h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Save(%s): %v", h, err)
	}
}

func testSaveLoadRoundTrip(t *testing.T, b backend.Backend) {
	h := packHandle("abcdef0123456789")
	data := []byte("the quick brown fox jumps over the lazy dog")
	save(t, b, h, data)

	got, err := backend.LoadAll(context.Background(), b, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("loaded %q, want %q", got, data)
	}
}

func testSaveEmptyObject(t *testing.T, b backend.Backend) {
	h := packHandle("empty00000000000")
	save(t, b, h, nil)

	got, err := backend.LoadAll(context.Background(), b, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("loaded %d bytes, want 0", len(got))
	}

	info, err := b.Stat(context.Background(), h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 0 {
		t.Errorf("Stat reports size %d, want 0", info.Size)
	}
}

// testSaveLargeObject exercises the multipart upload path in backends that have
// one; packs are megabytes in size in real use.
func testSaveLargeObject(t *testing.T, b backend.Backend) {
	h := packHandle("large00000000000")
	data := make([]byte, 9<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	save(t, b, h, data)

	got, err := backend.LoadAll(context.Background(), b, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("large object did not survive the round trip")
	}
}

// testSaveRejectsWrongSize checks that a truncated stream is caught rather than
// silently stored, which would corrupt a pack.
func testSaveRejectsWrongSize(t *testing.T, b backend.Backend) {
	h := packHandle("wrongsize0000000")
	data := []byte("twenty-four bytes-------")

	if err := b.Save(context.Background(), h, bytes.NewReader(data), int64(len(data))+10); err == nil {
		t.Error("Save accepted a stream shorter than the declared size")
	}
}

func testSaveIsIdempotent(t *testing.T, b backend.Backend) {
	h := packHandle("idempotent000000")
	data := []byte("written twice")

	save(t, b, h, data)
	save(t, b, h, data)

	got, err := backend.LoadAll(context.Background(), b, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("loaded %q, want %q", got, data)
	}
}

// testRangedLoad covers the access pattern restore depends on: pulling
// individual chunks out of the middle of a pack without downloading the rest.
func testRangedLoad(t *testing.T, b backend.Backend) {
	h := packHandle("ranged0000000000")
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	save(t, b, h, data)

	tests := []struct {
		offset, length int64
		want           string
	}{
		{0, 10, "0123456789"},
		{10, 6, "abcdef"},
		{int64(len(data)) - 4, 4, "wxyz"},
		{5, 1, "5"},
		{0, int64(len(data)), string(data)},
		{20, 0, "klmnopqrstuvwxyz"}, // length 0 means "to the end"
	}
	for _, tt := range tests {
		rd, err := b.Load(context.Background(), h, tt.offset, tt.length)
		if err != nil {
			t.Fatalf("Load(offset=%d, length=%d): %v", tt.offset, tt.length, err)
		}
		got, err := io.ReadAll(rd)
		rd.Close()
		if err != nil {
			t.Fatalf("reading range: %v", err)
		}
		if string(got) != tt.want {
			t.Errorf("Load(offset=%d, length=%d) = %q, want %q", tt.offset, tt.length, got, tt.want)
		}
	}
}

func testRangedLoadOutOfBounds(t *testing.T, b backend.Backend) {
	h := packHandle("bounds0000000000")
	save(t, b, h, []byte("0123456789"))

	tests := []struct{ offset, length int64 }{
		{0, 11},
		{5, 10},
		{11, 1},
		{-1, 5},
		{0, -1},
	}
	for _, tt := range tests {
		rd, err := b.Load(context.Background(), h, tt.offset, tt.length)
		if err == nil {
			rd.Close()
			t.Errorf("Load(offset=%d, length=%d) succeeded, want an error", tt.offset, tt.length)
		}
	}
}

func testLoadMissing(t *testing.T, b backend.Backend) {
	_, err := b.Load(context.Background(), packHandle("missing000000000"), 0, 0)
	if !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Load on a missing object returned %v, want ErrNotFound", err)
	}
}

func testStat(t *testing.T, b backend.Backend) {
	h := packHandle("stat000000000000")
	data := []byte("some bytes here")
	save(t, b, h, data)

	info, err := b.Stat(context.Background(), h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", info.Size, len(data))
	}
	if info.Handle != h {
		t.Errorf("Handle = %v, want %v", info.Handle, h)
	}
}

func testStatMissing(t *testing.T, b backend.Backend) {
	_, err := b.Stat(context.Background(), packHandle("missing000000000"))
	if !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stat on a missing object returned %v, want ErrNotFound", err)
	}
}

func testList(t *testing.T, b backend.Backend) {
	// More than one shard prefix, so sharded layouts are exercised.
	names := []string{"00aaaaaaaaaaaaaa", "01bbbbbbbbbbbbbb", "ffcccccccccccccc"}
	want := make(map[string]int64, len(names))
	for i, name := range names {
		data := bytes.Repeat([]byte{byte(i)}, i+1)
		save(t, b, packHandle(name), data)
		want[name] = int64(len(data))
	}

	// An object of a different type must not appear in the pack listing.
	save(t, b, backend.Handle{Type: backend.TypeSnapshot, Name: "snap000000000000"}, []byte("snapshot"))

	got := make(map[string]int64)
	err := b.List(context.Background(), backend.TypePack, func(fi backend.FileInfo) error {
		if fi.Type != backend.TypePack {
			t.Errorf("List(pack) yielded type %q", fi.Type)
		}
		got[fi.Name] = fi.Size
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("List returned %d objects, want %d: %v", len(got), len(want), got)
	}
	for name, size := range want {
		if got[name] != size {
			t.Errorf("List reported size %d for %s, want %d", got[name], name, size)
		}
	}
}

func testListEmpty(t *testing.T, b backend.Backend) {
	for _, ft := range []backend.FileType{
		backend.TypePack, backend.TypeIndex, backend.TypeSnapshot, backend.TypeKey, backend.TypeConfig,
	} {
		var count int
		err := b.List(context.Background(), ft, func(backend.FileInfo) error {
			count++
			return nil
		})
		// Listing a type that has never been written must succeed and yield
		// nothing, not fail because no directory or key prefix exists yet.
		if err != nil {
			t.Errorf("List(%s) on an empty repository: %v", ft, err)
		}
		if count != 0 {
			t.Errorf("List(%s) yielded %d objects, want 0", ft, count)
		}
	}
}

func testListStopsOnError(t *testing.T, b backend.Backend) {
	for i := 0; i < 5; i++ {
		save(t, b, packHandle(fmt.Sprintf("%02daaaaaaaaaaaaaa", i)), []byte("x"))
	}

	sentinel := errors.New("stop")
	var count int
	err := b.List(context.Background(), backend.TypePack, func(backend.FileInfo) error {
		count++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("List returned %v, want the callback's error", err)
	}
	if count != 1 {
		t.Errorf("callback ran %d times after returning an error, want 1", count)
	}
}

func testRemove(t *testing.T, b backend.Backend) {
	h := packHandle("removeme00000000")
	save(t, b, h, []byte("transient"))

	if err := b.Remove(context.Background(), h); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := b.Stat(context.Background(), h); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("object still present after Remove: %v", err)
	}
}

func testRemoveMissing(t *testing.T, b backend.Backend) {
	err := b.Remove(context.Background(), packHandle("neverexisted0000"))
	if !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Remove on a missing object returned %v, want ErrNotFound", err)
	}
}

func testConfigObject(t *testing.T, b backend.Backend) {
	h := backend.Handle{Type: backend.TypeConfig}
	data := []byte("repository configuration")
	save(t, b, h, data)

	got, err := backend.LoadAll(context.Background(), b, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("loaded %q, want %q", got, data)
	}

	var count int
	if err := b.List(context.Background(), backend.TypeConfig, func(backend.FileInfo) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("List(config): %v", err)
	}
	if count != 1 {
		t.Errorf("List(config) yielded %d objects, want 1", count)
	}
}

func testInvalidHandles(t *testing.T, b backend.Backend) {
	ctx := context.Background()
	bad := []backend.Handle{
		{Type: "bogus", Name: "x"},
		{Type: backend.TypePack},                      // missing name
		{Type: backend.TypeConfig, Name: "x"},         // config takes no name
		{Type: backend.TypePack, Name: "../escape"},   // path traversal
		{Type: backend.TypePack, Name: "nested/name"}, // separator
		{Type: backend.TypePack, Name: ".."},          // parent directory
		{Type: backend.TypePack, Name: "back\\slash"}, // Windows separator
	}
	for _, h := range bad {
		if err := b.Save(ctx, h, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("Save accepted invalid handle %+v", h)
		}
		if _, err := b.Load(ctx, h, 0, 0); err == nil {
			t.Errorf("Load accepted invalid handle %+v", h)
		}
		if _, err := b.Stat(ctx, h); err == nil {
			t.Errorf("Stat accepted invalid handle %+v", h)
		}
	}
}

// testConcurrentSaveAndLoad checks the documented requirement that backends are
// safe for concurrent use; the backup pipeline uploads packs from several
// goroutines at once.
func testConcurrentSaveAndLoad(t *testing.T, b backend.Backend) {
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := packHandle(fmt.Sprintf("%02dconcurrent0000", i))
			data := bytes.Repeat([]byte{byte(i)}, 1024*(i+1))

			if err := b.Save(context.Background(), h, bytes.NewReader(data), int64(len(data))); err != nil {
				errs <- fmt.Errorf("worker %d: save: %w", i, err)
				return
			}
			got, err := backend.LoadAll(context.Background(), b, h)
			if err != nil {
				errs <- fmt.Errorf("worker %d: load: %w", i, err)
				return
			}
			if !bytes.Equal(got, data) {
				errs <- fmt.Errorf("worker %d: data mismatch", i)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func testAllFileTypes(t *testing.T, b backend.Backend) {
	types := []backend.FileType{
		backend.TypePack, backend.TypeIndex, backend.TypeSnapshot, backend.TypeKey,
	}
	for _, ft := range types {
		h := backend.Handle{Type: ft, Name: "0123456789abcdef"}
		save(t, b, h, []byte(string(ft)+" payload"))
	}

	// Objects of different types share a name; they must remain distinct.
	for _, ft := range types {
		got, err := backend.LoadAll(context.Background(), b, backend.Handle{Type: ft, Name: "0123456789abcdef"})
		if err != nil {
			t.Fatalf("LoadAll(%s): %v", ft, err)
		}
		if want := string(ft) + " payload"; string(got) != want {
			t.Errorf("%s object was overwritten by another type: got %q, want %q", ft, got, want)
		}
	}
}

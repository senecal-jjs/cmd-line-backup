package crypto

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mustKey(t *testing.T) *Key {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := mustKey(t)

	for _, size := range []int{0, 1, 15, 16, 17, 1024, 1 << 20} {
		plaintext := bytes.Repeat([]byte{0xAB}, size)
		sealed, err := k.SealData(plaintext, nil)
		if err != nil {
			t.Fatalf("SealData(%d bytes): %v", size, err)
		}
		if len(sealed) != SealedSize(size) {
			t.Errorf("sealed size = %d, want %d", len(sealed), SealedSize(size))
		}

		got, err := k.OpenData(sealed, nil)
		if err != nil {
			t.Fatalf("OpenData(%d bytes): %v", size, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("round trip of %d bytes produced different data", size)
		}
	}
}

func TestSealIsRandomized(t *testing.T) {
	k := mustKey(t)
	plaintext := []byte("the same plaintext twice")

	first, err := k.SealData(plaintext, nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}
	second, err := k.SealData(plaintext, nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}

	// Each call draws a fresh salt, so identical plaintext must not produce
	// identical ciphertext. This is what makes the fixed zero nonce safe.
	if bytes.Equal(first, second) {
		t.Error("sealing the same plaintext twice produced identical output")
	}
	if bytes.Equal(first[:saltSize], second[:saltSize]) {
		t.Error("two seals reused the same salt")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	k := mustKey(t)
	plaintext := bytes.Repeat([]byte("secret"), 100)
	sealed, err := k.SealData(plaintext, nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}

	// Every byte matters: flipping a bit anywhere, including in the salt that
	// selects the derived key, must be detected.
	positions := []struct {
		name  string
		index int
	}{
		{"salt", 0},
		{"salt end", saltSize - 1},
		{"ciphertext start", saltSize},
		{"ciphertext middle", saltSize + len(plaintext)/2},
		{"auth tag", len(sealed) - 1},
	}
	for _, pos := range positions {
		t.Run(pos.name, func(t *testing.T) {
			corrupt := bytes.Clone(sealed)
			corrupt[pos.index] ^= 0x01

			if _, err := k.OpenData(corrupt, nil); !errors.Is(err, ErrDecryption) {
				t.Errorf("OpenData on data corrupted at %s returned %v, want ErrDecryption", pos.name, err)
			}
		})
	}
}

func TestOpenRejectsTruncatedCiphertext(t *testing.T) {
	k := mustKey(t)
	sealed, err := k.SealData([]byte("data"), nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}

	for _, n := range []int{0, 1, saltSize, saltSize + gcmOverhead - 1, len(sealed) - 1} {
		if _, err := k.OpenData(sealed[:n], nil); !errors.Is(err, ErrDecryption) {
			t.Errorf("OpenData on %d-byte input returned %v, want ErrDecryption", n, err)
		}
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	k1, k2 := mustKey(t), mustKey(t)

	sealed, err := k1.SealData([]byte("private"), nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}
	if _, err := k2.OpenData(sealed, nil); !errors.Is(err, ErrDecryption) {
		t.Errorf("OpenData with the wrong key returned %v, want ErrDecryption", err)
	}
}

// TestDataAndMetaSubkeysAreDistinct verifies the separation between bulk data
// and metadata: an object sealed as one must not open as the other, so a
// repository object cannot be substituted across roles.
func TestDataAndMetaSubkeysAreDistinct(t *testing.T) {
	k := mustKey(t)
	plaintext := []byte("payload")

	sealedData, err := k.SealData(plaintext, nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}
	if _, err := k.OpenMeta(sealedData, nil); !errors.Is(err, ErrDecryption) {
		t.Errorf("OpenMeta accepted a data object: %v", err)
	}

	sealedMeta, err := k.SealMeta(plaintext, nil)
	if err != nil {
		t.Fatalf("SealMeta: %v", err)
	}
	if _, err := k.OpenData(sealedMeta, nil); !errors.Is(err, ErrDecryption) {
		t.Errorf("OpenData accepted a metadata object: %v", err)
	}
}

func TestAdditionalDataIsAuthenticated(t *testing.T) {
	k := mustKey(t)
	plaintext := []byte("bound to context")

	sealed, err := k.SealData(plaintext, []byte("pack-42"))
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}

	if _, err := k.OpenData(sealed, []byte("pack-43")); !errors.Is(err, ErrDecryption) {
		t.Errorf("OpenData accepted mismatched additional data: %v", err)
	}
	if _, err := k.OpenData(sealed, nil); !errors.Is(err, ErrDecryption) {
		t.Errorf("OpenData accepted missing additional data: %v", err)
	}

	got, err := k.OpenData(sealed, []byte("pack-42"))
	if err != nil {
		t.Fatalf("OpenData with correct additional data: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("round trip with additional data produced different data")
	}
}

func TestChunkIDIsStableAndContentDependent(t *testing.T) {
	k := mustKey(t)

	a := k.ChunkID([]byte("hello"))
	b := k.ChunkID([]byte("hello"))
	c := k.ChunkID([]byte("hellp"))

	if a != b {
		t.Error("the same content produced different chunk IDs, which would break deduplication")
	}
	if a == c {
		t.Error("different content produced the same chunk ID")
	}
	if a.IsZero() {
		t.Error("chunk ID is the zero value")
	}
}

// TestChunkIDIsKeyed is the property that prevents an attacker with repository
// access from confirming whether a file they already possess is backed up: the
// identifier depends on the key, not just the content.
func TestChunkIDIsKeyed(t *testing.T) {
	k1, k2 := mustKey(t), mustKey(t)
	content := []byte("a file whose presence should not be confirmable")

	if k1.ChunkID(content) == k2.ChunkID(content) {
		t.Error("chunk IDs are identical under different keys, so they are not keyed")
	}
}

func TestVerifyChunkID(t *testing.T) {
	k := mustKey(t)
	content := []byte("some chunk")
	id := k.ChunkID(content)

	if !k.VerifyChunkID(id, content) {
		t.Error("VerifyChunkID rejected matching content")
	}
	if k.VerifyChunkID(id, []byte("different chunk")) {
		t.Error("VerifyChunkID accepted non-matching content")
	}
}

func TestKeyfileRoundTrip(t *testing.T) {
	k := mustKey(t)
	passphrase := []byte("correct horse battery staple")

	data, err := k.MarshalKeyfile(passphrase)
	if err != nil {
		t.Fatalf("MarshalKeyfile: %v", err)
	}

	restored, err := UnmarshalKeyfile(data, passphrase)
	if err != nil {
		t.Fatalf("UnmarshalKeyfile: %v", err)
	}
	if restored.master != k.master {
		t.Fatal("restored master key differs from the original")
	}

	// The restored key must be functionally identical, not merely equal in
	// bytes: subkeys have to derive the same way too.
	if restored.ChunkID([]byte("x")) != k.ChunkID([]byte("x")) {
		t.Error("restored key derives different chunk IDs")
	}
	sealed, err := k.SealData([]byte("payload"), nil)
	if err != nil {
		t.Fatalf("SealData: %v", err)
	}
	if _, err := restored.OpenData(sealed, nil); err != nil {
		t.Errorf("restored key could not open data sealed by the original: %v", err)
	}
}

func TestKeyfileRejectsWrongPassphrase(t *testing.T) {
	k := mustKey(t)

	data, err := k.MarshalKeyfile([]byte("right passphrase"))
	if err != nil {
		t.Fatalf("MarshalKeyfile: %v", err)
	}
	if _, err := UnmarshalKeyfile(data, []byte("wrong passphrase")); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("got %v, want ErrWrongPassphrase", err)
	}
}

func TestKeyfileDoesNotContainPlaintextKeyMaterial(t *testing.T) {
	k := mustKey(t)

	data, err := k.MarshalKeyfile([]byte("passphrase"))
	if err != nil {
		t.Fatalf("MarshalKeyfile: %v", err)
	}
	for name, material := range map[string][]byte{
		"master":   k.master[:],
		"chunk ID": k.chunkID[:],
		"data":     k.data[:],
		"meta":     k.meta[:],
	} {
		if bytes.Contains(data, material) {
			t.Errorf("keyfile contains the %s key in plaintext", name)
		}
	}
}

func TestKeyfileRejectsMalformedInput(t *testing.T) {
	passphrase := []byte("passphrase")
	k := mustKey(t)
	valid, err := k.MarshalKeyfile(passphrase)
	if err != nil {
		t.Fatalf("MarshalKeyfile: %v", err)
	}

	var kf keyfile
	if err := json.Unmarshal(valid, &kf); err != nil {
		t.Fatalf("unmarshal valid keyfile: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*keyfile)
	}{
		{"unsupported version", func(f *keyfile) { f.Version = 99 }},
		{"unknown KDF", func(f *keyfile) { f.KDF.Algorithm = "pbkdf2" }},
		{"missing salt", func(f *keyfile) { f.KDF.Salt = nil }},
		{"zero time", func(f *keyfile) { f.KDF.Time = 0 }},
		{"zero memory", func(f *keyfile) { f.KDF.Memory = 0 }},
		{"zero threads", func(f *keyfile) { f.KDF.Threads = 0 }},
		{"empty sealed key", func(f *keyfile) { f.Sealed = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broken := kf
			tt.mutate(&broken)
			data, err := json.Marshal(broken)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := UnmarshalKeyfile(data, passphrase); err == nil {
				t.Error("UnmarshalKeyfile accepted a malformed keyfile")
			}
		})
	}

	if _, err := UnmarshalKeyfile([]byte("not json"), passphrase); err == nil {
		t.Error("UnmarshalKeyfile accepted non-JSON input")
	}
}

func TestMarshalKeyfileRejectsEmptyPassphrase(t *testing.T) {
	k := mustKey(t)
	if _, err := k.MarshalKeyfile(nil); err == nil {
		t.Error("MarshalKeyfile accepted an empty passphrase")
	}
}

func TestSaveAndLoadKeyfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "gobackup.key")
	passphrase := []byte("passphrase")
	k := mustKey(t)

	if err := k.SaveKeyfile(path, passphrase); err != nil {
		t.Fatalf("SaveKeyfile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// The keyfile is the single most sensitive artifact on disk; it must not be
	// readable by other users.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("keyfile permissions are %v, want 0600", perm)
	}

	loaded, err := LoadKeyfile(path, passphrase)
	if err != nil {
		t.Fatalf("LoadKeyfile: %v", err)
	}
	if loaded.master != k.master {
		t.Error("loaded key differs from the saved one")
	}
}

// TestSaveKeyfileRefusesToOverwrite guards against the worst possible data-loss
// bug in a backup tool: destroying the key that every existing backup depends on.
func TestSaveKeyfileRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gobackup.key")
	passphrase := []byte("passphrase")

	original := mustKey(t)
	if err := original.SaveKeyfile(path, passphrase); err != nil {
		t.Fatalf("SaveKeyfile: %v", err)
	}

	if err := mustKey(t).SaveKeyfile(path, passphrase); err == nil {
		t.Fatal("SaveKeyfile overwrote an existing keyfile")
	}

	loaded, err := LoadKeyfile(path, passphrase)
	if err != nil {
		t.Fatalf("LoadKeyfile: %v", err)
	}
	if loaded.master != original.master {
		t.Error("the original key was destroyed")
	}
}

func TestLoadKeyfileMissingFile(t *testing.T) {
	if _, err := LoadKeyfile(filepath.Join(t.TempDir(), "absent"), []byte("p")); err == nil {
		t.Error("LoadKeyfile succeeded on a missing file")
	}
}

func TestIDEncoding(t *testing.T) {
	k := mustKey(t)
	id := k.ChunkID([]byte("content"))

	parsed, err := ParseID(id.String())
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if parsed != id {
		t.Error("ID did not survive a String/ParseID round trip")
	}

	if len(id.Short()) != 8 {
		t.Errorf("Short() = %q, want 8 characters", id.Short())
	}

	type wrapper struct {
		ID ID `json:"id"`
	}
	data, err := json.Marshal(wrapper{ID: id})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(id.String())) {
		t.Errorf("JSON encoding %s does not contain the hex ID", data)
	}

	var got wrapper
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != id {
		t.Error("ID did not survive a JSON round trip")
	}
}

func TestParseIDRejectsInvalidInput(t *testing.T) {
	for _, s := range []string{"", "abc", "zz", string(make([]byte, 64))} {
		if _, err := ParseID(s); err == nil {
			t.Errorf("ParseID(%q) succeeded, want an error", s)
		}
	}
}

func TestIDFromBytes(t *testing.T) {
	if _, err := IDFromBytes(make([]byte, IDSize)); err != nil {
		t.Errorf("IDFromBytes on %d bytes: %v", IDSize, err)
	}
	for _, n := range []int{0, 31, 33} {
		if _, err := IDFromBytes(make([]byte, n)); err == nil {
			t.Errorf("IDFromBytes accepted %d bytes", n)
		}
	}
}

func BenchmarkSealData(b *testing.B) {
	k, err := GenerateKey()
	if err != nil {
		b.Fatal(err)
	}
	plaintext := make([]byte, 1<<20)

	b.SetBytes(int64(len(plaintext)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := k.SealData(plaintext, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkChunkID(b *testing.B) {
	k, err := GenerateKey()
	if err != nil {
		b.Fatal(err)
	}
	plaintext := make([]byte, 1<<20)

	b.SetBytes(int64(len(plaintext)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k.ChunkID(plaintext)
	}
}

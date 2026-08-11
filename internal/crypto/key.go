// Package crypto provides gobackup's client-side encryption: key management,
// authenticated encryption of repository objects, and keyed content identifiers.
//
// # Threat model
//
// The repository is assumed to be stored somewhere the operator does not
// control or fully trust. Everything written to it is encrypted locally, so the
// storage provider learns only the number of objects, their sizes, and when
// they were written. It never sees file contents, file names, directory
// structure, or anything derived from plaintext without the key.
//
// # Authenticated encryption
//
// Objects are sealed with AES-256-GCM under a key derived per object:
//
//	salt      = 32 random bytes
//	objectKey = HKDF-SHA256(subkey, salt, label)
//	sealed    = salt || AES-256-GCM(objectKey, zeroNonce, plaintext, aad)
//
// Deriving a fresh key for every object is what makes the fixed zero nonce safe.
// The usual hazard with AES-GCM is nonce reuse: with a single long-lived key and
// random 96-bit nonces, NIST caps usage at roughly 2^32 messages before
// collisions become a real risk, and a deduplicating backup tool seals one
// object per chunk. Rather than manage a nonce counter across processes and
// machines, or reach for a wider-nonce construction, this design guarantees that
// no key is ever used twice, which removes the message-count ceiling entirely.
// AES-GCM in turn brings hardware acceleration on every realistic target
// (AES-NI, ARMv8 crypto extensions) and lives in the standard library.
//
// The cost is one HKDF invocation per object -- an HMAC over a few dozen bytes,
// negligible beside megabytes of AES -- plus 48 bytes of overhead per object.
//
// # Keyed content identifiers
//
// Chunk IDs are HMAC-SHA256 over the plaintext under a dedicated subkey rather
// than a bare hash. Deduplication still works, because identical plaintext still
// yields an identical ID for anyone holding the key. But an attacker who can
// read the repository cannot test whether a file they possess is present in a
// backup, since computing its ID requires the key. A plain SHA-256 identifier
// would leak exactly that.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

const (
	// keySize is the length of the master secret and of every derived subkey.
	keySize = 32
	// saltSize is the length of the random per-object key derivation salt.
	saltSize = 32
)

// Labels separating the subkeys derived from the master secret. They are part of
// the repository format and must not change.
const (
	labelChunkID = "gobackup v1 chunk id key"
	labelData    = "gobackup v1 data key"
	labelMeta    = "gobackup v1 metadata key"
	labelObject  = "gobackup v1 object key"
)

// ErrDecryption reports that an object could not be decrypted or failed its
// authentication check. It deliberately does not distinguish between a wrong
// key, a corrupted object, and a tampered one.
var ErrDecryption = errors.New("crypto: decryption failed: wrong key, or the data is corrupt or has been tampered with")

// Key holds the master secret for a repository along with the subkeys derived
// from it. A Key must be treated as highly sensitive: it grants full read and
// write access to every backup in the repository.
type Key struct {
	master  [keySize]byte
	chunkID [keySize]byte
	data    [keySize]byte
	meta    [keySize]byte
}

// GenerateKey creates a new random master key.
func GenerateKey() (*Key, error) {
	var master [keySize]byte
	if _, err := rand.Read(master[:]); err != nil {
		return nil, fmt.Errorf("crypto: generating master key: %w", err)
	}
	return keyFromMaster(master)
}

// keyFromMaster derives the subkeys from a master secret.
func keyFromMaster(master [keySize]byte) (*Key, error) {
	k := &Key{master: master}
	for _, sub := range []struct {
		label string
		dst   *[keySize]byte
	}{
		{labelChunkID, &k.chunkID},
		{labelData, &k.data},
		{labelMeta, &k.meta},
	} {
		derived, err := hkdf.Key(sha256.New, master[:], nil, sub.label, keySize)
		if err != nil {
			return nil, fmt.Errorf("crypto: deriving subkey %q: %w", sub.label, err)
		}
		copy(sub.dst[:], derived)
	}
	return k, nil
}

// ChunkID returns the keyed identifier for a chunk of plaintext. Identical
// plaintext always yields an identical ID under the same key, which is what
// makes deduplication possible, but the ID cannot be computed without the key.
func (k *Key) ChunkID(plaintext []byte) ID {
	mac := hmac.New(sha256.New, k.chunkID[:])
	mac.Write(plaintext)
	var id ID
	copy(id[:], mac.Sum(nil))
	return id
}

// VerifyChunkID reports whether plaintext matches the given chunk ID, comparing
// in constant time.
func (k *Key) VerifyChunkID(id ID, plaintext []byte) bool {
	want := k.ChunkID(plaintext)
	return hmac.Equal(id[:], want[:])
}

// SealData encrypts a chunk or pack payload. aad is authenticated but not
// encrypted, and must be supplied identically to OpenData; callers use it to
// bind ciphertext to its context so that objects cannot be swapped for one
// another.
func (k *Key) SealData(plaintext, aad []byte) ([]byte, error) {
	return seal(k.data[:], plaintext, aad)
}

// OpenData decrypts a chunk or pack payload sealed by SealData.
func (k *Key) OpenData(sealed, aad []byte) ([]byte, error) {
	return open(k.data[:], sealed, aad)
}

// SealMeta encrypts repository metadata: the config object, indexes, and
// snapshots. Metadata uses a subkey distinct from bulk data so the two can never
// be confused for one another.
func (k *Key) SealMeta(plaintext, aad []byte) ([]byte, error) {
	return seal(k.meta[:], plaintext, aad)
}

// OpenMeta decrypts metadata sealed by SealMeta.
func (k *Key) OpenMeta(sealed, aad []byte) ([]byte, error) {
	return open(k.meta[:], sealed, aad)
}

// SealedSize returns the on-disk size of a plaintext of the given length.
func SealedSize(plaintextLen int) int {
	return saltSize + plaintextLen + gcmOverhead
}

// gcmOverhead is the size of the GCM authentication tag.
const gcmOverhead = 16

// seal encrypts plaintext under a freshly derived subkey. The layout is
// salt || ciphertext || tag; no nonce is stored because it is always zero, which
// is safe precisely because the key is unique per call.
func seal(subkey, plaintext, aad []byte) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("crypto: generating salt: %w", err)
	}

	aead, err := objectAEAD(subkey, salt)
	if err != nil {
		return nil, err
	}

	out := make([]byte, saltSize, SealedSize(len(plaintext)))
	copy(out, salt)
	nonce := make([]byte, aead.NonceSize())
	return aead.Seal(out, nonce, plaintext, aad), nil
}

// open reverses seal.
func open(subkey, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < saltSize+gcmOverhead {
		return nil, fmt.Errorf("%w: object is only %d bytes, too short to be valid",
			ErrDecryption, len(sealed))
	}

	aead, err := objectAEAD(subkey, sealed[:saltSize])
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, aead.NonceSize())
	plaintext, err := aead.Open(nil, nonce, sealed[saltSize:], aad)
	if err != nil {
		return nil, ErrDecryption
	}
	return plaintext, nil
}

// objectAEAD derives the per-object key for the given salt and returns an AEAD
// bound to it.
func objectAEAD(subkey, salt []byte) (cipher.AEAD, error) {
	objectKey, err := hkdf.Key(sha256.New, subkey, salt, labelObject, keySize)
	if err != nil {
		return nil, fmt.Errorf("crypto: deriving object key: %w", err)
	}
	block, err := aes.NewCipher(objectKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: initializing cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: initializing GCM: %w", err)
	}
	return aead, nil
}

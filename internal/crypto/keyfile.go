package crypto

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/argon2"
)

// keyfileVersion is the on-disk keyfile format version.
const keyfileVersion = 1

// Argon2id parameters used when wrapping a new keyfile. They are stored inside
// the keyfile, so raising them later does not break existing files.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB, i.e. 64 MiB
	argonThreads = 4
)

const labelKeyWrap = "gobackup v1 key wrapping"

// ErrWrongPassphrase reports that a keyfile could not be unwrapped with the
// supplied passphrase.
var ErrWrongPassphrase = errors.New("crypto: incorrect passphrase")

// keyfile is the serialized, passphrase-protected form of a master key.
//
// The KDF parameters travel with the file so that a keyfile written today stays
// readable after the defaults are raised.
type keyfile struct {
	Version int       `json:"version"`
	Created time.Time `json:"created"`
	KDF     kdfParams `json:"kdf"`
	Sealed  []byte    `json:"sealed"`
}

type kdfParams struct {
	Algorithm string `json:"algorithm"`
	Salt      []byte `json:"salt"`
	Time      uint32 `json:"time"`
	Memory    uint32 `json:"memory"`
	Threads   uint8  `json:"threads"`
}

// MarshalKeyfile wraps the master key with a passphrase and returns the
// serialized keyfile.
func (k *Key) MarshalKeyfile(passphrase []byte) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("crypto: passphrase must not be empty")
	}

	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("crypto: generating KDF salt: %w", err)
	}

	params := kdfParams{
		Algorithm: "argon2id",
		Salt:      salt,
		Time:      argonTime,
		Memory:    argonMemory,
		Threads:   argonThreads,
	}
	wrapKey := deriveWrapKey(passphrase, params)
	defer zero(wrapKey)

	sealed, err := seal(wrapKey, k.master[:], []byte(labelKeyWrap))
	if err != nil {
		return nil, fmt.Errorf("crypto: wrapping master key: %w", err)
	}

	return json.MarshalIndent(keyfile{
		Version: keyfileVersion,
		Created: time.Now().UTC(),
		KDF:     params,
		Sealed:  sealed,
	}, "", "  ")
}

// UnmarshalKeyfile unwraps a serialized keyfile with a passphrase.
func UnmarshalKeyfile(data, passphrase []byte) (*Key, error) {
	var kf keyfile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("crypto: parsing keyfile: %w", err)
	}
	if kf.Version != keyfileVersion {
		return nil, fmt.Errorf("crypto: unsupported keyfile version %d, this build understands version %d",
			kf.Version, keyfileVersion)
	}
	if kf.KDF.Algorithm != "argon2id" {
		return nil, fmt.Errorf("crypto: unsupported key derivation function %q", kf.KDF.Algorithm)
	}
	if len(kf.KDF.Salt) == 0 {
		return nil, errors.New("crypto: keyfile is missing its KDF salt")
	}
	if kf.KDF.Threads == 0 || kf.KDF.Time == 0 || kf.KDF.Memory == 0 {
		return nil, errors.New("crypto: keyfile has invalid KDF parameters")
	}

	wrapKey := deriveWrapKey(passphrase, kf.KDF)
	defer zero(wrapKey)

	master, err := open(wrapKey, kf.Sealed, []byte(labelKeyWrap))
	if err != nil {
		// A failure here is overwhelmingly likely to be a mistyped passphrase,
		// so report that rather than a generic decryption error.
		return nil, ErrWrongPassphrase
	}
	defer zero(master)

	if len(master) != keySize {
		return nil, fmt.Errorf("crypto: keyfile contains a %d-byte master key, want %d", len(master), keySize)
	}
	var m [keySize]byte
	copy(m[:], master)
	return keyFromMaster(m)
}

// SaveKeyfile writes the passphrase-wrapped key to path with owner-only
// permissions.
func (k *Key) SaveKeyfile(path string, passphrase []byte) error {
	data, err := k.MarshalKeyfile(passphrase)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("crypto: creating keyfile directory: %w", err)
		}
	}
	// O_EXCL avoids silently destroying an existing key, which would make every
	// backup it protects permanently unreadable.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("crypto: keyfile %s already exists; refusing to overwrite it", path)
		}
		return fmt.Errorf("crypto: creating keyfile: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("crypto: writing keyfile: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("crypto: syncing keyfile: %w", err)
	}
	return f.Close()
}

// LoadKeyfile reads and unwraps a keyfile from disk.
func LoadKeyfile(path string, passphrase []byte) (*Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: reading keyfile: %w", err)
	}
	key, err := UnmarshalKeyfile(data, passphrase)
	if err != nil {
		return nil, fmt.Errorf("crypto: keyfile %s: %w", path, err)
	}
	return key, nil
}

func deriveWrapKey(passphrase []byte, p kdfParams) []byte {
	return argon2.IDKey(passphrase, p.Salt, p.Time, p.Memory, p.Threads, keySize)
}

// zero overwrites a buffer holding key material. Go's garbage collector may
// still have copied it elsewhere, so this is a best-effort measure that narrows
// the window rather than a guarantee.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

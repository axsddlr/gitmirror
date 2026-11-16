// Package secretbox encrypts small secrets (forge tokens managed in the web
// UI) at rest with AES-256-GCM under a key kept in <state_dir>/secret.key.
//
// The key file holds exactly KeySize random bytes and is created once with
// mode 0600. A key file of the wrong size is an error and is never replaced:
// a new key would make every stored secret undecryptable.
//
// A sealed value is a version byte, a random 12-byte nonce and the GCM
// ciphertext with its tag. Errors never contain plaintext.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// KeyFile is the key file name inside the state directory.
const KeyFile = "secret.key"

// KeySize is the key length in bytes (AES-256).
const KeySize = 32

// version is the first byte of every sealed value.
const version byte = 1

// ErrDecrypt is returned by Open for data that was not sealed with this key
// or was modified.
var ErrDecrypt = errors.New("secretbox: cannot decrypt (wrong key or corrupted data)")

// Box seals and opens secrets with one key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box for a KeySize-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key is %d bytes, want %d", len(key), KeySize)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Load returns a Box for the existing key file at path. A missing file
// yields an error wrapping fs.ErrNotExist; Load never creates it.
func Load(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secretbox: read key file: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key file %s is corrupt: %d bytes, want %d "+
			"(restore it from a backup; it is not regenerated because stored secrets would be lost)", path, len(key), KeySize)
	}
	return New(key)
}

// LoadOrCreate is Load, creating the key file (mode 0600, KeySize random
// bytes) when it does not exist. An existing file is never overwritten.
func LoadOrCreate(path string) (*Box, error) {
	b, err := Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secretbox: generate key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return Load(path) // created concurrently
	}
	if err != nil {
		return nil, fmt.Errorf("secretbox: create key file: %w", err)
	}
	_, werr := f.Write(key)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path) // only the half-written file this call created
		return nil, fmt.Errorf("secretbox: write key file: %w", werr)
	}
	return New(key)
}

// Seal encrypts plaintext with a fresh random nonce.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	out := make([]byte, 1+ns, 1+ns+len(plaintext)+b.aead.Overhead())
	out[0] = version
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return b.aead.Seal(out, out[1:1+ns], plaintext, out[:1]), nil
}

// Open decrypts a value produced by Seal. Tampered data, another key or an
// unknown version yield an error.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < 1+ns+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	if sealed[0] != version {
		return nil, fmt.Errorf("secretbox: unknown sealed value version %d", sealed[0])
	}
	pt, err := b.aead.Open(nil, sealed[1:1+ns], sealed[1+ns:], sealed[:1])
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

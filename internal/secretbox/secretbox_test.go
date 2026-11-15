package secretbox

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := LoadOrCreate(filepath.Join(t.TempDir(), KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"", "x", "SENTINEL-token-0001", strings.Repeat("long", 1000)} {
		ct, err := b.Seal([]byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		// Short messages can appear in random bytes by chance; only check
		// ones long enough for that to be practically impossible.
		if len(msg) >= 8 && bytes.Contains(ct, []byte(msg)) {
			t.Fatalf("ciphertext contains the plaintext")
		}
		if ct[0] != version {
			t.Fatalf("version byte = %d", ct[0])
		}
		pt, err := b.Open(ct)
		if err != nil {
			t.Fatal(err)
		}
		if string(pt) != msg {
			t.Fatalf("Open = %q, want %q", pt, msg)
		}
	}
	a1, _ := b.Seal([]byte("same"))
	a2, _ := b.Seal([]byte("same"))
	if bytes.Equal(a1, a2) {
		t.Fatal("two seals of the same plaintext are identical (nonce reuse)")
	}
}

func TestOpenTamperedFails(t *testing.T) {
	b, err := LoadOrCreate(filepath.Join(t.TempDir(), KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := b.Seal([]byte("secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range ct {
		bad := bytes.Clone(ct)
		bad[i] ^= 0x01
		if _, err := b.Open(bad); err == nil {
			t.Fatalf("Open succeeded with byte %d flipped", i)
		}
	}
	for _, bad := range [][]byte{nil, {version}, ct[:len(ct)-1], append([]byte{9}, ct[1:]...)} {
		if _, err := b.Open(bad); err == nil {
			t.Fatalf("Open(%x) succeeded", bad)
		}
	}
	other, err := LoadOrCreate(filepath.Join(t.TempDir(), KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(ct); err == nil {
		t.Fatal("Open with another key succeeded")
	}
}

func TestKeyFileCreatedAndReused(t *testing.T) {
	p := filepath.Join(t.TempDir(), KeyFile)
	if _, err := Load(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing key = %v, want ErrNotExist", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load created the key file: %v", err)
	}
	b1, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != KeySize {
		t.Fatalf("key file size = %d", fi.Size())
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	key1, _ := os.ReadFile(p)
	ct, err := b1.Seal([]byte("tok"))
	if err != nil {
		t.Fatal(err)
	}
	for _, open := range []func(string) (*Box, error){LoadOrCreate, Load} {
		b2, err := open(p)
		if err != nil {
			t.Fatal(err)
		}
		pt, err := b2.Open(ct)
		if err != nil || string(pt) != "tok" {
			t.Fatalf("second instance Open = %q, %v", pt, err)
		}
	}
	key2, _ := os.ReadFile(p)
	if !bytes.Equal(key1, key2) {
		t.Fatal("key file changed on reuse")
	}
}

func TestCorruptKeyFileErrorsAndIsKept(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty": {},
		"short": bytes.Repeat([]byte{1}, KeySize-1),
		"long":  bytes.Repeat([]byte{1}, KeySize+1),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), KeyFile)
			if err := os.WriteFile(p, content, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, open := range []func(string) (*Box, error){LoadOrCreate, Load} {
				if _, err := open(p); err == nil {
					t.Fatal("corrupt key file accepted")
				} else if errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("corrupt key reported as missing: %v", err)
				}
			}
			got, _ := os.ReadFile(p)
			if !bytes.Equal(got, content) {
				t.Fatal("corrupt key file was overwritten")
			}
		})
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("16-byte key accepted")
	}
	if _, err := New(make([]byte, KeySize)); err != nil {
		t.Fatal(err)
	}
}

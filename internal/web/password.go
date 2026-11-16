package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// defaultIterations is the PBKDF2-SHA256 work factor of new hashes
	// (OWASP 2023 recommendation).
	defaultIterations = 600000
	// maxIterations bounds the work factor read back from the database.
	maxIterations = 10_000_000
	hashAlgo      = "pbkdf2-sha256"
	saltLen       = 16
	keyLen        = 32

	minPasswordLen = 12
	maxPasswordLen = 256
)

// hashIterations is the work factor used by hashPassword; tests lower it.
var hashIterations = defaultIterations

var b64 = base64.RawStdEncoding

// hashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<key>" (salt and
// key in unpadded standard base64). The algorithm and iteration count are
// stored so they can be raised later without breaking existing hashes.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, hashIterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashAlgo, hashIterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// checkPassword reports whether pw matches the encoded hash, comparing in
// constant time. A malformed hash is an error.
func checkPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashAlgo {
		return false, errors.New("unsupported password hash format")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > maxIterations {
		return false, errors.New("bad password hash iteration count")
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false, errors.New("bad password hash salt")
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, errors.New("bad password hash key")
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// checkPasswordRules returns the field ("password" or "confirm") and a
// user-facing message for a problem with a new password, or "", "" if it is
// acceptable.
func checkPasswordRules(pw, confirm string) (fieldName, msg string) {
	switch n := len([]rune(pw)); {
	case n < minPasswordLen:
		return "password", fmt.Sprintf("Use at least %d characters. A few words in a row make a good, memorable password.", minPasswordLen)
	case len(pw) > maxPasswordLen:
		return "password", fmt.Sprintf("Use at most %d characters.", maxPasswordLen)
	case pw != confirm:
		return "confirm", "The two passwords do not match. Please type the same password twice."
	}
	return "", ""
}

// setupAlphabet has no easily confused characters (0/O, 1/I/L).
const setupAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// newSetupCode returns a random code like "ABCD-EFGH-JKMN" (about 59 bits).
func newSetupCode() (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	n := 0
	for n < 12 {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		// Rejection sampling keeps the distribution uniform.
		if int(buf[0]) >= 256-256%len(setupAlphabet) {
			continue
		}
		if n > 0 && n%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(setupAlphabet[int(buf[0])%len(setupAlphabet)])
		n++
	}
	return b.String(), nil
}

// normaliseSetupCode upper-cases a typed code and drops spaces and dashes.
func normaliseSetupCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

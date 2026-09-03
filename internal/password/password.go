// Package password implements Argon2id hashing in the standard PHC string
// format, plus the server-side password policy. The policy deliberately
// avoids mechanical composition rules (NIST 800-63B guidance): length,
// blocklists and similarity checks instead.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

// Parameters for new hashes. Kept in the PHC string so future upgrades are
// self-describing; Verify+NeedsRehash transparently re-hash on next login.
type Params struct {
	MemoryKiB  uint32
	Iterations uint32
	Parallel   uint8
	SaltLen    int
	KeyLen     uint32
}

func DefaultParams() Params {
	return Params{MemoryKiB: 65536, Iterations: 3, Parallel: 1, SaltLen: 16, KeyLen: 32}
}

var ErrInvalidHash = errors.New("invalid password hash format")

func randomSalt(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Hash derives an Argon2id hash and encodes it in PHC format:
// $argon2id$v=19$m=65536,t=3,p=1$<b64salt>$<b64key>
func Hash(plain string, p Params) (string, error) {
	salt, err := randomSalt(p.SaltLen)
	if err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(plain), salt, p.Iterations, p.MemoryKiB, p.Parallel, p.KeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallel,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

type decoded struct {
	salt, key []byte
	p         Params
}

func decodePHC(hash string) (*decoded, error) {
	parts := strings.Split(hash, "$")
	// "" "argon2id" "v=19" "m=…,t=…,p=…" "salt" "key"
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return nil, fmt.Errorf("%w: unsupported argon2 version %d", ErrInvalidHash, version)
	}
	d := &decoded{p: Params{KeyLen: 32, SaltLen: 16}}
	var m, t uint32
	var pp uint8
	n, _ := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &pp)
	if n != 3 {
		return nil, ErrInvalidHash
	}
	d.p.MemoryKiB, d.p.Iterations, d.p.Parallel = m, t, pp
	var err error
	if d.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, ErrInvalidHash
	}
	if d.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, ErrInvalidHash
	}
	d.p.KeyLen = uint32(len(d.key))
	return d, nil
}

// Verify checks a candidate against a PHC hash in constant time
// (subtle.ConstantTimeCompare over the derived key). Returns (ok, err).
func Verify(plain, hash string) (bool, error) {
	d, err := decodePHC(hash)
	if err != nil {
		return false, err
	}
	candidate := argon2.IDKey([]byte(plain), d.salt, d.p.Iterations, d.p.MemoryKiB, d.p.Parallel, d.p.KeyLen)
	// Constant-time compare; equal length by construction (KeyLen from hash).
	return subtle.ConstantTimeCompare(candidate, d.key) == 1, nil
}

// NeedsRehash reports whether a stored hash's parameters are weaker than
// the current policy — the caller transparently re-hashes on next login.
func NeedsRehash(hash string, current Params) bool {
	d, err := decodePHC(hash)
	if err != nil {
		return true
	}
	return d.p.MemoryKiB != current.MemoryKiB || d.p.Iterations != current.Iterations || d.p.Parallel != current.Parallel
}

// VerifyDummy burns the same Argon2id cost as a real verification so a
// login attempt for a NON-EXISTENT email is timing-indistinguishable from
// one for an existing account (anti-enumeration).
func VerifyDummy(p Params) {
	dummy, _ := Hash("dummy-password-for-timing", p)
	_, _ = Verify("not-the-password", dummy)
}

// --- Policy -----------------------------------------------------------------

// PolicyError is safe to return to the client: it names the rule class,
// never which character failed.
type PolicyError struct{ Reason string }

func (e *PolicyError) Error() string { return e.Reason }

const (
	minLen = 10
	maxLen = 128
)

var blocklist = map[string]struct{}{
	"password": {}, "123456": {}, "123456789": {}, "12345678": {}, "111111": {},
	"1234567": {}, "password123": {}, "qwerty123": {}, "qwertyuiop": {},
	"iloveyou": {}, "admin123": {}, "letmein123": {}, "livetranslate": {},
	"пароль": {}, "пароль123": {}, "11111111": {}, "000000": {}, "abc123456": {},
}

// Validate enforces the server-side policy. The input is used as-is: no
// trimming, no case folding (the rules compare lowercased COPIES).
func Validate(plain, email, displayName string) error {
	runes := []rune(plain)
	if !printableOrSpace(runes) {
		return &PolicyError{"password_unsupported_characters"}
	}
	// The blocklist is checked BEFORE the length floor so short blocklisted
	// passwords get the specific error (several blocklist entries are
	// shorter than minLen).
	lower := strings.ToLower(plain)
	if _, bad := blocklist[lower]; bad {
		return &PolicyError{"password_common"}
	}
	if len(runes) < minLen {
		return &PolicyError{"password_too_short"}
	}
	if len(runes) > maxLen {
		return &PolicyError{"password_too_long"}
	}
	// Similar to email local-part or display name → reject.
	if similar(lower, email) || similar(lower, displayName) {
		return &PolicyError{"password_similar_to_account"}
	}
	return nil
}

func printableOrSpace(runes []rune) bool {
	for _, r := range runes {
		if r == '\n' || r == '\r' || r == '\t' {
			return false
		}
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// similar reports whether password contains a meaningful chunk of s (or
// vice versa), case-insensitively. 6+ shared consecutive chars counts.
func similar(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if len(a) < 6 || len(b) < 6 {
		return false
	}
	for i := 0; i+len("123456") <= len(a); i++ {
		chunk := a[i : i+6]
		if len(b) >= 6 && strings.Contains(b, chunk) {
			return true
		}
	}
	// Password containing the full display name / email local part.
	if len(b) <= 32 && len(a) >= len(b) && strings.Contains(a, b) {
		return true
	}
	return false
}

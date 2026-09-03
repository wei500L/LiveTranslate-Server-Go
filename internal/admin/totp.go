package admin

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP per RFC 6238 (SHA-1, 6 digits, 30-second step) — the same algorithm
// every authenticator app speaks. Implemented on the standard library; no
// external dependency, fully testable against the RFC vectors.

const totpStep = 30 * time.Second

// totpCode computes the 6-digit code for a counter value.
func totpCode(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code)
}

// VerifyTOTP checks a code against the base32 secret, accepting the code
// from the previous, current and next step (±30s clock skew).
func VerifyTOTP(base32Secret, code string) bool {
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(base32Secret), " ", "")))
	if err != nil || len(secret) == 0 {
		return false
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	now := time.Now().Unix()
	for _, drift := range []int64{-1, 0, 1} {
		if totpCode(secret, uint64((now+drift*30)/30)) == code {
			return true
		}
	}
	return false
}

// NewTOTPSecret generates a base32 secret from 20 random bytes.
func NewTOTPSecret(randBytes func(n int) ([]byte, error)) (string, error) {
	b, err := randBytes(20)
	if err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

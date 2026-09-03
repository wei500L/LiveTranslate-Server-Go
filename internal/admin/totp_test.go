package admin

import (
	"encoding/base32"
	"testing"
	"time"
)

// RFC 6238 appendix B test vector: the standard SHA-1 secret, T=59s
// (counter 1). The RFC lists 8-digit values; the 6-digit form is its
// trailing six digits (287082).
func TestTOTPRFCVector(t *testing.T) {
	secret := []byte("12345678901234567890") // RFC test secret (ASCII)
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, tc := range cases {
		if got := totpCode(secret, uint64(tc.unix/30)); got != tc.want {
			t.Fatalf("TOTP(T=%d) = %s, want %s", tc.unix, got, tc.want)
		}
	}
}

// VerifyTOTP accepts the current and adjacent step, rejects far codes.
func TestVerifyTOTPWindow(t *testing.T) {
	secret := []byte("12345678901234567890")
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)

	now := time.Now().Unix()
	current := totpCode(secret, uint64(now/30))
	prev := totpCode(secret, uint64(now/30-1))
	next := totpCode(secret, uint64(now/30+1))
	far := totpCode(secret, uint64(now/30+5))

	for name, code := range map[string]string{
		"current": current, "prev": prev, "next": next,
	} {
		if !VerifyTOTP(b32, code) {
			t.Fatalf("VerifyTOTP rejected the %s-step code %s", name, code)
		}
	}
	if VerifyTOTP(b32, far) {
		t.Fatalf("VerifyTOTP accepted a code 5 steps out: %s", far)
	}
	if VerifyTOTP(b32, "12345") || VerifyTOTP(b32, "abcdef") {
		t.Fatal("VerifyTOTP accepted malformed codes")
	}
	if VerifyTOTP("not-base32!!", current) {
		t.Fatal("VerifyTOTP accepted a bad secret")
	}
}

func TestNewTOTPSecretShape(t *testing.T) {
	s, err := NewTOTPSecret(func(n int) ([]byte, error) {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i)
		}
		return b, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 20 bytes → 32 base32 chars, no padding.
	if len(s) != 32 || s[len(s)-1] == '=' {
		t.Fatalf("secret shape wrong: %q", s)
	}
	if _, err := NewTOTPSecret(func(int) ([]byte, error) { return nil, errBoom }); err == nil {
		t.Fatal("rand failure not propagated")
	}
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }

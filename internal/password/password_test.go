package password

import (
	"strings"
	"testing"
)

func testParams() Params {
	// Small but real Argon2id — keeps the unit suite fast.
	return Params{MemoryKiB: 8192, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32}
}

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("correct-horse-battery-9", testParams())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("PHC header wrong: %s", h)
	}
	ok, err := Verify("correct-horse-battery-9", h)
	if err != nil || !ok {
		t.Fatalf("verify round trip: ok=%v err=%v", ok, err)
	}
	ok, err = Verify("wrong-password-123", h)
	if err != nil || ok {
		t.Fatalf("wrong password verified: ok=%v err=%v", ok, err)
	}
}

// Every hash gets a fresh random salt: identical passwords hash differently.
func TestPerPasswordSalt(t *testing.T) {
	h1, _ := Hash("same-password-123", testParams())
	h2, _ := Hash("same-password-123", testParams())
	if h1 == h2 {
		t.Fatal("two hashes of one password are identical — salt missing")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-hash", "$argon2i$v=19$m=8192,t=1,p=1$x$x",
		"$argon2id$v=99$m=8192,t=1,p=1$x$x",
		"$argon2id$v=19$m=abc,t=1,p=1$AAAA$AAAA",
	} {
		if _, err := Verify("whatever-password", bad); err == nil {
			t.Fatalf("malformed hash accepted: %q", bad)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	weak, _ := Hash("p", Params{MemoryKiB: 4096, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32})
	current := testParams()
	if !NeedsRehash(weak, current) {
		t.Fatal("weaker hash not flagged")
	}
	good, _ := Hash("p", current)
	if NeedsRehash(good, current) {
		t.Fatal("current-policy hash flagged")
	}
	if !NeedsRehash("garbage", current) {
		t.Fatal("garbage not flagged")
	}
}

func TestValidatePolicy(t *testing.T) {
	cases := []struct {
		name, pw, email, display string
		wantErr                  string // "" = accepted
	}{
		{"too short", "short1", "a@example.com", "名", "password_too_short"},
		{"exactly 10 ok", "123456789a", "a@example.com", "名", ""},
		{"too long", strings.Repeat("x", 129), "a@example.com", "名", "password_too_long"},
		{"exactly 128 ok", strings.Repeat("y", 128), "a@example.com", "名", ""},
		{"newline rejected", "line1\nline2x", "a@example.com", "名", "password_unsupported_characters"},
		{"blocklist en", "password123", "a@example.com", "名", "password_common"},
		{"blocklist ru", "пароль123", "a@example.com", "名", "password_common"},
		{"similar to email", "myemail-plus-99", "myemail@example.com", "名", "password_similar_to_account"},
		{"similar to display", "张三的密码xyz加长", "a@example.com", "张三的密码xyz", "password_similar_to_account"},
		{"unrelated pass", "correct-horse-battery-9", "a@example.com", "张三", ""},
		{"unicode ok", "密码密码密码密码密码", "a@example.com", "测试显示名", ""},
		{"case-insensitive blocklist", "PASSWORD123", "a@example.com", "名", "password_common"},
	}
	for _, tc := range cases {
		err := Validate(tc.pw, tc.email, tc.display)
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: rejected %q: %v", tc.name, tc.pw, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: accepted %q", tc.name, tc.pw)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: error %q, want %q", tc.name, err.Error(), tc.wantErr)
		}
	}
}

// No trimming or case folding: what the user typed is what is hashed.
func TestValidateDoesNotTrim(t *testing.T) {
	// A space-padded passphrase is VALID (spaces are password characters)
	// and must round-trip byte-exactly — a trimmed copy must NOT verify.
	padded := " padded-pass-1 "
	if err := Validate(padded, "a@example.com", "名"); err != nil {
		t.Fatalf("space-padded passphrase rejected: %v", err)
	}
	h, _ := Hash(padded, testParams())
	if ok, _ := Verify("padded-pass-1", h); ok {
		t.Fatal("trimmed copy verified — input is being trimmed somewhere")
	}
	if ok, _ := Verify(padded, h); !ok {
		t.Fatal("exact input failed to verify")
	}
	// Case is preserved: no case folding on hash input.
	mixed := "CaseSensitive-Pass-9"
	hm, _ := Hash(mixed, testParams())
	if ok, _ := Verify("casesensitive-pass-9", hm); ok {
		t.Fatal("case-folded copy verified — input is being folded")
	}
}

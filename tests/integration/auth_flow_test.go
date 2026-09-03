package integration

import (
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"livetranslate/server/internal/config"
)

// §26: register → email code → active → token pair, end to end.
func TestRegisterVerifyLoginFullFlow(t *testing.T) {
	e := newEnv(t, nil)
	resp, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "full@example.com", "password": "correct-horse-battery-9",
		"displayName": "完整流程", "device": device("ios-A"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	// Anti-enumeration shape: sent + detail, and NO userId field.
	var reg map[string]any
	decode(t, body, &reg)
	if reg["sent"] != true {
		t.Fatalf("register response sent != true: %s", body)
	}
	if _, has := reg["userId"]; has {
		t.Fatalf("register response leaks userId: %s", body)
	}

	code := e.mailer.verificationCode()
	resp, body = e.do(http.MethodPost, "/v1/auth/email/verify", "", map[string]any{
		"email": "full@example.com", "code": code,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify: %d %s", resp.StatusCode, body)
	}
	var first loginResp
	decode(t, body, &first)
	if first.AccessToken == "" || first.RefreshToken == "" || !first.EmailVerified {
		t.Fatalf("verify did not issue a verified token pair: %s", body)
	}

	// The verify-issued token must already grant sync access.
	resp, body = e.get("/v1/sync/status", first.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync/status with verify token: %d %s", resp.StatusCode, body)
	}

	// A fresh login on another device works too.
	resp, body, second := e.login("full@example.com", "correct-horse-battery-9", "ios-B")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	if second.UserID != first.UserID {
		t.Fatalf("login userId %s != verify userId %s", second.UserID, first.UserID)
	}
	if second.TokenType != "Bearer" || second.ExpiresIn != 900 {
		t.Fatalf("unexpected token metadata: %+v", second)
	}
}

// §26: duplicate email must be byte-identical to a fresh signup.
func TestRegisterDuplicateEmailIsByteIdentical(t *testing.T) {
	e := newEnv(t, nil)
	req := map[string]any{
		"email": "dupe@example.com", "password": "correct-horse-battery-9",
		"displayName": "重复", "device": device("ios-A"),
	}
	resp1, body1 := e.do(http.MethodPost, "/v1/auth/register", "", req)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first register: %d %s", resp1.StatusCode, body1)
	}
	// Same email, different password: the taken path burns a dummy hash and
	// returns the exact same bytes.
	req["password"] = "another-password-777"
	resp2, body2 := e.do(http.MethodPost, "/v1/auth/register", "", req)
	if resp2.StatusCode != resp1.StatusCode {
		t.Fatalf("status differs: %d vs %d", resp1.StatusCode, resp2.StatusCode)
	}
	if body1 != body2 {
		t.Fatalf("duplicate-email response differs:\n%s\n%s", body1, body2)
	}
	// Exactly one pending user exists (pending-reuse, not a second row).
	var n int
	err := testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE normalized_email = 'dupe@example.com'`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("users rows = %d err=%v, want 1", n, err)
	}
	// Two codes were sent; the newest one verifies.
	code := e.mailer.verificationCode()
	resp, body := e.do(http.MethodPost, "/v1/auth/email/verify", "", map[string]any{
		"email": "dupe@example.com", "code": code,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify after duplicate register: %d %s", resp.StatusCode, body)
	}
}

// §26: an unverified account can neither log in nor touch sync.
func TestUnverifiedAccountCannotLoginOrSync(t *testing.T) {
	e := newEnv(t, nil)
	resp, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "pending@example.com", "password": "correct-horse-battery-9",
		"displayName": "待验证", "device": device("ios-A"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}

	resp, body, _ = e.login("pending@example.com", "correct-horse-battery-9", "ios-A")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unverified login status = %d, want 403: %s", resp.StatusCode, body)
	}
	var errResp map[string]any
	decode(t, body, &errResp)
	if errResp["detail"] != "email not verified" {
		t.Fatalf("unverified login detail = %v", errResp["detail"])
	}
	// No token was issued, so sync is unreachable by construction; assert
	// the guard anyway with a forged empty bearer.
	resp, _ = e.get("/v1/sync/status", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sync without token = %d, want 401", resp.StatusCode)
	}
}

// §26: wrong password and unknown email return identical errors.
func TestWrongPasswordAndUnknownEmailUnified(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("known@example.com", "correct-horse-battery-9")

	respW, bodyW, _ := e.login("known@example.com", "totally-wrong-pass-1", "ios-A")
	respU, bodyU, _ := e.login("ghost@example.com", "correct-horse-battery-9", "ios-A")
	if respW.StatusCode != http.StatusUnauthorized || respU.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statuses: wrong=%d unknown=%d, want 401/401", respW.StatusCode, respU.StatusCode)
	}
	if bodyW != bodyU {
		t.Fatalf("error bodies differ:\n%s\n%s", bodyW, bodyU)
	}
	var er map[string]any
	decode(t, bodyW, &er)
	if er["detail"] != "invalid email or password" {
		t.Fatalf("detail = %v", er["detail"])
	}
	// login_events recorded both for the audit trail.
	var n int
	if err := testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM login_events WHERE result IN ('invalid_password','unknown_email')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("login_events rows = %d err=%v, want 2", n, err)
	}
}

// §26: unknown-email and wrong-password logins must be timing-indistinguishable
// (the dummy-verify design). With small Argon2 params the absolute cost is
// modest, so the assertion is on the RATIO between the two paths.
func TestLoginTimingParity(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("timing@example.com", "correct-horse-battery-9")

	median := func(email, pw string) time.Duration {
		var ds []time.Duration
		for i := 0; i < 7; i++ {
			start := time.Now()
			resp, body, _ := e.login(email, pw, "ios-timing")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("login %s: %d %s", email, resp.StatusCode, body)
			}
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	unknown := median("ghost@example.com", "correct-horse-battery-9")
	wrong := median("timing@example.com", "totally-wrong-pass-1")
	if unknown < 500*time.Microsecond || wrong < 500*time.Microsecond {
		t.Fatalf("Argon2 cost missing? unknown=%v wrong=%v", unknown, wrong)
	}
	hi, lo := unknown, wrong
	if lo > hi {
		hi, lo = wrong, unknown
	}
	if float64(hi) > 3.0*float64(lo) {
		t.Fatalf("timing side channel: unknown=%v wrong=%v (ratio %.2f)",
			unknown, wrong, float64(hi)/float64(lo))
	}
	t.Logf("timing: unknown=%v wrong=%v", unknown, wrong)
}

// §26: resend cooldown — an immediate resend is throttled, a later one works.
func TestResendCooldown(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ResendCooldown = 400 * time.Millisecond })
	_, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "cooldown@example.com", "password": "correct-horse-battery-9",
		"displayName": "冷却", "device": device("ios-A"),
	})
	if body == "" {
		t.Fatal("register failed")
	}
	resp, body := e.do(http.MethodPost, "/v1/auth/email/resend", "", map[string]any{
		"email": "cooldown@example.com",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("immediate resend = %d, want 429: %s", resp.StatusCode, body)
	}
	var er map[string]any
	decode(t, body, &er)
	if er["detail"] != "please wait before requesting another code" {
		t.Fatalf("resend detail = %v", er["detail"])
	}
	time.Sleep(450 * time.Millisecond)
	resp, body = e.do(http.MethodPost, "/v1/auth/email/resend", "", map[string]any{
		"email": "cooldown@example.com",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-cooldown resend = %d: %s", resp.StatusCode, body)
	}
	// The new code verifies.
	code := e.mailer.verificationCode()
	resp, body = e.do(http.MethodPost, "/v1/auth/email/verify", "", map[string]any{
		"email": "cooldown@example.com", "code": code,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify after resend: %d %s", resp.StatusCode, body)
	}
}

// §26: verification codes have an attempt cap — after too many wrong codes
// the correct one is refused until a new code is requested.
func TestVerifyAttemptCap(t *testing.T) {
	e := newEnv(t, nil)
	_, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "cap@example.com", "password": "correct-horse-battery-9",
		"displayName": "上限", "device": device("ios-A"),
	})
	if body == "" {
		t.Fatal("register failed")
	}
	real := e.mailer.verificationCode()
	for i := 0; i < 5; i++ {
		resp, _ := e.do(http.MethodPost, "/v1/auth/email/verify", "", map[string]any{
			"email": "cap@example.com", "code": fmt.Sprintf("%06d", (i+1)*111111%1000000),
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("wrong-code attempt %d = %d, want 400", i, resp.StatusCode)
		}
	}
	// The real code no longer passes: attempts exhausted.
	resp, body := e.do(http.MethodPost, "/v1/auth/email/verify", "", map[string]any{
		"email": "cap@example.com", "code": real,
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct code after 5 wrong = %d, want 429: %s", resp.StatusCode, body)
	}
	var er map[string]any
	decode(t, body, &er)
	if er["detail"] != "too many attempts, request a new code" {
		t.Fatalf("cap detail = %v", er["detail"])
	}
}

// §26: password policy at registration (length, blocklist, similarity).
func TestPasswordPolicyAtRegister(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		name     string
		password string
		email    string
		want     string
	}{
		{"too short", "short1", "p1@example.com", "password rejected: password_too_short"},
		{"too long", longPassword(129), "p2@example.com", "password rejected: password_too_long"},
		{"blocklist", "password123", "p3@example.com", "password rejected: password_common"},
		{"similar to email", "contains-myemail99", "myemail@example.com", "password rejected: password_similar_to_account"},
	}
	for _, tc := range cases {
		resp, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
			"email": tc.email, "password": tc.password,
			"displayName": "策略", "device": device("ios-A"),
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", tc.name, resp.StatusCode, body)
		}
		var er map[string]any
		decode(t, body, &er)
		if er["detail"] != tc.want {
			t.Fatalf("%s: detail = %v, want %s", tc.name, er["detail"], tc.want)
		}
	}
	// Unicode passwords are accepted when long enough.
	resp, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "uni@example.com", "password": "密码密码密码密码密码",
		"displayName": "测试显示名", "device": device("ios-A"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unicode password rejected: %d %s", resp.StatusCode, body)
	}
}

func longPassword(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}

// §26: per-IP auth rate limit → 429 + Retry-After.
func TestAuthRateLimitPerIP(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.AuthRateLimitPerMinute = 3 })
	for i := 0; i < 3; i++ {
		resp, body := e.do(http.MethodPost, "/v1/auth/login", "", map[string]any{
			"email": fmt.Sprintf("r%d@example.com", i), "password": "whatever-pass-99",
			"device": device("ios-A"),
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d: %s", i, resp.StatusCode, body)
		}
	}
	resp, body := e.do(http.MethodPost, "/v1/auth/login", "", map[string]any{
		"email": "r9@example.com", "password": "whatever-pass-99", "device": device("ios-A"),
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th attempt = %d, want 429: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

// §26: progressive delay then per-IP block on repeated failures.
func TestLoginProgressiveDelayAndIPBlock(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.LoginFailMax = 2
		c.IPFailMax = 4
	})
	e.registerAndVerify("delay@example.com", "correct-horse-battery-9")

	// Two failures reach LoginFailMax: the next attempts pay a delay.
	for i := 0; i < 2; i++ {
		resp, body, _ := e.login("delay@example.com", "wrong-password-11", "ios-A")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("warmup failure %d = %d: %s", i, resp.StatusCode, body)
		}
	}
	start := time.Now()
	resp, body, _ := e.login("delay@example.com", "wrong-password-11", "ios-A")
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("delayed attempt = %d: %s", resp.StatusCode, body)
	}
	// 3 failures → 750ms planned delay; allow generous scheduling slack.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("progressive delay missing: %v", elapsed)
	}
	// Attempts 4 and 5 still pay the delay (4 recorded failures each);
	// the IP block fires once 4 failures are RECORDED and the next attempt
	// consults the counter.
	resp, body, _ = e.login("delay@example.com", "wrong-password-11", "ios-A")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("attempt 4 = %d, want 401: %s", resp.StatusCode, body)
	}
	resp, body, _ = e.login("delay@example.com", "wrong-password-11", "ios-A")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("ip-blocked attempt = %d, want 429: %s", resp.StatusCode, body)
	}
	// The correct password is ALSO refused while IP-blocked…
	resp, body, _ = e.login("delay@example.com", "correct-horse-battery-9", "ios-A")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct password while blocked = %d, want 429: %s", resp.StatusCode, body)
	}
	// …and no permanent lockout flag exists on the account: the password
	// itself was never disabled (status still active).
	var status string
	if err := testDB.Pool.QueryRow(t.Context(),
		`SELECT status FROM users WHERE normalized_email='delay@example.com'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("account status = %q err=%v, want active (no lockout)", status, err)
	}
}

// Registration must not leak which emails exist through the mailer either:
// an unknown email produces NO outbound message but the same response.
func TestRegisterUnknownEmailSendsNoMail(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {})
	resp, _ := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "invalid-format", "password": "correct-horse-battery-9",
		"displayName": "无效", "device": device("ios-A"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invalid email register = %d, want 200", resp.StatusCode)
	}
	if n := len(e.mailer.snapshot()); n != 0 {
		t.Fatalf("mail sent for invalid email: %d", n)
	}
}

func (c *captureMailer) snapshot() []capturedMail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedMail, len(c.sent))
	copy(out, c.sent)
	return out
}

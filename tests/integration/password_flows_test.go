package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"livetranslate/server/internal/password"
)

// §26: forgot → reset. The reset token is single-use and revokes every
// refresh token of the account.
func TestForgotAndResetPassword(t *testing.T) {
	e := newEnv(t, nil)
	first := e.registerAndVerify("reset@example.com", "original-password-77")
	// A second device holds a refresh token that must die on reset.
	_, _, second := e.login("reset@example.com", "original-password-77", "ios-B")

	// Forgot: uniform response, code/token in the mail.
	resp, body := e.do(http.MethodPost, "/v1/auth/password/forgot", "", map[string]any{
		"email": "reset@example.com",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forgot: %d %s", resp.StatusCode, body)
	}
	respUnknown, bodyUnknown := e.do(http.MethodPost, "/v1/auth/password/forgot", "", map[string]any{
		"email": "ghost@example.com",
	})
	if respUnknown.StatusCode != http.StatusOK || bodyUnknown != body {
		t.Fatalf("forgot is not uniform: %d/%d %q vs %q",
			resp.StatusCode, respUnknown.StatusCode, body, bodyUnknown)
	}
	tok := e.mailer.resetToken()

	// Reset with a bad token fails cleanly.
	resp, body = e.do(http.MethodPost, "/v1/auth/password/reset", "", map[string]any{
		"token": "definitely-not-the-token-000000", "newPassword": "brand-new-pass-99",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset with bad token = %d: %s", resp.StatusCode, body)
	}

	// Reset with the real token succeeds.
	resp, body = e.do(http.MethodPost, "/v1/auth/password/reset", "", map[string]any{
		"token": tok, "newPassword": "brand-new-pass-99",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d %s", resp.StatusCode, body)
	}

	// Single use: the same token cannot reset again.
	resp, body = e.do(http.MethodPost, "/v1/auth/password/reset", "", map[string]any{
		"token": tok, "newPassword": "yet-another-pass-88",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset replay = %d, want 400: %s", resp.StatusCode, body)
	}

	// Both devices' refresh tokens were revoked.
	for name, rt := range map[string]string{"first": first.RefreshToken, "second": second.RefreshToken} {
		resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": rt})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s refresh after reset = %d, want 401: %s", name, resp.StatusCode, body)
		}
	}

	// Old password no longer logs in; the new one does.
	resp, body, _ = e.login("reset@example.com", "original-password-77", "ios-C")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password accepted after reset: %d", resp.StatusCode)
	}
	resp, body, fresh := e.login("reset@example.com", "brand-new-pass-99", "ios-C")
	if resp.StatusCode != http.StatusOK || fresh.AccessToken == "" {
		t.Fatalf("new password login: %d %s", resp.StatusCode, body)
	}
}

// §26: forgot-password only mails verified, active accounts.
func TestForgotPasswordIgnoresUnverified(t *testing.T) {
	e := newEnv(t, nil)
	_, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "unverified@example.com", "password": "correct-horse-battery-9",
		"displayName": "未验证", "device": device("ios-A"),
	})
	if body == "" {
		t.Fatal("register failed")
	}
	resp, body := e.do(http.MethodPost, "/v1/auth/password/forgot", "", map[string]any{
		"email": "unverified@example.com",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forgot for unverified = %d: %s", resp.StatusCode, body)
	}
	if n := len(e.mailer.snapshot()); n != 1 { // only the verification code
		t.Fatalf("reset mail sent for unverified account: %d mails", n)
	}
}

// §26: change password requires the current password and revokes OTHER
// devices, keeping the requesting device signed in.
func TestChangePasswordRevokesOtherDevices(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("changer@example.com", "original-password-77")
	_, _, phone := e.login("changer@example.com", "original-password-77", "ios-phone")
	_, _, iPad := e.login("changer@example.com", "original-password-77", "ios-ipad")

	// Wrong current password → 401, nothing changes.
	resp, body := e.do(http.MethodPost, "/v1/me/password/change", phone.AccessToken, map[string]any{
		"currentPassword": "wrong-current-11", "newPassword": "brand-new-pass-99",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("change with wrong current = %d: %s", resp.StatusCode, body)
	}

	// Correct change from the phone.
	resp, body = e.do(http.MethodPost, "/v1/me/password/change", phone.AccessToken, map[string]any{
		"currentPassword": "original-password-77", "newPassword": "brand-new-pass-99",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change: %d %s", resp.StatusCode, body)
	}

	// The phone still has a live refresh (it may roll), the iPad does not.
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": iPad.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("iPad refresh after change = %d, want 401: %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": phone.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("phone refresh after change = %d, want 200: %s", resp.StatusCode, body)
	}

	// New password works; old does not.
	resp, body, _ = e.login("changer@example.com", "original-password-77", "ios-x")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password accepted after change: %d", resp.StatusCode)
	}
	resp, body, _ = e.login("changer@example.com", "brand-new-pass-99", "ios-x")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new password login: %d %s", resp.StatusCode, body)
	}
}

// §26: refresh rotation with reuse detection — replaying a rotated token
// revokes the whole family.
func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("rotate@example.com", "correct-horse-battery-9")
	_, _, dev := e.login("rotate@example.com", "correct-horse-battery-9", "ios-A")

	// Rotate once: old token becomes invalid, new one works.
	resp, body := e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": dev.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first refresh: %d %s", resp.StatusCode, body)
	}
	var rotated loginResp
	decode(t, body, &rotated)

	// REPLAY the original (already-rotated) token: replay detection fires…
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": dev.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed refresh = %d, want 401: %s", resp.StatusCode, body)
	}
	// …and the whole family is dead: the legitimate rotated token is revoked.
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": rotated.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rotated token after replay = %d, want 401: %s", resp.StatusCode, body)
	}

	// A second device's tokens survive (family = one device chain).
	_, _, other := e.login("rotate@example.com", "correct-horse-battery-9", "ios-B")
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": other.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other device refresh after replay elsewhere = %d: %s", resp.StatusCode, body)
	}
}

// §26: logout semantics — idempotent, and revokeDevice kills the chain.
func TestLogoutSemantics(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("logout@example.com", "correct-horse-battery-9")
	_, _, dev := e.login("logout@example.com", "correct-horse-battery-9", "ios-A")

	// Logout with an unknown token is still 204 (idempotent, matches Python).
	resp, _ := e.do(http.MethodPost, "/v1/auth/logout", "", map[string]any{
		"refreshToken": "not-a-real-token-at-all-000", "revokeDevice": false,
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout unknown token = %d, want 204", resp.StatusCode)
	}

	// logout-all requires auth and revokes every refresh token.
	_, _, other := e.login("logout@example.com", "correct-horse-battery-9", "ios-B")
	resp, body := e.do(http.MethodPost, "/v1/auth/logout-all", dev.AccessToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout-all: %d %s", resp.StatusCode, body)
	}
	for name, rt := range map[string]string{"A": dev.RefreshToken, "B": other.RefreshToken} {
		resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": rt})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("device %s refresh after logout-all = %d, want 401", name, resp.StatusCode)
		}
	}
	// The access token itself: status 'active' but its refresh is gone; the
	// JWT dies at expiry (15 min) — asserted by policy, not by this test.

	// logout-all without a token is rejected.
	resp, _ = e.do(http.MethodPost, "/v1/auth/logout-all", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout-all without auth = %d, want 401", resp.StatusCode)
	}
}

// §26: device management — list and per-device revocation.
func TestDeviceListAndRevoke(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("devices@example.com", "correct-horse-battery-9")
	_, _, phone := e.login("devices@example.com", "correct-horse-battery-9", "ios-phone")
	_, _, iPad := e.login("devices@example.com", "correct-horse-battery-9", "ios-ipad")

	resp, body := e.get("/v1/me/devices", phone.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list devices: %d %s", resp.StatusCode, body)
	}
	var list struct {
		Devices []struct {
			DeviceID   string    `json:"deviceId"`
			Name       string    `json:"name"`
			AppVersion string    `json:"appVersion"`
			Current    bool      `json:"current"`
			LastSeenAt time.Time `json:"lastSeenAt"`
		} `json:"devices"`
	}
	decode(t, body, &list)
	if len(list.Devices) < 2 {
		t.Fatalf("devices listed = %d, want >= 2: %s", len(list.Devices), body)
	}

	// Revoke the iPad, addressed by its display name.
	var ipadID string
	for _, d := range list.Devices {
		if d.Name == "测试设备-ios-ipad" {
			ipadID = d.DeviceID
		}
	}
	if ipadID == "" {
		t.Fatalf("ipad not in device list: %s", body)
	}
	resp, body = e.do(http.MethodDelete, "/v1/me/devices/"+ipadID, phone.AccessToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke device: %d %s", resp.StatusCode, body)
	}
	// The iPad's refresh token is dead; the phone's still works.
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": iPad.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("iPad refresh after revoke = %d, want 401: %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": phone.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("phone refresh after revoke = %d, want 200: %s", resp.StatusCode, body)
	}
}

// Transparent re-hash: a credential stored with weaker parameters is
// upgraded to the current policy on the next successful login.
func TestTransparentRehashOnLogin(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("rehash@example.com", "correct-horse-battery-9")

	// Re-hash the password with WEAKER parameters and store that — the
	// faithful simulation of a legacy credential created before a policy
	// upgrade (a PHC string is self-describing, so Verify still succeeds).
	weakHash, err := password.Hash("correct-horse-battery-9", password.Params{
		MemoryKiB: 4096, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = testDB.Pool.Exec(t.Context(), `
		UPDATE password_credentials pc
		SET password_hash = $1
		FROM users u
		WHERE pc.user_id = u.id AND u.normalized_email = 'rehash@example.com'`, weakHash)
	if err != nil {
		t.Fatalf("store weak hash: %v", err)
	}
	resp, body, _ := e.login("rehash@example.com", "correct-horse-battery-9", "ios-A")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login with legacy hash: %d %s", resp.StatusCode, body)
	}
	var stored string
	if err := testDB.Pool.QueryRow(t.Context(), `
		SELECT pc.password_hash FROM password_credentials pc
		JOIN users u ON u.id = pc.user_id
		WHERE u.normalized_email = 'rehash@example.com'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "m=8192") {
		t.Fatalf("hash not re-upgraded after login: %s", stored)
	}
}

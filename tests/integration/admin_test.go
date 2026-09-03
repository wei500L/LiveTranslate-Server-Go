package integration

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"livetranslate/server/internal/admin"
	"livetranslate/server/internal/password"
)

// createAdminAccount inserts an admin directly (the CLI path is exercised
// separately; here we need a known password hash at test params).
func (e *env) createAdminAccount(t *testing.T, username, passwd string) {
	t.Helper()
	hash, err := password.Hash(passwd, password.Params{
		MemoryKiB: 8192, Iterations: 1, Parallel: 1, SaltLen: 16, KeyLen: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateAdmin(t.Context(), testDB.Q(), username, hash, nil); err != nil {
		t.Fatal(err)
	}
}

// adminClient returns a cookie-holding client that does NOT follow
// redirects, so 303s can be asserted directly.
func adminClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func adminPostForm(t *testing.T, c *http.Client, action string, vals url.Values) int {
	t.Helper()
	resp, err := c.PostForm(action, vals)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func adminGet(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(b)
}

// §26: admin login, session cookie, and the CSRF double-submit gate.
func TestAdminLoginAndCSRF(t *testing.T) {
	e := newEnv(t, nil)
	e.createAdminAccount(t, "ops", "Admin-Test-Pass-2026!")

	// Wrong password: 401, no session.
	bad := adminClient(t)
	if code := adminPostForm(t, bad, e.adminSrv.URL+"/login", url.Values{
		"username": {"ops"}, "password": {"wrong-pass-123"},
	}); code != http.StatusUnauthorized {
		t.Fatalf("bad admin login = %d", code)
	}
	if code, _ := adminGet(t, bad, e.adminSrv.URL+"/"); code != http.StatusSeeOther {
		t.Fatalf("unauthenticated dashboard = %d, want 303 → /login", code)
	}

	// Correct login: cookie session works on GET pages…
	c := adminClient(t)
	if code := adminPostForm(t, c, e.adminSrv.URL+"/login", url.Values{
		"username": {"ops"}, "password": {"Admin-Test-Pass-2026!"},
	}); code != http.StatusSeeOther {
		t.Fatal("admin login not accepted")
	}
	for _, path := range []string{"/", "/users", "/invitations", "/audit"} {
		if code, body := adminGet(t, c, e.adminSrv.URL+path); code != http.StatusOK {
			t.Fatalf("page %s = %d: %s", path, code, body)
		}
	}

	// …but state-changing POSTs need the CSRF token.
	if code := adminPostForm(t, c, e.adminSrv.URL+"/invitations", url.Values{
		"note": {"no-csrf"}, "max_uses": {"1"},
	}); code != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", code)
	}
	// Logout also requires CSRF.
	if code := adminPostForm(t, c, e.adminSrv.URL+"/logout", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d, want 403", code)
	}
}

// adminCSRF extracts the current CSRF token from any page.
func (e *env) adminCSRF(t *testing.T, c *http.Client) string {
	t.Helper()
	_, body := adminGet(t, c, e.adminSrv.URL+"/invitations")
	i := strings.Index(body, `name="csrf_token" value="`)
	if i < 0 {
		t.Fatal("no csrf field on page")
	}
	rest := body[i+len(`name="csrf_token" value="`):]
	return rest[:strings.Index(rest, `"`)]
}

// adminUserID finds a user's id via the admin search page.
func (e *env) adminUserID(t *testing.T, c *http.Client, query string) string {
	t.Helper()
	_, page := adminGet(t, c, e.adminSrv.URL+"/users?q="+query)
	i := strings.Index(page, "/users/")
	if i < 0 {
		t.Fatalf("user %q not listed", query)
	}
	rest := page[i+len("/users/"):]
	return rest[:strings.Index(rest, "\"")]
}

// §26: admin suspend → user loses access (byte-identical detail to the
// Python-era guard) → reactivate → access returns; both actions audited.
func TestAdminSuspendAndAudit(t *testing.T) {
	e := newEnv(t, nil)
	e.createAdminAccount(t, "ops", "Admin-Test-Pass-2026!")
	u := e.registerAndVerify("suspendee@example.com", "correct-horse-battery-9")
	c := adminClient(t)
	adminPostForm(t, c, e.adminSrv.URL+"/login", url.Values{
		"username": {"ops"}, "password": {"Admin-Test-Pass-2026!"},
	})
	userID := e.adminUserID(t, c, "suspendee")
	csrf := e.adminCSRF(t, c)

	if code := adminPostForm(t, c, e.adminSrv.URL+"/users/"+userID+"/suspend", url.Values{
		"reason": {"考试违规"}, "csrf_token": {csrf},
	}); code != http.StatusSeeOther {
		t.Fatalf("suspend = %d, want 303", code)
	}

	// The suspended user's API token is refused with the Python-era text.
	resp2, body := e.get("/v1/account/me", u.AccessToken)
	if resp2.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "account unavailable") {
		t.Fatalf("suspended user access: %d %s", resp2.StatusCode, body)
	}

	// The audit log records the action and the reason.
	var reason string
	err := testDB.Pool.QueryRow(t.Context(), `
		SELECT reason FROM audit_events
		WHERE action = 'admin.user_suspend' ORDER BY id DESC LIMIT 1`).Scan(&reason)
	if err != nil {
		t.Fatalf("no suspend audit row: %v", err)
	}
	if reason != "考试违规" {
		t.Fatalf("audit reason = %q", reason)
	}

	// Reactivate restores access.
	if code := adminPostForm(t, c, e.adminSrv.URL+"/users/"+userID+"/reactivate", url.Values{
		"reason": {"复核通过"}, "csrf_token": {csrf},
	}); code != http.StatusSeeOther {
		t.Fatalf("reactivate = %d", code)
	}
	resp2, body = e.get("/v1/account/me", u.AccessToken)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("reactivated user access: %d %s", resp2.StatusCode, body)
	}
}

// §26 (§5): administrators NEVER see classroom content — the transcript
// text must not appear on any admin page.
func TestAdminCannotSeeTranscriptContent(t *testing.T) {
	e := newEnv(t, nil)
	e.createAdminAccount(t, "ops", "Admin-Test-Pass-2026!")
	u := e.registerAndVerify("secret@example.com", "correct-horse-battery-9")

	const russianMarker = "СЕКРЕТНЫЙМАРКЕР5487"
	const chineseMarker = "绝密标记5487"
	sessID := uuid.New().String()
	entryID := uuid.New().String()
	op := entryOp(uuid.New().String(), sessID, entryID, 1, russianMarker)
	op["payload"].(map[string]any)["chineseText"] = chineseMarker
	_, _, out := e.push(t, u.AccessToken,
		sessionOp(uuid.New().String(), sessID, 0, "秘密课堂标题X7"), op)
	for i, r := range results(t, out) {
		if resultField(t, r, "status") != "accepted" {
			t.Fatalf("setup push %d failed: %v", i, r)
		}
	}

	c := adminClient(t)
	adminPostForm(t, c, e.adminSrv.URL+"/login", url.Values{
		"username": {"ops"}, "password": {"Admin-Test-Pass-2026!"},
	})
	userID := e.adminUserID(t, c, "secret")

	// User detail, users list, dashboard, audit: none may leak the markers.
	pages := map[string]string{
		"users":     e.adminSrv.URL + "/users",
		"detail":    e.adminSrv.URL + "/users/" + userID,
		"dashboard": e.adminSrv.URL + "/",
		"audit":     e.adminSrv.URL + "/audit",
	}
	for name, pageURL := range pages {
		_, body := adminGet(t, c, pageURL)
		for _, marker := range []string{russianMarker, chineseMarker, "秘密课堂标题X7"} {
			if strings.Contains(body, marker) {
				t.Fatalf("%s page leaks classroom content %q", name, marker)
			}
		}
	}
	// The detail page shows counts and the no-content notice, not text.
	_, body := adminGet(t, c, e.adminSrv.URL+"/users/"+userID)
	if !strings.Contains(body, "管理员不可查看") {
		t.Fatalf("user detail missing the no-content notice")
	}
}

// §26: invitation create + revoke through the admin UI.
func TestAdminInvitations(t *testing.T) {
	e := newEnv(t, nil)
	e.createAdminAccount(t, "ops", "Admin-Test-Pass-2026!")
	c := adminClient(t)
	adminPostForm(t, c, e.adminSrv.URL+"/login", url.Values{
		"username": {"ops"}, "password": {"Admin-Test-Pass-2026!"},
	})
	csrf := e.adminCSRF(t, c)

	if code := adminPostForm(t, c, e.adminSrv.URL+"/invitations", url.Values{
		"note": {"新学期"}, "max_uses": {"5"}, "csrf_token": {csrf},
	}); code != http.StatusSeeOther {
		t.Fatalf("invitation create = %d, want 303", code)
	}
	_, body := adminGet(t, c, e.adminSrv.URL+"/invitations")
	if !strings.Contains(body, "新学期") {
		t.Fatal("created invitation not listed")
	}
	// Extract the code and revoke it.
	i := strings.Index(body, "<code>")
	if i < 0 {
		t.Fatal("no invitation code shown")
	}
	rest := body[i+len("<code>"):]
	code := rest[:strings.Index(rest, "</code>")]
	if code == "" {
		t.Fatal("empty invitation code")
	}
	if status := adminPostForm(t, c, e.adminSrv.URL+"/invitations/revoke", url.Values{
		"code": {code}, "csrf_token": {csrf},
	}); status != http.StatusSeeOther {
		t.Fatalf("invitation revoke = %d", status)
	}
	_, body = adminGet(t, c, e.adminSrv.URL+"/invitations")
	if !strings.Contains(body, "已吊销") {
		t.Fatal("revoked invitation not shown as revoked")
	}
}

// §26: progressive admin lockout — after enough failures the account is
// locked even for the CORRECT password (no lockout bypass).
func TestAdminProgressiveLockout(t *testing.T) {
	e := newEnv(t, nil)
	e.createAdminAccount(t, "lockme", "Admin-Test-Pass-2026!")

	fail := func() int {
		return adminPostForm(t, adminClient(t), e.adminSrv.URL+"/login", url.Values{
			"username": {"lockme"}, "password": {"wrong-pass-123"},
		})
	}
	// Threshold: 2 failures → 1 minute lockout (store-level constant).
	for i := 0; i < 2; i++ {
		if code := fail(); code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i, code)
		}
	}
	// Correct password is now refused: locked.
	if code := adminPostForm(t, adminClient(t), e.adminSrv.URL+"/login", url.Values{
		"username": {"lockme"}, "password": {"Admin-Test-Pass-2026!"},
	}); code != http.StatusUnauthorized {
		t.Fatalf("login while locked = %d, want 401", code)
	}
	var locked bool
	if err := testDB.Pool.QueryRow(t.Context(), `
		SELECT locked_until > now() FROM admin_accounts WHERE username = 'lockme'`).Scan(&locked); err != nil || !locked {
		t.Fatalf("lockout flag missing (err=%v locked=%v)", err, locked)
	}
}

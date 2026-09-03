package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// §26: account deletion purges sync data, revokes tokens and soft-deletes
// the account; the email becomes reusable and old credentials never work.
func TestDeleteAccount(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("gone@example.com", "correct-horse-battery-9")

	sessID := uuid.New().String()
	_, _, out := e.push(t, u.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "将被清空"))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("setup push failed: %v", out)
	}

	resp, body := e.do(http.MethodDelete, "/v1/account", u.AccessToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete account: %d %s", resp.StatusCode, body)
	}

	// Sync data purged.
	var n int
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM classroom_sessions`).Scan(&n)
	if n != 0 {
		t.Fatalf("classroom_sessions not purged: %d", n)
	}
	// Refresh token revoked → no new access tokens.
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": u.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after deletion = %d: %s", resp.StatusCode, body)
	}
	// Login with the old password fails.
	resp, body, _ = e.login("gone@example.com", "correct-horse-battery-9", "ios-A")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login after deletion = %d: %s", resp.StatusCode, body)
	}
	// The email is reusable: a fresh signup goes through.
	resp, body = e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "gone@example.com", "password": "correct-horse-battery-9",
		"displayName": "新生", "device": device("ios-A"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-register deleted email: %d %s", resp.StatusCode, body)
	}
}

// §26: cloud-data purge wipes sync content but keeps the account and the
// current session alive.
func TestPurgeCloudData(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("purge@example.com", "correct-horse-battery-9")

	sessID := uuid.New().String()
	_, _, out := e.push(t, u.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "云端数据"))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("setup push failed: %v", out)
	}

	resp, body := e.do(http.MethodDelete, "/v1/account/cloud-data", u.AccessToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge cloud data: %d %s", resp.StatusCode, body)
	}

	// Everything sync-related is gone…
	for _, table := range []string{"classroom_sessions", "transcript_entries", "bookmarks", "favorite_sessions", "sync_changes", "processed_operations"} {
		var n int
		_ = testDB.Pool.QueryRow(t.Context(),
			"SELECT count(*) FROM "+table).Scan(&n)
		if n != 0 {
			t.Fatalf("%s not purged: %d rows", table, n)
		}
	}
	// …pull from zero is empty…
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	if len(changes) != 0 {
		t.Fatalf("pull after purge: %v", changes)
	}
	// …but the account still works (status active, token valid).
	resp, body = e.get("/v1/account/me", u.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me after purge: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refreshToken": u.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh after purge = %d: %s", resp.StatusCode, body)
	}
}

// Unauthenticated access to the account routes is refused.
func TestAccountRoutesRequireAuth(t *testing.T) {
	e := newEnv(t, nil)
	for _, m := range []struct{ method, path string }{
		{http.MethodGet, "/v1/account/me"},
		{http.MethodDelete, "/v1/account/cloud-data"},
		{http.MethodDelete, "/v1/account"},
	} {
		resp, _ := e.do(m.method, m.path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", m.method, m.path, resp.StatusCode)
		}
	}
}

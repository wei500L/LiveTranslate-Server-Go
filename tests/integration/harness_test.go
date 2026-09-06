// Package integration runs the full server stack (auth + sync + account +
// admin) against a REAL PostgreSQL database. No SQLite, no mocks of the
// protocol layer: every test exercises the exact HTTP surface the iOS client
// talks to, on the exact migrations production runs.
//
// Database selection (in priority order):
//  1. LIVETRANSLATE_TEST_DATABASE_URL  — full DSN, used as-is
//  2. LIVETRANSLATE_TEST_DATABASE_NAME — database name on the local socket
//  3. default                          — livetranslate_go_test on the local
//     socket (matches the dev PostgreSQL at /tmp:5432)
//
// The database is DROPPED AND RECREATED at the start of every `go test` run;
// never point these variables at anything you care about.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"livetranslate/server/db"
	"livetranslate/server/internal/admin"
	"livetranslate/server/internal/audit"
	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi"
	accountapi "livetranslate/server/internal/httpapi/accountapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/httpapi/middleware"
	syncapi "livetranslate/server/internal/httpapi/syncapi"
	"livetranslate/server/internal/mail"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
	"livetranslate/server/internal/token"
)

var testDB *store.DB

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn := prepareTestDatabase(ctx)
	if dsn == "" {
		fmt.Fprintln(os.Stderr, `integration tests require PostgreSQL:
  export LIVETRANSLATE_TEST_DATABASE_URL="postgres://oo@/livetranslate_go_test?host=/tmp&port=5432"
(refusing to fall back to SQLite — the suite exists to verify real PostgreSQL semantics)`)
		os.Exit(1)
	}
	if err := db.Migrate(dsn); err != nil {
		fmt.Fprintln(os.Stderr, "migrations:", err)
		os.Exit(1)
	}
	st, err := store.NewDB(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	testDB = st
	code := m.Run()
	st.Close()
	os.Exit(code)
}

// prepareTestDatabase locates a PostgreSQL server, derives the test database
// DSN, and (re)creates the database so every run starts clean. A full
// LIVETRANSLATE_TEST_DATABASE_URL is honored as-is and still gets dropped
// and recreated — never point it at anything you care about.
func prepareTestDatabase(ctx context.Context) string {
	var dsns []string
	if v := os.Getenv("LIVETRANSLATE_TEST_DATABASE_URL"); v != "" {
		dsns = []string{v}
	} else {
		name := os.Getenv("LIVETRANSLATE_TEST_DATABASE_NAME")
		if name == "" {
			name = "livetranslate_go_test"
		}
		dsns = []string{
			fmt.Sprintf("postgres://oo@/%s?host=/tmp&port=5432", name),
			fmt.Sprintf("postgres://oo@/%s?host=localhost&port=5432", name),
		}
	}
	for _, dsn := range dsns {
		admin, ok := recreateDatabase(ctx, dsn)
		if ok {
			return dsn
		}
		_ = admin
	}
	return ""
}

// recreateDatabase drops and creates the database named in dsn by talking to
// the server's "postgres" maintenance database. Returns ok=false when the
// server itself is unreachable.
func recreateDatabase(ctx context.Context, dsn string) (string, bool) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", false
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", false
	}
	q := u.Query()
	host := q.Get("host")
	if host == "" {
		host = u.Hostname()
	}
	port := q.Get("port")
	if port == "" {
		port = u.Port()
	}
	if port == "" {
		port = "5432"
	}
	user := u.User.Username()
	adminDSN := fmt.Sprintf("postgres://%s@/postgres?host=%s&port=%s&sslmode=disable", user, host, port)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return adminDSN, false
	}
	defer admin.Close(ctx)
	// Nobody else may hold the test database; terminate leftovers first.
	for _, stmt := range []string{
		fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()`, name),
		fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, name),
		fmt.Sprintf(`CREATE DATABASE %s`, name),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			fmt.Fprintf(os.Stderr, "test-db setup (%s): %v\n", stmt, err)
			return adminDSN, false
		}
	}
	return adminDSN, true
}

// allTables is truncated (RESTART IDENTITY) before every test so each one
// starts from a pristine database.
var allTables = []string{
	"users", "auth_identities", "password_credentials", "email_challenges",
	"password_reset_tokens", "login_events", "invitations", "admin_accounts",
	"admin_sessions", "audit_events", "devices", "refresh_tokens",
	"classroom_sessions", "transcript_entries", "bookmarks",
	"favorite_sessions", "session_notes", "study_reviews",
	"session_attachments", "sync_changes", "processed_operations",
	"interpreter_conversations", "interpreter_turns",
	"errand_cases", "errand_case_items",
}

func truncateAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := testDB.Pool.Exec(ctx,
		"TRUNCATE "+strings.Join(allTables, ", ")+" RESTART IDENTITY CASCADE")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// captureMailer records every outbound email so tests can read verification
// codes and reset tokens out of the message bodies.
type captureMailer struct {
	mu    sync.Mutex
	sent  []capturedMail
	t     *testing.T
	clock chan time.Time // when non-nil, TimeNow is sourced from here
}

type capturedMail struct{ To, Subject, Body string }

func (c *captureMailer) Send(_ context.Context, msg *mail.Message) error {
	c.mu.Lock()
	c.sent = append(c.sent, capturedMail{msg.To, msg.Subject, msg.Text})
	c.mu.Unlock()
	return nil
}
func (c *captureMailer) Configured() bool { return true }

func (c *captureMailer) last() capturedMail {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		c.t.Fatal("no mail captured")
	}
	return c.sent[len(c.sent)-1]
}

var sixDigits = regexp.MustCompile(`\b(\d{6})\b`)

// verificationCode pulls the six-digit code out of the most recent email.
func (c *captureMailer) verificationCode() string {
	m := sixDigits.FindStringSubmatch(c.last().Body)
	if m == nil {
		c.t.Fatalf("no 6-digit code in mail body: %q", c.last().Body)
	}
	return m[1]
}

// resetToken pulls the opaque reset token out of the latest mail body. The
// token sits alone on its own line in the email.
var resetTokenRe = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_-]{20,})\s*$`)

func (c *captureMailer) resetToken() string {
	m := resetTokenRe.FindStringSubmatch(c.last().Body)
	if m == nil {
		c.t.Fatalf("no reset token in mail body: %q", c.last().Body)
	}
	return m[1]
}

// env is one isolated full-stack server instance.
type env struct {
	t        *testing.T
	cfg      *config.Config
	api      *httptest.Server // /v1 — what iOS talks to
	adminSrv *httptest.Server
	mailer   *captureMailer
}

// newEnv builds a full API + admin stack on a clean database. mutate adjusts
// the config before wiring (rate limits, cooldowns, Argon2 params…).
func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	truncateAll(t)

	cfg := &config.Config{
		DatabaseURL:     "test",
		JWTSecret:       "integration-test-secret",
		JWTIssuer:       "livetranslate-server",
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
		DevLoginEnabled: false,
		// Explicit open registration (the capabilities endpoint and the
		// register gate read this).
		RegistrationMode: config.RegistrationOpen,

		// Real Argon2id, small parameters: fast enough for hundreds of
		// test hashes while exercising the identical code paths.
		Argon2MemoryKiB:  8192,
		Argon2Iterations: 1,
		Argon2Parallel:   1,

		EmailVerifyTTL:   10 * time.Minute,
		PasswordResetTTL: 30 * time.Minute,
		ResendCooldown:   60 * time.Second,

		AuthRateLimitPerMinute: 1000,
		LoginFailWindow:        15 * time.Minute,
		LoginFailMax:           10,
		IPFailMax:              30,

		TombstoneRetentionDays: 180,
		MaxBodyBytes:           10 << 20,
		SchemaVersion:          1,
		MinClientSchemaVersion: 1,
		MaxClientSchemaVersion: 1,

		SessionTTL: 8 * time.Hour,
		DevMode:    true, // Secure=false cookies so httptest can hold them
	}
	if mutate != nil {
		mutate(cfg)
	}
	mailer := &captureMailer{t: t}

	tokens := token.NewManager(cfg)
	auditor := audit.NewRecorder(testDB)
	authSvc := auth.NewService(cfg, testDB, tokens, mailer, auditor)
	syncSvc := syncpkg.NewService(cfg, testDB)

	trust, err := middleware.ParseTrustedProxies(nil)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	authH := authapi.NewHandler(cfg, authSvc, tokens, testDB, trust)
	authH.Register(mux)
	syncapi.NewHandler(cfg, syncSvc, authH).Register(mux)
	accountapi.NewHandler(testDB, authH, authSvc).Register(mux)
	api := httptest.NewServer(httpapi.Handler(mux, cfg.MaxBodyBytes, 30*time.Second))

	admSvc := admin.NewService(cfg, testDB, auditor, authSvc)
	admH := admin.NewHandler(cfg, admSvc)
	admMux := http.NewServeMux()
	admH.Register(admMux)
	admSrv := httptest.NewServer(httpapi.Handler(admMux, 1<<20, 15*time.Second))

	e := &env{t: t, cfg: cfg, api: api, adminSrv: admSrv, mailer: mailer}
	t.Cleanup(api.Close)
	t.Cleanup(admSrv.Close)
	return e
}

// --- HTTP helpers -----------------------------------------------------------

func (e *env) do(method, path, bearer string, body any) (*http.Response, string) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			rd = strings.NewReader(v)
		default:
			b, err := json.Marshal(body)
			if err != nil {
				e.t.Fatal(err)
			}
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, e.api.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp, string(raw)
}

func (e *env) get(path, bearer string) (*http.Response, string) {
	return e.do(http.MethodGet, path, bearer, nil)
}

func decode(t *testing.T, body string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), dst); err != nil {
		t.Fatalf("decode %T from %s: %v", dst, body, err)
	}
}

// --- Account helpers ----------------------------------------------------------

func device(id string) map[string]any {
	return map[string]any{"clientDeviceId": id, "displayName": "测试设备-" + id, "appVersion": "15.0"}
}

// registerAndVerify performs the full signup: register → read the code from
// the captured mail → verify (which returns the first token pair).
func (e *env) registerAndVerify(email, password string) loginResp {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": email, "password": password, "displayName": "测试用户",
		"device": device("dev-" + emailPrefix(email)),
	})
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("register %s: %d %s", email, resp.StatusCode, body)
	}
	code := e.mailer.verificationCode()
	resp, body = e.do(http.MethodPost, "/v1/auth/email/verify", "", map[string]any{
		"email": email, "code": code,
	})
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("verify %s: %d %s", email, resp.StatusCode, body)
	}
	var out loginResp
	decode(e.t, body, &out)
	return out
}

func (e *env) login(email, password, deviceID string) (*http.Response, string, loginResp) {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/v1/auth/login", "", map[string]any{
		"email": email, "password": password, "device": device(deviceID),
	})
	var out loginResp
	if resp.StatusCode == http.StatusOK {
		decode(e.t, body, &out)
	}
	return resp, body, out
}

func emailPrefix(email string) string {
	return strings.SplitN(email, "@", 2)[0]
}

// loginResp mirrors the wire LoginResponse (subset the tests need).
type loginResp struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	TokenType     string `json:"tokenType"`
	ExpiresIn     int    `json:"expiresIn"`
	UserID        string `json:"userId"`
	EmailVerified bool   `json:"emailVerified"`
}

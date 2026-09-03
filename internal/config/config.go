// Package config loads server configuration from environment variables.
// Mirrors the Python service's Settings so the same .env works unchanged.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Environment profile: development | staging | production. Production
	// triggers the static boot validation (see validate.go).
	AppEnv      string
	DatabaseURL string // postgres://… (pgx native format)

	// Auth
	JWTSecret         string
	JWTSecretPrevious string // optional prior secret: verified, never signed with
	JWTIssuer         string // "livetranslate-server"
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	AppleBundleID     string
	AppleJWKSURL      string
	DevLoginEnabled   bool
	RegistrationMode  string // open | invite_only | disabled (single source of truth)
	RequireInvitation bool   // legacy env; folded into RegistrationMode

	// Passwords (Argon2id)
	Argon2MemoryKiB  uint32
	Argon2Iterations uint32
	Argon2Parallel   uint8

	// Email verification / password reset
	EmailVerifyTTL     time.Duration
	PasswordResetTTL   time.Duration
	ResendCooldown     time.Duration
	SMTPHost           string
	SMTPPort           int
	SMTPUsername       string
	SMTPPassword       string
	SMTPFrom           string
	SMTPFromName       string // display name on the From: header
	SMTPTLSMode        string // starttls | smtps | none
	SMTPConnectTimeout time.Duration
	SMTPSendTimeout    time.Duration
	MailpitBaseURL     string // dev-only: log/mailpit capture endpoint

	// Public URLs (email links, AASA). Never a guessed production domain.
	PublicBaseURL     string // https://host — origin only
	PasswordResetPath string // path under PublicBaseURL (default /reset-password)

	// Universal Links / AASA
	AASATeamID string // Apple Developer Team ID; EMPTY = AASA serves an empty app list

	// Rate limiting
	AuthRateLimitPerMinute int
	LoginFailWindow        time.Duration
	LoginFailMax           int // per email before progressive delay
	IPFailMax              int // per IP before short block

	// Sync
	TombstoneRetentionDays int
	MaxBodyBytes           int64
	SchemaVersion          int
	MinClientSchemaVersion int
	MaxClientSchemaVersion int

	// Retention for maintenance cleanup (days; 0 = keep forever).
	LoginEventsRetentionDays int
	AuditRetentionDays       int

	// Admin (cmd/admin). ADMIN_ALLOW_WILDCARD_BIND is the explicit
	// operator opt-in for containerized wildcard binds whose port mapping
	// is only reachable on a controlled internal network.
	AdminListenAddr        string
	AdminAllowWildcardBind bool
	SessionTTL             time.Duration

	// Observability. Both default OFF; when enabled, /metrics is served on
	// the API listener (gate it at the reverse proxy) and pprof only answers
	// loopback clients.
	MetricsEnabled bool
	PprofEnabled   bool
	LogLevel       string // debug | info | warn | error

	// Server
	ListenAddr     string
	TrustedProxies []string
	CORSOrigins    []string
	DevMode        bool // enables /v1/auth/dev + verbose logs; NEVER on prod
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "true" || v == "1" || v == "yes"
	}
	return fallback
}

func envDur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		// bare seconds
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
}

// Load reads the environment. Fails when the JWT secret is the documented
// placeholder (the Python service had the same refuse-to-serve guard).
func Load() (*Config, error) {
	c := &Config{
		AppEnv:      envProfile("APP_ENV", EnvDevelopment),
		DatabaseURL: env("DATABASE_URL", ""),

		JWTSecret:         env("JWT_SECRET", "REPLACE_WITH_RANDOM_SECRET"),
		JWTSecretPrevious: env("JWT_SECRET_PREVIOUS", ""),
		JWTIssuer:         env("JWT_ISSUER", "livetranslate-server"),
		AccessTokenTTL:    envDur("ACCESS_TOKEN_TTL_SECONDS", 15*time.Minute),
		RefreshTokenTTL:   envDur("REFRESH_TOKEN_TTL_SECONDS", 30*24*time.Hour),
		AppleBundleID:     env("APPLE_BUNDLE_ID", "com.livetranslate.ios"),
		AppleJWKSURL:      env("APPLE_JWKS_URL", "https://appleid.apple.com/auth/keys"),
		DevLoginEnabled:   envBool("DEV_LOGIN_ENABLED", false),
		RequireInvitation: envBool("REQUIRE_INVITATION", false),

		// Argon2id: 64 MiB, t=3, p=1. Calibrated ~120-200ms on a small
		// ARM64 server while staying far below memory exhaustion for
		// concurrent logins (p=1, 64MiB per verification).
		Argon2MemoryKiB:  uint32(envInt("ARGON2_MEMORY_KIB", 65536)),
		Argon2Iterations: uint32(envInt("ARGON2_ITERATIONS", 3)),
		Argon2Parallel:   uint8(envInt("ARGON2_PARALLELISM", 1)),

		EmailVerifyTTL:     envDur("EMAIL_VERIFY_TTL", 10*time.Minute),
		PasswordResetTTL:   envDur("PASSWORD_RESET_TTL", 30*time.Minute),
		ResendCooldown:     envDur("RESEND_COOLDOWN", 60*time.Second),
		SMTPHost:           env("SMTP_HOST", ""),
		SMTPPort:           envInt("SMTP_PORT", 587),
		SMTPUsername:       env("SMTP_USERNAME", ""),
		SMTPPassword:       env("SMTP_PASSWORD", ""),
		SMTPFrom:           env("SMTP_FROM", ""),
		SMTPFromName:       env("SMTP_FROM_NAME", "LiveTranslate"),
		SMTPTLSMode:        env("SMTP_TLS_MODE", SMTPStartTLS),
		SMTPConnectTimeout: envDur("SMTP_CONNECT_TIMEOUT", 15*time.Second),
		SMTPSendTimeout:    envDur("SMTP_SEND_TIMEOUT", 20*time.Second),
		MailpitBaseURL:     env("MAILPIT_BASE_URL", ""),

		PublicBaseURL:     strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		PasswordResetPath: env("PASSWORD_RESET_PATH", "/reset-password"),

		AASATeamID: env("AASA_TEAM_ID", ""),

		AuthRateLimitPerMinute: envInt("AUTH_RATE_LIMIT_PER_MINUTE", 30),
		LoginFailWindow:        envDur("LOGIN_FAIL_WINDOW", 15*time.Minute),
		LoginFailMax:           envInt("LOGIN_FAIL_MAX", 10),
		IPFailMax:              envInt("IP_FAIL_MAX", 30),

		TombstoneRetentionDays: envInt("TOMBSTONE_RETENTION_DAYS", 180),
		MaxBodyBytes:           int64(envInt("MAX_BODY_BYTES", 10*1024*1024)),
		SchemaVersion:          envInt("SCHEMA_VERSION", 1),
		MinClientSchemaVersion: envInt("MIN_CLIENT_SCHEMA_VERSION", 1),
		MaxClientSchemaVersion: envInt("MAX_CLIENT_SCHEMA_VERSION", 1),

		LoginEventsRetentionDays: envInt("LOGIN_EVENTS_RETENTION_DAYS", 90),
		AuditRetentionDays:       envInt("AUDIT_RETENTION_DAYS", 365),

		AdminListenAddr:        env("ADMIN_LISTEN_ADDR", "127.0.0.1:8081"),
		AdminAllowWildcardBind: envBool("ADMIN_ALLOW_WILDCARD_BIND", false),
		SessionTTL:             envDur("ADMIN_SESSION_TTL", 8*time.Hour),

		MetricsEnabled: envBool("METRICS_ENABLED", false),
		PprofEnabled:   envBool("PPROF_ENABLED", false),
		LogLevel:       env("LOG_LEVEL", "info"),

		ListenAddr:     env("LISTEN_ADDR", "127.0.0.1:8000"),
		TrustedProxies: splitCSV(env("TRUSTED_PROXIES", "")),
		CORSOrigins:    splitCSV(env("CORS_ORIGINS", "")),
		DevMode:        envBool("DEV_MODE", false),
	}
	if c.JWTSecret == "REPLACE_WITH_RANDOM_SECRET" {
		return nil, fmt.Errorf("JWT_SECRET is the placeholder — refusing to serve auth (generate with: openssl rand -hex 32)")
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required (postgres://…)")
	}
	c.RegistrationMode = resolveRegistrationMode(env("REGISTRATION_MODE", ""), c.RequireInvitation)
	if err := c.ValidateProduction(); err != nil {
		return nil, err
	}
	return c, nil
}

func envProfile(key, fallback string) string {
	v := strings.ToLower(strings.TrimSpace(env(key, fallback)))
	switch v {
	case EnvProduction, "prod":
		return EnvProduction
	case EnvStaging, "stage":
		return EnvStaging
	default:
		return EnvDevelopment
	}
}

// resolveRegistrationMode unifies REGISTRATION_MODE (preferred) with the
// legacy REQUIRE_INVITATION bool. An explicit REGISTRATION_MODE always wins.
func resolveRegistrationMode(mode string, legacyInvitation bool) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case RegistrationOpen:
		return RegistrationOpen
	case RegistrationInviteOnly, "invite", "inviteonly":
		return RegistrationInviteOnly
	case RegistrationDisabled, "closed":
		return RegistrationDisabled
	case "":
		if legacyInvitation {
			return RegistrationInviteOnly
		}
		return RegistrationOpen
	default:
		// Unknown value: fail CLOSED (never accept registrations because of
		// a typo in the env var).
		return RegistrationDisabled
	}
}

// ResetLinkURL builds the password-reset link for emails. Empty when no
// public URL is configured (the mail then falls back to the bare token,
// matching the legacy manual-paste flow).
func (c *Config) ResetLinkURL(token string) string {
	if c.PublicBaseURL == "" || c.PasswordResetPath == "" {
		return ""
	}
	return c.PublicBaseURL + c.PasswordResetPath + "?token=" + token
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range splitComma(s) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// RandomHex returns n random bytes hex-encoded (crypto/rand).
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

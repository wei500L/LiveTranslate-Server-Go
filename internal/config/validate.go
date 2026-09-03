// Package config loads server configuration from environment variables.
// Mirrors the Python service's Settings so the same .env works unchanged.
package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Registration modes — the single source of truth surfaced through
// GET /v1/auth/capabilities and the admin dashboard (read-only there).
const (
	RegistrationOpen       = "open"
	RegistrationInviteOnly = "invite_only"
	RegistrationDisabled   = "disabled"
)

// SMTPTLSMode selects the transport security negotiation.
const (
	SMTPStartTLS  = "starttls" // upgrade a plaintext connection (587)
	SMTPImplicit  = "smtps"    // TLS from the first byte (465)
	SMTPPlainNone = "none"     // dev relay only; production refuses it
)

// Environment profiles. APP_ENV selects the boot-time validation posture.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// placeholders that must never survive into a production configuration.
var placeholderMarkers = []string{
	"REPLACE_WITH", "CHANGEME", "changeme", "example.com", "yourdomain",
	"INSERT_", "TODO",
}

func isPlaceholder(v string) bool {
	for _, m := range placeholderMarkers {
		if strings.Contains(v, m) {
			return true
		}
	}
	return false
}

// ValidateProduction enforces the production boot contract statically (no
// network, no DB). It returns every violation so operators see the full
// list at once instead of fixing them one boot at a time.
func (c *Config) ValidateProduction() error {
	if c.AppEnv != EnvProduction {
		return nil
	}
	var problems []string
	if c.JWTSecret == "" || isPlaceholder(c.JWTSecret) || len(c.JWTSecret) < 32 {
		problems = append(problems, "JWT_SECRET must be a generated value (≥32 chars), not a placeholder")
	}
	if c.DatabaseURL == "" || isPlaceholder(c.DatabaseURL) {
		problems = append(problems, "DATABASE_URL is required")
	}
	if c.DevMode {
		problems = append(problems, "DEV_MODE must be false in production")
	}
	if c.DevLoginEnabled {
		problems = append(problems, "DEV_LOGIN_ENABLED must be false in production")
	}
	if c.MailpitBaseURL != "" {
		problems = append(problems, "MAILPIT_BASE_URL (dev-only mail capture) must be unset in production")
	}
	if c.PublicBaseURL == "" {
		problems = append(problems, "PUBLIC_BASE_URL is required (email links point at it)")
	} else if u, err := url.Parse(c.PublicBaseURL); err != nil || u.Scheme != "https" || u.Host == "" {
		problems = append(problems, "PUBLIC_BASE_URL must be an HTTPS origin (https://host)")
	}
	if c.SMTPHost == "" || c.SMTPFrom == "" {
		problems = append(problems, "SMTP_HOST and SMTP_FROM are required in production (no silent code swallowing)")
	}
	if c.SMTPTLSMode == SMTPPlainNone {
		problems = append(problems, "SMTP_TLS_MODE=none is not allowed in production")
	}
	if strings.HasPrefix(c.ListenAddr, "0.0.0.0") || strings.HasPrefix(c.ListenAddr, ":") {
		// Binding wide is fine behind a local reverse proxy inside the
		// container/host network; the API itself must still not be
		// published directly. Compose maps the port explicitly, so a wide
		// bind is only flagged when TRUSTED_PROXIES is unset — that
		// combination means "publicly reachable AND no proxy awareness".
		if len(c.TrustedProxies) == 0 && len(c.CORSOrigins) == 0 {
			problems = append(problems, "LISTEN_ADDR binds all interfaces with no TRUSTED_PROXIES/CORS — expose via a reverse proxy instead")
		}
	}
	if c.AdminListenAddr != "" && !strings.HasPrefix(c.AdminListenAddr, "127.0.0.1") {
		// The admin UI has no public-facing auth surface by design; keep it
		// loopback-bound (front it with an access-controlled proxy when
		// remote access is needed).
		problems = append(problems, "ADMIN_LISTEN_ADDR should stay on 127.0.0.1 (proxy with an IP allowlist for remote access)")
	}
	if len(problems) > 0 {
		return fmt.Errorf("production configuration rejected:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// Package config loads server configuration from environment variables.
// Mirrors the Python service's Settings so the same .env works unchanged.
package config

import (
	"fmt"
	"net"
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
//
// Container topology is LEGAL: the API may bind 0.0.0.0 inside the
// container — whether it is reachable from the internet is decided by the
// Compose port mappings / reverse proxy / firewall, not by the bind. What
// this validation constrains is the surfaces that must not be public.
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
	if err := validateAdminBind(c.AdminListenAddr, c.AdminAllowWildcardBind); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("production configuration rejected:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// validateAdminBind constrains the admin listener: loopback, a
// private/RFC1918 address (a controlled internal network), or an explicit
// wildcard opt-in for containerized deployments whose port mapping is NOT
// public (the operator asserts that with ADMIN_ALLOW_WILDCARD_BIND=true).
// A wildcard bind without the opt-in — or a bind to a specific PUBLIC
// address — is refused: the admin surface has no public-facing auth design.
func validateAdminBind(addr string, allowWildcard bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ADMIN_LISTEN_ADDR %q is not host:port", addr)
	}
	if host == "" {
		// ":8081" binds all interfaces — same rule as 0.0.0.0.
		if !allowWildcard {
			return fmt.Errorf("ADMIN_LISTEN_ADDR binds all interfaces; in production bind 127.0.0.1 or a private address, or set ADMIN_ALLOW_WILDCARD_BIND=true when the port is only reachable on a controlled internal network")
		}
		return nil
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return fmt.Errorf("ADMIN_LISTEN_ADDR host %q is not an IP address", host)
	case ip.IsLoopback():
		return nil
	case ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified():
		if ip.IsUnspecified() && !allowWildcard {
			return fmt.Errorf("ADMIN_LISTEN_ADDR binds all interfaces; in production bind 127.0.0.1 or a private address, or set ADMIN_ALLOW_WILDCARD_BIND=true when the port is only reachable on a controlled internal network")
		}
		return nil
	default:
		return fmt.Errorf("ADMIN_LISTEN_ADDR must be loopback or a private address in production (got %s)", host)
	}
}

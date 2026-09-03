// Package mail is the outbound-email abstraction. SMTP is the production
// implementation; a logging implementation covers local dev when no SMTP is
// configured (register then FAILS LOUDLY in prod mode — see Sender rules).
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"log/slog"

	"livetranslate/server/internal/config"
)

// Sender sends a plain-text email. Implementations must never log the body.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
	// Configured reports whether real delivery is possible.
	Configured() bool
}

// SMTPSender talks to the configured relay. STARTTLS when SMTP_USE_TLS.
type SMTPSender struct {
	cfg *config.Config
}

func NewSMTPSender(cfg *config.Config) *SMTPSender { return &SMTPSender{cfg: cfg} }

func (s *SMTPSender) Configured() bool {
	return s.cfg.SMTPHost != "" && s.cfg.SMTPFrom != ""
}

func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	if !s.Configured() {
		return fmt.Errorf("SMTP not configured")
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.SMTPHost, s.cfg.SMTPPort)
	host := s.cfg.SMTPHost

	var auth smtp.Auth
	if s.cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", s.cfg.SMTPUsername, s.cfg.SMTPPassword, host)
	}

	msg := strings.Join([]string{
		"From: " + s.cfg.SMTPFrom,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}, "\r\n")

	dialer := &netDialer{timeout: 15 * time.Second, tlsCfg: &tls.Config{ServerName: host}}
	conn, err := dialer.dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer conn.Close()

	c, err := newSMTPClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	defer c.Close()
	if s.cfg.SMTPUseTLS && c.extStartTLS() {
		if err := c.startTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if auth != nil && c.extAuth() {
		if err := c.auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.mail(s.cfg.SMTPFrom); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	if err := c.rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close-data: %w", err)
	}
	return c.quit()
}

// LogSender is the DEV fallback: logs that a mail WOULD be sent (never the
// body — it may contain a verification code). It reports Configured() only
// in DevMode, so production boots without SMTP refuse registration with
// ErrNoMailTransport instead of silently swallowing codes.
type LogSender struct {
	devMode bool
	logged  chan string // buffered capture for tests/dev tooling
}

func NewLogSender(devMode bool) *LogSender {
	return &LogSender{devMode: devMode, logged: make(chan string, 64)}
}

func (s *LogSender) Configured() bool { return s.devMode }

func (s *LogSender) Send(ctx context.Context, to, subject, body string) error {
	slog.Info("dev mail sender: message recorded", "to", to, "subject", subject)
	select {
	case s.logged <- to + "|" + subject:
	default:
	}
	return nil
}

// Captured drains captured (to, subject) pairs — used by tests.
func (s *LogSender) Captured() []string {
	var out []string
	for {
		select {
		case m := <-s.logged:
			out = append(out, m)
		default:
			return out
		}
	}
}

// MailpitSender posts messages to a local Mailpit instance's HTTP API
// (POST {base}/api/v1/send). Dev/E2E only: Mailpit keeps no secrets and
// must never be reachable from the internet. Codes become retrievable from
// Mailpit's web UI / API, which is exactly what integration tests need.
type MailpitSender struct {
	baseURL string
	client  *http.Client
}

func NewMailpitSender(baseURL string) *MailpitSender {
	return &MailpitSender{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *MailpitSender) Configured() bool { return true }

func (s *MailpitSender) Send(ctx context.Context, to, subject, body string) error {
	// Mailpit's /api/v1/send requires object-form addresses.
	payload := map[string]any{
		"From":    map[string]string{"Email": "livetranslate-dev@localhost", "Name": "LiveTranslate (dev)"},
		"To":      []map[string]string{{"Email": to}},
		"Subject": subject,
		"Text":    body,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/api/v1/send", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("mailpit send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mailpit send: status %d", resp.StatusCode)
	}
	return nil
}

// NewSender picks the implementation: SMTP when configured; else Mailpit
// when MAILPIT_BASE_URL is set (dev); else the dev log sender. Production
// must configure SMTP — anything else yields Configured()==false unless
// DevMode is on, and the auth service refuses to register in that case.
func NewSender(cfg *config.Config) Sender {
	if cfg.SMTPHost != "" && cfg.SMTPFrom != "" {
		return NewSMTPSender(cfg)
	}
	if cfg.MailpitBaseURL != "" {
		return NewMailpitSender(cfg.MailpitBaseURL)
	}
	return NewLogSender(cfg.DevMode)
}

// Package mail is the outbound-email abstraction: a small template set
// (text + HTML), an SMTP production implementation with STARTTLS/implicit
// TLS, and dev implementations (log capture, Mailpit). SMTP is the only
// production transport — no vendor-specific SDKs by design.
//
// Logging discipline: sender implementations log destinations and subjects
// at most, NEVER bodies (they carry one-time codes and reset links).
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"log/slog"

	"livetranslate/server/internal/config"
	"livetranslate/server/internal/metrics"
)

// Sender sends a rendered message. Implementations must never log the body.
type Sender interface {
	Send(ctx context.Context, msg *Message) error
	// Configured reports whether real delivery is possible.
	Configured() bool
}

// --- SMTP (production) ---------------------------------------------------------

// SMTPSender talks to the configured relay over STARTTLS (587), implicit
// TLS (465) or plaintext (dev only — production validation refuses "none").
type SMTPSender struct {
	cfg *config.Config
}

func NewSMTPSender(cfg *config.Config) *SMTPSender { return &SMTPSender{cfg: cfg} }

func (s *SMTPSender) Configured() bool {
	return s.cfg.SMTPHost != "" && s.cfg.SMTPFrom != ""
}

func (s *SMTPSender) Send(ctx context.Context, msg *Message) error {
	if !s.Configured() {
		return fmt.Errorf("SMTP not configured")
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.SMTPHost, s.cfg.SMTPPort)
	host := s.cfg.SMTPHost

	var auth smtpAuth
	if s.cfg.SMTPUsername != "" {
		auth = plainAuth("", s.cfg.SMTPUsername, s.cfg.SMTPPassword, host)
	}

	wire := s.buildMessage(msg)

	dialer := &netDialer{timeout: s.cfg.SMTPConnectTimeout}
	var conn netConn
	var err error
	if s.cfg.SMTPTLSMode == config.SMTPImplicit {
		// TLS from the first byte (port 465 style).
		conn, err = dialer.dialTLS(addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.dial(addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer conn.Close()

	c, err := newSMTPClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	defer c.Close()
	if err := c.deadline(time.Now().Add(s.cfg.SMTPSendTimeout)); err != nil {
		return fmt.Errorf("smtp timeout: %w", err)
	}
	if s.cfg.SMTPTLSMode == config.SMTPStartTLS && c.extStartTLS() {
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
	if err := c.rcpt(msg.To); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(wire)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close-data: %w", err)
	}
	return c.quit()
}

// buildMessage assembles the MIME multipart/alternative wire format: the
// From display name is RFC 2047 encoded when it carries non-ASCII.
func (s *SMTPSender) buildMessage(msg *Message) string {
	from := s.cfg.SMTPFrom
	if s.cfg.SMTPFromName != "" {
		from = rfc2047Encode(s.cfg.SMTPFromName) + " <" + s.cfg.SMTPFrom + ">"
	}
	headers := []string{
		"From: " + from,
		"To: " + msg.To,
		"Subject: " + rfc2047EncodeSubject(msg.Subject),
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="ltmail"`,
	}
	body := strings.Join([]string{
		"--ltmail",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: base64",
		"",
		wrap76(base64.StdEncoding.EncodeToString([]byte(msg.Text))),
		"--ltmail",
		"Content-Type: text/html; charset=UTF-8",
		"Content-Transfer-Encoding: base64",
		"",
		wrap76(base64.StdEncoding.EncodeToString([]byte(msg.HTML))),
		"--ltmail--",
		"",
	}, "\r\n")
	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body
}

func rfc2047Encode(s string) string {
	if isASCII(s) {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// rfc2047EncodeSubject keeps an ASCII subject verbatim (ours are already
// "prefix 中文 / English" shaped) and encodes only when necessary.
func rfc2047EncodeSubject(s string) string { return rfc2047Encode(s) }

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7e || s[i] < 0x20 {
			return false
		}
	}
	return true
}

// wrap76 splits base64 into 76-column lines per RFC 2045.
func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

// --- Dev senders --------------------------------------------------------------

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

func (s *LogSender) Send(ctx context.Context, msg *Message) error {
	slog.Info("dev mail sender: message recorded", "to", msg.To, "subject", msg.Subject)
	select {
	case s.logged <- msg.To + "|" + msg.Subject:
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

func (s *MailpitSender) Send(ctx context.Context, msg *Message) error {
	// Mailpit's /api/v1/send requires object-form addresses.
	payload := map[string]any{
		"From":    map[string]string{"Email": "livetranslate-dev@localhost", "Name": "LiveTranslate (dev)"},
		"To":      []map[string]string{{"Email": msg.To}},
		"Subject": msg.Subject,
		"Text":    msg.Text,
		"HTML":    msg.HTML,
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

// --- MeteredSender ---------------------------------------------------------------

// MeteredSender wraps any Sender with delivery counters for /metrics and
// the admin dashboard (aggregate counts only — no addresses).
type MeteredSender struct {
	inner Sender
}

func NewMeteredSender(inner Sender) Sender {
	return &MeteredSender{inner: inner}
}

func (s *MeteredSender) Configured() bool { return s.inner.Configured() }

func (s *MeteredSender) Send(ctx context.Context, msg *Message) error {
	err := s.inner.Send(ctx, msg)
	if err != nil {
		metrics.Inc(metrics.MailFailedTotal)
	} else {
		metrics.Inc(metrics.MailSentTotal)
	}
	return err
}

// NewSender picks the implementation: SMTP when configured; else Mailpit
// when MAILPIT_BASE_URL is set (dev); else the dev log sender. Production
// must configure SMTP — anything else yields Configured()==false unless
// DevMode is on, and the auth service refuses to register in that case.
// The chosen sender is always wrapped for metrics.
func NewSender(cfg *config.Config) Sender {
	var inner Sender
	if cfg.SMTPHost != "" && cfg.SMTPFrom != "" {
		inner = NewSMTPSender(cfg)
	} else if cfg.MailpitBaseURL != "" {
		inner = NewMailpitSender(cfg.MailpitBaseURL)
	} else {
		inner = NewLogSender(cfg.DevMode)
	}
	return NewMeteredSender(inner)
}

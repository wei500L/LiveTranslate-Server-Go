package mail

import (
	"crypto/tls"
	"net"
	"net/smtp"
	"time"
)

// Thin adapters over net/smtp so the sender above stays testable.

type smtpAuth = smtp.Auth

func plainAuth(identity, username, password, host string) smtpAuth {
	return smtp.PlainAuth(identity, username, password, host)
}

// netConn is satisfied by both a plaintext and a TLS connection.
type netConn = net.Conn

type netDialer struct {
	timeout time.Duration
}

func (d *netDialer) dial(addr string) (netConn, error) {
	return net.DialTimeout("tcp", addr, d.timeout)
}

// dialTLS connects with TLS from the first byte (implicit-TLS relays).
func (d *netDialer) dialTLS(addr string, cfg *tls.Config) (netConn, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: d.timeout}}
	return dialer.Dial("tcp", addr)
}

type smtpClient struct {
	c *smtp.Client
}

func newSMTPClient(conn netConn, host string) (*smtpClient, error) {
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return nil, err
	}
	return &smtpClient{c: c}, nil
}

// deadline bounds the whole send phase (auth → DATA → QUIT).
func (w *smtpClient) deadline(t time.Time) error {
	return w.c.Conn().SetDeadline(t)
}

func (w *smtpClient) extStartTLS() bool {
	ok, _ := w.c.Extension("STARTTLS")
	return ok
}

func (w *smtpClient) extAuth() bool {
	ok, _ := w.c.Extension("AUTH")
	return ok
}
func (w *smtpClient) startTLS(cfg *tls.Config) error { return w.c.StartTLS(cfg) }
func (w *smtpClient) auth(a smtpAuth) error          { return w.c.Auth(a) }
func (w *smtpClient) mail(from string) error         { return w.c.Mail(from) }
func (w *smtpClient) rcpt(to string) error           { return w.c.Rcpt(to) }
func (w *smtpClient) data() (writeCloser, error)     { return w.c.Data() }
func (w *smtpClient) quit() error                    { return w.c.Quit() }
func (w *smtpClient) Close() error                   { return w.c.Close() }

type writeCloser interface {
	Write([]byte) (int, error)
	Close() error
}

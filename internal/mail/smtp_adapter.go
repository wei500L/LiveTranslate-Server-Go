package mail

import (
	"crypto/tls"
	"net"
	"net/smtp"
	"time"
)

// Thin adapters over net/smtp so the sender above stays testable.

type netDialer struct {
	timeout time.Duration
	tlsCfg  *tls.Config
}

func (d *netDialer) dial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, d.timeout)
}

type smtpClient struct {
	c *smtp.Client
}

func newSMTPClient(conn net.Conn, host string) (*smtpClient, error) {
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return nil, err
	}
	return &smtpClient{c: c}, nil
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
func (w *smtpClient) auth(a smtp.Auth) error         { return w.c.Auth(a) }
func (w *smtpClient) mail(from string) error         { return w.c.Mail(from) }
func (w *smtpClient) rcpt(to string) error           { return w.c.Rcpt(to) }
func (w *smtpClient) data() (writeCloser, error)     { return w.c.Data() }
func (w *smtpClient) quit() error                    { return w.c.Quit() }
func (w *smtpClient) Close() error                   { return w.c.Close() }

type writeCloser interface {
	Write([]byte) (int, error)
	Close() error
}

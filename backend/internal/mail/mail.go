// Package mail is the iteration-1 transactional email sink (plan step
// 8.1): stdlib + enmime only, per-send dialing (no pooling), one of three
// TLS policies (none | starttls | implicit), and a NopSender fallback when
// no SMTP host is configured. Exactly-once is the caller's job — the
// bootstrap's RowsAffected flag and the settings handler's OnboardedAt
// check — this package only sends.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"time"

	"finance-tracker/internal/config"

	"github.com/jhillyerd/enmime"
)

// DefaultFrom is the MAIL FROM / From header used when SMTP_FROM is empty.
const DefaultFrom = "Personal Finance Tracker <finance-tracker@localhost>"

// dialTimeout bounds each per-send connection attempt.
const dialTimeout = 10 * time.Second

// Message is one transactional email. To is a bare address; Text is the
// plain-text body (non-empty); HTML is the HTML body (inline CSS only).
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers one Message. Implementations must be safe for
// concurrent use.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// NopSender drops messages after logging the subject at info level. New
// returns it when no SMTP host is configured so callers can Send
// unconditionally.
type NopSender struct{}

// Send logs the subject and returns nil without dialing anything.
func (NopSender) Send(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "mail disabled; dropping message", "to", m.To, "subject", m.Subject)
	return nil
}

// New returns a Sender for cfg. An empty Host selects NopSender; any other
// unsupported TLS policy is rejected up front (config.Load already
// validates, this guards direct construction).
func New(cfg config.SMTP) Sender {
	if cfg.Host == "" {
		return NopSender{}
	}
	switch cfg.TLSPolicy {
	case "none", "starttls", "implicit":
	default:
		// Fail loud at construction, not mid-send.
		return &smtpSender{cfg: config.SMTP{Host: cfg.Host, TLSPolicy: ""}, err: fmt.Errorf("mail: unsupported SMTP_TLS_POLICY %q", cfg.TLSPolicy)}
	}
	if cfg.Port == 0 {
		cfg.Port = 25
	}
	return &smtpSender{cfg: cfg}
}

// smtpSender dials once per Send (iteration 1 has no pooling).
type smtpSender struct {
	cfg config.SMTP
	// err, when set, makes every Send return it (construction-time
	// validation failure).
	err error
}

// Send delivers m over a fresh connection: greeting, optional AUTH
// (only when a user is configured), MAIL FROM, RCPT TO, DATA (enmime
// multipart/alternative, text part first), QUIT. The first SMTP error is
// returned wrapped.
func (s *smtpSender) Send(ctx context.Context, m Message) error {
	if s.err != nil {
		return s.err
	}
	if m.To == "" {
		return fmt.Errorf("mail: message has no recipient")
	}
	from := s.cfg.From
	if from == "" {
		from = DefaultFrom
	}
	body, err := encodeBody(from, m)
	if err != nil {
		return fmt.Errorf("mail: %s: %w", m.To, err)
	}
	client, err := s.client(ctx)
	if err != nil {
		return fmt.Errorf("mail: %s: %w", m.To, err)
	}
	defer client.Close()
	if s.cfg.User != "" {
		auth := smtp.PlainAuth("", s.cfg.User, s.cfg.Pass, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mail: %s: auth: %w", m.To, err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mail: %s: mail from: %w", m.To, err)
	}
	if err := client.Rcpt(m.To); err != nil {
		return fmt.Errorf("mail: %s: rcpt to: %w", m.To, err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: %s: data: %w", m.To, err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mail: %s: write body: %w", m.To, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: %s: end data: %w", m.To, err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("mail: %s: quit: %w", m.To, err)
	}
	slog.InfoContext(ctx, "mail sent", "to", m.To, "subject", m.Subject)
	return nil
}

// client opens the connection per TLSPolicy:
//
//   - none:     plain TCP, plaintext SMTP.
//   - starttls: plain TCP, upgrade when the server advertises STARTTLS;
//     Mailpit-style plain sinks that do not advertise it stay plaintext.
//   - implicit: TCP then a TLS handshake (TLS 1.2 minimum) before the
//     SMTP greeting.
func (s *smtpSender) client(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	switch s.cfg.TLSPolicy {
	case "none":
		return smtp.NewClient(conn, s.cfg.Host)
	case "starttls":
		client, err := smtp.NewClient(conn, s.cfg.Host)
		if err != nil {
			return nil, err
		}
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return client, nil // plain sink: stay plaintext
		}
		if err := client.StartTLS(nil); err != nil {
			client.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
		return client, nil
	case "implicit":
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: s.cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		return smtp.NewClient(tlsConn, s.cfg.Host)
	default:
		conn.Close()
		return nil, fmt.Errorf("unsupported SMTP_TLS_POLICY %q", s.cfg.TLSPolicy)
	}
}

// splitNameAddress parses a "Name <addr>" or bare-address From into its
// parts for the MIME From header (the wire MAIL FROM keeps the raw
// configured string).
func splitNameAddress(from string) (name, addr string) {
	if p, err := mail.ParseAddress(from); err == nil {
		return p.Name, p.Address
	}
	return "", from
}

// encodeBody serializes m as an enmime multipart/alternative message: the
// plain-text part FIRST, then the HTML part (enmime's Build order), with
// standard headers (From, To, Subject, Date, MIME-Version).
func encodeBody(from string, m Message) ([]byte, error) {
	name, addr := splitNameAddress(from)
	part, err := enmime.Builder().
		From(name, addr).
		To("", m.To).
		Subject(m.Subject).
		Text([]byte(m.Text)).
		HTML([]byte(m.HTML)).
		Build()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := part.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

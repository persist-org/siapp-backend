package mail

import (
	"context"
	"errors"
	"fmt"
	"time"

	gomail "github.com/wneessen/go-mail"
)

// TLSMode says how the connection to the SMTP server is secured.
type TLSMode string

// The TLS modes accepted in SMTP_TLS.
const (
	// TLSNone sends in plain text. Only for a local catcher such as Mailpit.
	TLSNone TLSMode = "none"
	// TLSStartTLS upgrades a plain connection with STARTTLS (usually port 587) and refuses
	// to continue if the server does not offer it.
	TLSStartTLS TLSMode = "starttls"
	// TLSImplicit uses TLS from the first byte (usually port 465).
	TLSImplicit TLSMode = "tls"
)

// ParseTLSMode converts the SMTP_TLS setting.
func ParseTLSMode(s string) (TLSMode, error) {
	switch m := TLSMode(s); m {
	case TLSNone, TLSStartTLS, TLSImplicit:
		return m, nil
	}
	return "", fmt.Errorf("SMTP_TLS must be none, starttls or tls, got %q", s)
}

// SMTPConfig is the connection to the mail server.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string // empty: no authentication
	Password string
	// From is the sender, e.g. "SiAPP <noreply@example.com>".
	From string
	TLS  TLSMode
}

// SMTPSender sends through an SMTP server with go-mail.
type SMTPSender struct{ cfg SMTPConfig }

const smtpTimeout = 15 * time.Second

// NewSMTPSender returns an SMTPSender. It checks the configuration but does not connect.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if cfg.Host == "" {
		return nil, errors.New("smtp: host is required")
	}
	probe := gomail.NewMsg()
	if err := probe.From(cfg.From); err != nil {
		return nil, fmt.Errorf("smtp: invalid From address %q: %w", cfg.From, err)
	}
	return &SMTPSender{cfg: cfg}, nil
}

// Send delivers m over a new connection. A connection per message is fine at this volume
// (password resets) and keeps the sender free of connection-state bugs.
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	msg := gomail.NewMsg()
	if err := msg.From(s.cfg.From); err != nil {
		return fmt.Errorf("set sender: %w", err)
	}
	if err := msg.To(m.To); err != nil {
		return fmt.Errorf("set recipient: %w", err)
	}
	msg.Subject(m.Subject)
	msg.SetBodyString(gomail.TypeTextPlain, m.Text)
	msg.AddAlternativeString(gomail.TypeTextHTML, m.HTML)

	opts := []gomail.Option{gomail.WithPort(s.cfg.Port), gomail.WithTimeout(smtpTimeout)}
	switch s.cfg.TLS {
	case TLSNone:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	case TLSStartTLS:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	case TLSImplicit:
		opts = append(opts, gomail.WithSSL())
	}
	if s.cfg.Username != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(s.cfg.Username),
			gomail.WithPassword(s.cfg.Password))
	}

	client, err := gomail.NewClient(s.cfg.Host, opts...)
	if err != nil {
		return fmt.Errorf("create smtp client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	return nil
}

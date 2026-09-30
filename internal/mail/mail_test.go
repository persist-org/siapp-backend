package mail_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/mail"
)

const resetLink = "https://app.example.com/reset-password?token=abc_DEF-123&x=1"

func TestPasswordResetMessage(t *testing.T) {
	msg, err := mail.NewPasswordResetMessage("budi@example.com", "Budi Santoso", resetLink, time.Hour)
	require.NoError(t, err)

	require.Equal(t, "budi@example.com", msg.To)
	require.Equal(t, "Atur ulang kata sandi SiAPP", msg.Subject)

	for name, body := range map[string]string{"text": msg.Text, "html": msg.HTML} {
		require.Contains(t, body, "Budi Santoso", name)
		require.Contains(t, body, "1 jam", name)
		require.Contains(t, body, "abc_DEF-123", name, "the token is in the link")
	}
	require.Contains(t, msg.Text, resetLink, "plain text shows the link exactly as given")
	require.NotContains(t, msg.Text, "&amp;", "text/template must not HTML-escape")
	require.Contains(t, msg.HTML, `href="`, "the HTML has a real link")
}

func TestPasswordResetMessageEscapesTheNameInHTMLOnly(t *testing.T) {
	msg, err := mail.NewPasswordResetMessage("x@example.com", `<script>alert(1)</script>`, resetLink, time.Hour)
	require.NoError(t, err)

	require.NotContains(t, msg.HTML, "<script>alert(1)</script>", "a hostile name must not become markup")
	require.Contains(t, msg.HTML, "&lt;script&gt;")
	require.Contains(t, msg.Text, "<script>alert(1)</script>", "plain text is shown as it is")
}

func TestPasswordResetMessageValidity(t *testing.T) {
	tests := map[time.Duration]string{
		time.Hour:        "1 jam",
		2 * time.Hour:    "2 jam",
		30 * time.Minute: "30 menit",
		90 * time.Minute: "90 menit",
	}
	for d, want := range tests {
		msg, err := mail.NewPasswordResetMessage("x@example.com", "A", resetLink, d)
		require.NoError(t, err)
		require.Contains(t, msg.Text, want, d.String())
	}
}

func TestFakeSender(t *testing.T) {
	f := mail.NewFakeSender()
	require.Empty(t, f.Sent())

	require.NoError(t, f.Send(t.Context(), mail.Message{To: "a@example.com"}))
	require.NoError(t, f.Send(t.Context(), mail.Message{To: "b@example.com"}))
	sent := f.Sent()
	require.Len(t, sent, 2)
	require.Equal(t, "a@example.com", sent[0].To)

	sent[0].To = "changed"
	require.Equal(t, "a@example.com", f.Sent()[0].To, "Sent returns a copy")

	f.Err = errors.New("smtp down")
	require.Error(t, f.Send(t.Context(), mail.Message{To: "c@example.com"}))
	require.Len(t, f.Sent(), 2, "a failed send records nothing")
}

// blockingSender lets a test hold a delivery open.
type blockingSender struct {
	release chan struct{}
	started chan struct{}
	err     error
	mu      sync.Mutex
	gotCtx  context.Context
}

func (b *blockingSender) Send(ctx context.Context, _ mail.Message) error {
	b.mu.Lock()
	b.gotCtx = ctx
	b.mu.Unlock()
	close(b.started)
	<-b.release
	return b.err
}

func TestAsync(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	t.Run("Send returns before the delivery finishes, and Wait waits for it", func(t *testing.T) {
		inner := &blockingSender{release: make(chan struct{}), started: make(chan struct{})}
		async := mail.NewAsync(inner, logger)

		require.NoError(t, async.Send(t.Context(), mail.Message{To: "a@example.com"}), "returned while the inner send is still blocked")
		<-inner.started

		shortCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, async.Wait(shortCtx), context.DeadlineExceeded, "still delivering")

		close(inner.release)
		require.NoError(t, async.Wait(t.Context()))
	})

	t.Run("the delivery survives the request that started it", func(t *testing.T) {
		inner := &blockingSender{release: make(chan struct{}), started: make(chan struct{})}
		async := mail.NewAsync(inner, logger)

		reqCtx, cancelRequest := context.WithCancel(t.Context())
		require.NoError(t, async.Send(reqCtx, mail.Message{To: "a@example.com"}))
		<-inner.started
		cancelRequest() // the HTTP request is over

		inner.mu.Lock()
		deliveryCtx := inner.gotCtx
		inner.mu.Unlock()
		require.NoError(t, deliveryCtx.Err(), "the delivery context is not tied to the request")

		close(inner.release)
		require.NoError(t, async.Wait(t.Context()))
	})

	t.Run("a failed delivery is logged without the address, and Send still returns nil", func(t *testing.T) {
		logs.Reset()
		inner := &blockingSender{release: make(chan struct{}), started: make(chan struct{}), err: errors.New("connection refused")}
		async := mail.NewAsync(inner, logger)

		require.NoError(t, async.Send(t.Context(), mail.Message{To: "secret-person@example.com"}))
		close(inner.release)
		require.NoError(t, async.Wait(t.Context()))

		require.Contains(t, logs.String(), "sending email failed")
		require.Contains(t, logs.String(), "connection refused")
		require.NotContains(t, logs.String(), "secret-person@example.com")
	})
}

func TestParseTLSMode(t *testing.T) {
	for _, s := range []string{"none", "starttls", "tls"} {
		m, err := mail.ParseTLSMode(s)
		require.NoError(t, err)
		require.Equal(t, mail.TLSMode(s), m)
	}
	for _, s := range []string{"", "ssl", "TLS", "STARTTLS"} {
		_, err := mail.ParseTLSMode(s)
		require.Error(t, err, s)
	}
}

func TestNewSMTPSenderValidatesConfig(t *testing.T) {
	_, err := mail.NewSMTPSender(mail.SMTPConfig{Port: 25, From: "a@example.com"})
	require.Error(t, err, "host is required")
	_, err = mail.NewSMTPSender(mail.SMTPConfig{Host: "localhost", Port: 25, From: "not an address"})
	require.Error(t, err)
	_, err = mail.NewSMTPSender(mail.SMTPConfig{Host: "localhost", Port: 25, From: "SiAPP <noreply@example.com>"})
	require.NoError(t, err)
}

func TestSMTPSenderDeliversAMultipartMessage(t *testing.T) {
	srv := startSMTP(t)
	sender, err := mail.NewSMTPSender(mail.SMTPConfig{
		Host: "127.0.0.1", Port: srv.port, From: "SiAPP <noreply@siapp.example>", TLS: mail.TLSNone,
	})
	require.NoError(t, err)

	msg, err := mail.NewPasswordResetMessage("budi@example.com", "Budi Santoso", resetLink, time.Hour)
	require.NoError(t, err)
	require.NoError(t, sender.Send(t.Context(), msg))

	raw := srv.waitForMessage(t)
	parsed, err := netmail.ReadMessage(strings.NewReader(raw))
	require.NoError(t, err)

	to, err := netmail.ParseAddress(parsed.Header.Get("To"))
	require.NoError(t, err)
	require.Equal(t, "budi@example.com", to.Address)
	from, err := netmail.ParseAddress(parsed.Header.Get("From"))
	require.NoError(t, err)
	require.Equal(t, "noreply@siapp.example", from.Address)
	require.Equal(t, "SiAPP", from.Name)
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	require.NoError(t, err)
	require.Equal(t, "Atur ulang kata sandi SiAPP", subject)

	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/alternative", mediaType)

	parts := map[string]string{}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		var body io.Reader = part
		if part.Header.Get("Content-Transfer-Encoding") == "quoted-printable" {
			body = quotedprintable.NewReader(part)
		}
		b, err := io.ReadAll(body)
		require.NoError(t, err)
		ct, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		require.NoError(t, err)
		parts[ct] = string(b)
	}

	require.Contains(t, parts["text/plain"], resetLink, "the plain-text part carries the exact link")
	require.Contains(t, parts["text/html"], "abc_DEF-123", "the HTML part carries the token")
}

func TestSMTPSenderReportsAConnectionFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close()) // nothing listens on this port any more

	sender, err := mail.NewSMTPSender(mail.SMTPConfig{Host: "127.0.0.1", Port: port, From: "a@example.com", TLS: mail.TLSNone})
	require.NoError(t, err)
	require.Error(t, sender.Send(t.Context(), mail.Message{To: "b@example.com", Subject: "x", Text: "x", HTML: "x"}))
}

// smtpServer is a tiny in-process SMTP server that accepts mail and keeps the raw messages.
type smtpServer struct {
	port int
	mu   sync.Mutex
	msgs []string
}

func startSMTP(t *testing.T) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	s := &smtpServer{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *smtpServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	reply := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	reply("220 test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 test") // no STARTTLS, no AUTH: a plain local catcher
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			reply("250 OK")
		case cmd == "DATA":
			reply("354 go ahead")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, ".")) // undo dot-stuffing
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, data.String())
			s.mu.Unlock()
			reply("250 queued")
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (s *smtpServer) waitForMessage(t *testing.T) string {
	t.Helper()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.msgs) > 0
	}, 5*time.Second, 10*time.Millisecond, "the SMTP server received no message")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[0]
}

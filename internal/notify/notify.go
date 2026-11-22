// Package notify sends operator alerts by email (SMTP) with a persistent
// per-key cooldown so a broken chain cannot flood the inbox.
package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Config is the SMTP connection. Host empty means notifications are disabled.
type Config struct {
	Host string
	Port int
	User string
	Pass string
	From string
	To   []string
}

// Enabled reports whether enough is configured to send mail.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.Host) != "" && len(c.To) > 0
}

// Sender delivers one message. It is an interface so tests can substitute it.
type Sender interface {
	Send(ctx context.Context, subject, body string) error
}

// SMTP sends plain-text UTF-8 mail. Port 465 uses implicit TLS, other ports
// try STARTTLS (QQ, 163, Resend, Aliyun and most providers all work).
type SMTP struct {
	cfg Config
}

func NewSMTP(cfg Config) *SMTP { return &SMTP{cfg: cfg} }

func (s *SMTP) Send(ctx context.Context, subject, body string) error {
	if !s.cfg.Enabled() {
		return errors.New("notify: smtp is not configured")
	}
	from := strings.TrimSpace(s.cfg.From)
	if from == "" {
		from = s.cfg.User
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	var (
		conn net.Conn
		err  error
	)
	if s.cfg.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: s.cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("notify: dial %s: %w", s.cfg.Host, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(30 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("notify: handshake: %w", err)
	}
	defer client.Close()

	if s.cfg.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("notify: starttls: %w", err)
			}
		}
	}
	if s.cfg.User != "" {
		if err := client.Auth(smtp.PlainAuth("", s.cfg.User, s.cfg.Pass, s.cfg.Host)); err != nil {
			return fmt.Errorf("notify: auth: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("notify: mail from: %w", err)
	}
	for _, rcpt := range s.cfg.To {
		rcpt = strings.TrimSpace(rcpt)
		if rcpt == "" {
			continue
		}
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("notify: rcpt %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("notify: data: %w", err)
	}
	if _, err := w.Write(buildMessage(from, s.cfg.To, subject, body)); err != nil {
		return fmt.Errorf("notify: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: close: %w", err)
	}
	return client.Quit()
}

// buildMessage renders a UTF-8 plain-text message with base64 body, which
// survives every provider's 8-bit rules.
func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return []byte(b.String())
}

// AlertStore persists the last time each alert key fired.
type AlertStore interface {
	LastAlert(ctx context.Context, key string) (time.Time, bool, error)
	MarkAlert(ctx context.Context, key string, at time.Time) error
}

// Manager applies the cooldown and keeps the rest of the service unaware of
// mail failures.
type Manager struct {
	sender   Sender
	store    AlertStore
	cooldown time.Duration
	now      func() time.Time
}

func NewManager(sender Sender, store AlertStore, cooldown time.Duration) *Manager {
	return &Manager{sender: sender, store: store, cooldown: cooldown, now: time.Now}
}

// Enabled reports whether mail would actually be attempted.
func (m *Manager) Enabled() bool { return m != nil && m.sender != nil }

// Alert sends at most once per cooldown window for the given key. Errors are
// returned for logging but callers should keep running.
func (m *Manager) Alert(ctx context.Context, key, subject, body string) error {
	return m.AlertEvery(ctx, key, subject, body, m.cooldown)
}

// AlertEvery is Alert with an explicit cooldown for this key (used by the gas
// wallet, which repeats every 30 minutes instead of every 6 hours).
func (m *Manager) AlertEvery(ctx context.Context, key, subject, body string, cooldown time.Duration) error {
	if !m.Enabled() {
		return nil
	}
	if m.store != nil && cooldown > 0 {
		last, ok, err := m.store.LastAlert(ctx, key)
		if err != nil {
			slog.Warn("notify: read alert state", "key", key, "error", err)
		} else if ok && m.now().Sub(last) < cooldown {
			return nil
		}
	}
	if err := m.sender.Send(ctx, subject, body); err != nil {
		return err
	}
	if m.store != nil {
		if err := m.store.MarkAlert(ctx, key, m.now()); err != nil {
			slog.Warn("notify: persist alert state", "key", key, "error", err)
		}
	}
	slog.Info("alert sent", "key", key, "subject", subject)
	return nil
}

// Send bypasses the cooldown (used by the one-off notifier binary).
func (m *Manager) Send(ctx context.Context, subject, body string) error {
	if !m.Enabled() {
		return errors.New("notify: not configured")
	}
	return m.sender.Send(ctx, subject, body)
}

package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"text/template"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// smtpProvider sends email via SMTP.
//
// secret: the SMTP password (optional if the server needs no auth).
// config:
//   - host (required), port (default 587)
//   - username (optional; enables PLAIN auth when set)
//   - from (required), to ([]string, required)
//   - tls: "starttls" (default), "tls" (implicit/SMTPS), or "none"
//   - subject_template: optional text/template rendered with the Event
type smtpProvider struct {
	host        string
	port        int
	username    string
	password    string
	from        string
	to          []string
	tlsMode     string
	subjectTmpl *template.Template
}

func newSMTP(cfg map[string]any, secret string) (Provider, error) {
	host := cfgString(cfg, "host")
	if host == "" {
		return nil, fmt.Errorf("smtp: host is required")
	}
	from := cfgString(cfg, "from")
	if from == "" {
		return nil, fmt.Errorf("smtp: from is required")
	}
	to := cfgStringSlice(cfg, "to")
	if len(to) == 0 {
		return nil, fmt.Errorf("smtp: at least one to address is required")
	}
	tlsMode := strings.ToLower(cfgStringDefault(cfg, "tls", "starttls"))
	switch tlsMode {
	case "starttls", "tls", "none":
	default:
		return nil, fmt.Errorf("smtp: invalid tls mode %q (want starttls|tls|none)", tlsMode)
	}
	p := &smtpProvider{
		host:     host,
		port:     cfgInt(cfg, "port", 587),
		username: cfgString(cfg, "username"),
		password: secret,
		from:     from,
		to:       to,
		tlsMode:  tlsMode,
	}
	if tpl := cfgString(cfg, "subject_template"); tpl != "" {
		t, err := template.New("subject").Parse(tpl)
		if err != nil {
			return nil, fmt.Errorf("smtp: invalid subject_template: %w", err)
		}
		p.subjectTmpl = t
	}
	return p, nil
}

func (p *smtpProvider) Send(ctx context.Context, msg models.NotificationMessage) error {
	addr := net.JoinHostPort(p.host, fmt.Sprintf("%d", p.port))
	tlsConfig := &tls.Config{ServerName: p.host}

	conn, err := p.dial(ctx, addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	client, err := smtp.NewClient(conn, p.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: new client: %w", err)
	}
	defer client.Close()

	if p.tlsMode == "starttls" {
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("smtp: starttls: %w", err)
		}
	}
	if p.username != "" {
		auth := smtp.PlainAuth("", p.username, p.password, p.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}
	if err := client.Mail(p.from); err != nil {
		return fmt.Errorf("smtp: mail from: %w", err)
	}
	for _, rcpt := range p.to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp: rcpt %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write(p.buildMessage(msg)); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: close data: %w", err)
	}
	return client.Quit()
}

func (p *smtpProvider) dial(ctx context.Context, addr string, tlsConfig *tls.Config) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: 15 * time.Second}
	if p.tlsMode == "tls" {
		d := &tls.Dialer{NetDialer: netDialer, Config: tlsConfig}
		return d.DialContext(ctx, "tcp", addr)
	}
	return netDialer.DialContext(ctx, "tcp", addr)
}

func (p *smtpProvider) buildMessage(msg models.NotificationMessage) []byte {
	ev := msg.Event
	subject := subjectLine(ev)
	if p.subjectTmpl != nil {
		var buf bytes.Buffer
		if err := p.subjectTmpl.Execute(&buf, ev); err == nil {
			subject = strings.TrimSpace(buf.String())
		}
	}
	if msg.Test {
		subject = "Omni-Notify test notification"
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", sanitizeHeader(p.from))
	fmt.Fprintf(&b, "To: %s\r\n", sanitizeHeader(strings.Join(p.to, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	body := plainBody(ev)
	if msg.Test {
		body = "This is a test notification from Omni-Notify.\r\n\r\n" + body
	}
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.Bytes()
}

// sanitizeHeader strips CR/LF to prevent header injection via event content.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

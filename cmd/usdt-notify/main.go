// Command usdt-notify sends one operator email with the scanner's SMTP
// settings. It exists so a cron watchdog can report "the scanner is not
// answering" — something the scanner itself cannot do while it is down.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/notify"
	"new-api-usdt/internal/scanner"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	if cfg.SMTPHost == "" || len(cfg.MailTo) == 0 {
		fmt.Fprintln(os.Stderr, "SMTP_HOST / MAIL_TO are not configured")
		os.Exit(2)
	}
	message := strings.TrimSpace(strings.Join(os.Args[1:], " "))
	if message == "" {
		raw, _ := io.ReadAll(os.Stdin)
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		message = "手动测试邮件"
	}
	subject := message
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	subject = "[USDT 通知] " + subject

	sender := notify.NewSMTP(notify.Config{
		Host: cfg.SMTPHost,
		Port: cfg.SMTPPort,
		User: cfg.SMTPUser,
		Pass: cfg.SMTPPass,
		From: cfg.MailFrom,
		To:   cfg.MailTo,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	body := "来自 usdt-scanner 的通知\n\n" + message + "\n\n版本：" + scanner.Version
	if err := sender.Send(ctx, subject, body); err != nil {
		fmt.Fprintln(os.Stderr, "send:", err)
		os.Exit(1)
	}
	fmt.Println("sent")
}

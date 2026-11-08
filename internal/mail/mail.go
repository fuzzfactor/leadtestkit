// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/report"
)

type Sender interface {
	Send(ctx context.Context, e *report.Email) error
}

// ServerError is a failure to use the SMTP server at all, as opposed to
// the server refusing a message or recipient.
type ServerError struct {
	Err error
}

func (e *ServerError) Error() string { return e.Err.Error() }
func (e *ServerError) Unwrap() error { return e.Err }

func IsServerError(err error) bool {
	var se *ServerError
	return errors.As(err, &se)
}

type SMTP struct {
	Host     string
	Port     int
	Security string
	Username string
	Password string
	Timeout  time.Duration

	// Allowed, when not empty, is the complete list of recipients the
	// sender may deliver to.  Anything else is refused.
	Allowed []string
	Log     *slog.Logger

	TLSConfig *tls.Config
}

func (s *SMTP) checkRecipients(rcpts []string) error {
	if len(rcpts) == 0 {
		return fmt.Errorf("no recipients")
	}
	if len(s.Allowed) == 0 {
		return nil
	}
	for _, r := range rcpts {
		ok := false
		for _, a := range s.Allowed {
			if strings.EqualFold(strings.TrimSpace(r), bareAddress(a)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("recipient %s is not in the allowed list", r)
		}
	}
	return nil
}

func bareAddress(a string) string {
	a = strings.TrimSpace(a)
	if i := strings.LastIndex(a, "<"); i >= 0 {
		a = strings.TrimSuffix(a[i+1:], ">")
	}
	return a
}

func (s *SMTP) Send(ctx context.Context, e *report.Email) error {
	if err := s.checkRecipients(e.Recipients); err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dialer := &net.Dialer{Timeout: timeout}
	// When the caller gave up, the server is not to blame.
	parent := ctx
	server := func(err error) error {
		if parent.Err() != nil {
			return err
		}
		return &ServerError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var conn net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: s.Host}
	if s.TLSConfig != nil {
		tlsConfig = s.TLSConfig
	}
	if s.Security == "tls" {
		d := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
		conn, err = d.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return server(fmt.Errorf("smtp dial %s: %w", addr, err))
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return server(fmt.Errorf("smtp: %w", err))
	}
	defer c.Close()
	if s.Security != "tls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return server(fmt.Errorf("smtp: server does not support STARTTLS"))
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return server(fmt.Errorf("smtp starttls: %w", err))
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return server(fmt.Errorf("smtp auth: %w", err))
		}
	}
	if err := c.Mail(e.From); err != nil {
		return server(fmt.Errorf("smtp mail from: %w", err))
	}
	for _, r := range e.Recipients {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(e.Data); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	return c.Quit()
}

// Log only logs emails; used when mode.email is off.
type Log struct {
	Log *slog.Logger
}

func (l *Log) Send(ctx context.Context, e *report.Email) error {
	subject := ""
	for _, line := range strings.Split(string(e.Data), "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subject = strings.TrimPrefix(line, "Subject: ")
			break
		}
		if line == "" {
			break
		}
	}
	l.Log.Info(fmt.Sprintf("Email disabled: not sending %q to %s.",
		subject, strings.Join(e.Recipients, ", ")))
	return nil
}

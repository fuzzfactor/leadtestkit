// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"leadtestkit/internal/report"
)

func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

type fakeSMTP struct {
	mu    sync.Mutex
	auth  string
	from  string
	rcpts []string
	data  string
}

// serve implements just enough of SMTP with STARTTLS for net/smtp.
func (f *fakeSMTP) serve(t *testing.T, ln net.Listener, cert tls.Certificate) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { conn.Write([]byte(s + "\r\n")) }
	w("220 localhost ready")
	tlsOn := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch {
		case cmd == "EHLO":
			if tlsOn {
				w("250-localhost\r\n250 AUTH PLAIN")
			} else {
				w("250-localhost\r\n250 STARTTLS")
			}
		case cmd == "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
			if err := tc.Handshake(); err != nil {
				t.Errorf("handshake: %v", err)
				return
			}
			conn = tc
			r = bufio.NewReader(conn)
			tlsOn = true
		case cmd == "AUTH":
			dec, _ := base64.StdEncoding.DecodeString(strings.Fields(line)[2])
			f.mu.Lock()
			f.auth = string(dec)
			f.mu.Unlock()
			w("235 ok")
		case strings.HasPrefix(strings.ToUpper(line), "MAIL FROM:"):
			f.from = line[10:]
			w("250 ok")
		case strings.HasPrefix(strings.ToUpper(line), "RCPT TO:"):
			f.rcpts = append(f.rcpts, line[8:])
			w("250 ok")
		case cmd == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("502 unknown")
		}
	}
}

// testEmail is a report, also sent to the extra recipients.
func testEmail(t *testing.T, extra ...string) *report.Email {
	e, err := report.Compose(report.Input{
		Style: "ovs", Label: "lorem-robot", Result: "success", Description: "lorem: passed",
		PatchID: 1, PatchName: "[PATCH] ipsum", PatchURL: "https://pw.example.org/patch/1/",
		From: "Robot <robot@example.org>", To: "lorem-build@lists.example.org",
		MessageID: "<leadtest.1@example.org>", Date: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Recipients = append(e.Recipients, extra...)
	return e
}

func TestSendStartTLS(t *testing.T) {
	cert, pool := testCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := &fakeSMTP{}
	done := make(chan struct{})
	go func() { f.serve(t, ln, cert); close(done) }()

	port, _ := strconv.Atoi(strings.Split(ln.Addr().String(), ":")[1])
	s := &SMTP{
		Host: "localhost", Port: port, Security: "starttls",
		Username: "lorem", Password: "ipsum-pass",
		Allowed:   []string{"Lorem Build <lorem-build@lists.example.org>"},
		TLSConfig: &tls.Config{ServerName: "localhost", RootCAs: pool},
	}
	if err := s.Send(context.Background(), testEmail(t)); err != nil {
		t.Fatal(err)
	}
	<-done
	if f.auth != "\x00lorem\x00ipsum-pass" || f.from != "<robot@example.org>" ||
		strings.Join(f.rcpts, ",") != "<lorem-build@lists.example.org>" {
		t.Errorf("auth=%q from=%q rcpts=%v", f.auth, f.from, f.rcpts)
	}
	if !strings.Contains(f.data, "Test-Label: lorem-robot\r\n") {
		t.Errorf("data:\n%s", f.data)
	}
}

func TestAllowlistRefusesOtherRecipients(t *testing.T) {
	s := &SMTP{Host: "localhost", Port: 1, Allowed: []string{"lorem-build@lists.example.org"}}
	err := s.Send(context.Background(), testEmail(t, "human@example.org"))
	if err == nil || !strings.Contains(err.Error(), "human@example.org") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	s := &SMTP{Host: "127.0.0.1", Port: port, Timeout: time.Second}
	err = s.Send(context.Background(), testEmail(t))
	if err == nil || !IsServerError(err) {
		t.Errorf("an unreachable server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.Send(ctx, testEmail(t))
	if err == nil || IsServerError(err) {
		t.Errorf("a cancelled send: %v", err)
	}
	s.Allowed = []string{"lorem-build@lists.example.org"}
	err = s.Send(context.Background(), testEmail(t, "human@example.org"))
	if err == nil || IsServerError(err) {
		t.Errorf("a refused recipient: %v", err)
	}
}

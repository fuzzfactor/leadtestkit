// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"leadtestkit/internal/httpx"
	"leadtestkit/internal/logx"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	keyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			t.Fatal(err)
		}
	})
	return testKey
}

func pkcs1(t *testing.T) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key(t))}))
}

func TestParseKey(t *testing.T) {
	der, err := x509.MarshalPKCS8PrivateKey(key(t))
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, text := range map[string]string{
		"pkcs1":   pkcs1(t),
		"pkcs8":   pkcs8,
		"spaces":  "\n  " + pkcs1(t) + "\n\n",
		"escaped": strings.ReplaceAll(strings.TrimSpace(pkcs1(t)), "\n", `\n`),
	} {
		k, err := ParseKey(text)
		if err != nil || !k.Equal(key(t)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseKey("lorem"); err == nil || !strings.Contains(err.Error(), "BEGIN") {
		t.Errorf("err = %v", err)
	}
}

// verifyJWT checks the signature and returns the claims.
func verifyJWT(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", jwt)
	}
	enc := base64.RawURLEncoding
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	header, _ := enc.DecodeString(parts[0])
	if string(header) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header = %s", header)
	}
	var claims map[string]any
	data, _ := enc.DecodeString(parts[1])
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

type server struct {
	t            *testing.T
	installs     int
	tokens       int
	installation int
	permissions  string
	repos        []string
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	claims := verifyJWT(s.t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if claims["iss"] != "Iv23lilorem" {
		s.t.Errorf("iss = %v", claims["iss"])
	}
	switch {
	case r.URL.Path == "/repos/ipsum/lorem/installation":
		s.installs++
		if s.installation == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"account":{"login":"ipsum"}}`, s.installation)
	case r.Method == http.MethodPost &&
		r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", s.installation):
		s.tokens++
		var body struct {
			Repositories []string `json:"repositories"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s.repos = body.Repositories
		fmt.Fprintf(w, `{"token":"ghs_lorem%d","expires_at":"2026-03-10T13:00:00Z",`+
			`"permissions":%s}`, s.tokens, s.permissions)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newApp(t *testing.T, s *server, now *time.Time, log *bytes.Buffer) *App {
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(log, nil))
	a, err := New(Options{
		APIURL: srv.URL + "/", Repo: "ipsum/lorem", ClientID: "Iv23lilorem",
		PrivateKey: pkcs1(t), Logger: logger, Now: func() time.Time { return *now },
		HTTP: httpx.New(httpx.Options{Name: "github-app", HTTPClient: srv.Client(),
			Logger: logger,
			Sleep:  func(ctx context.Context, d time.Duration) error { return nil }}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestJWT(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	a := newApp(t, &server{t: t}, &now, &bytes.Buffer{})
	jwt, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyJWT(t, jwt)
	if claims["iat"] != float64(now.Unix()-60) || claims["exp"] != float64(now.Unix()+540) ||
		claims["iss"] != "Iv23lilorem" {
		t.Errorf("claims = %v", claims)
	}
}

func TestToken(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s := &server{t: t, installation: 77,
		permissions: `{"actions":"write","contents":"write","metadata":"read",` +
			`"workflows":"write"}`}
	var log bytes.Buffer
	a := newApp(t, s, &now, &log)
	ctx := context.Background()

	auth, err := a.Authorization(ctx)
	if err != nil || auth != "Bearer ghs_lorem1" {
		t.Fatalf("auth = %q, %v", auth, err)
	}
	if len(s.repos) != 1 || s.repos[0] != "lorem" {
		t.Errorf("token not limited to the test repository: %v", s.repos)
	}
	if logx.Redact("x ghs_lorem1 x") != "x [REDACTED] x" {
		t.Error("the token is not redacted from logs")
	}
	if strings.Contains(log.String(), "lacks permissions") {
		t.Errorf("unexpected permission warning:\n%s", log.String())
	}

	now = now.Add(49 * time.Minute)
	if tok, _ := a.Token(ctx); tok != "ghs_lorem1" || s.tokens != 1 {
		t.Errorf("a valid token was not reused: %q, %d tokens", tok, s.tokens)
	}
	now = now.Add(2 * time.Minute)
	if tok, _ := a.Token(ctx); tok != "ghs_lorem2" || s.tokens != 2 || s.installs != 1 {
		t.Errorf("token not renewed before expiry: %q, %d tokens, %d lookups",
			tok, s.tokens, s.installs)
	}
}

func TestTokenErrors(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	var log bytes.Buffer
	a := newApp(t, &server{t: t}, &now, &log)
	_, err := a.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not installed on ipsum/lorem") {
		t.Errorf("err = %v", err)
	}

	s := &server{t: t, installation: 77, permissions: `{"contents":"read"}`}
	a = newApp(t, s, &now, &log)
	if _, err := a.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "actions: write, contents: write, workflows: write") {
		t.Errorf("missing permissions not reported:\n%s", log.String())
	}

	if _, err := New(Options{ClientID: "Iv23lilorem", PrivateKey: "lorem"}); err == nil {
		t.Error("a bad key must be refused")
	}
	if _, err := New(Options{PrivateKey: pkcs1(t)}); err == nil {
		t.Error("an empty Client ID must be refused")
	}
}

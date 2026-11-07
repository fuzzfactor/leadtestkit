// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package ghapp obtains installation tokens of a GitHub App for one
// repository.
package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"leadtestkit/internal/httpx"
	"leadtestkit/internal/logx"
)

// Installation tokens are valid for an hour; one with less than this
// left is replaced, so that it does not expire during a request or a
// git push.
const renewBefore = 10 * time.Minute

type Options struct {
	APIURL string
	// Repo is "owner/name", the only repository the tokens can access.
	Repo string
	// ClientID identifies the App; its App ID works as well.
	ClientID string
	// PrivateKey is a PEM private key of the App, PKCS #1 as downloaded
	// from GitHub or PKCS #8.
	PrivateKey string
	Logger     *slog.Logger
	HTTP       *httpx.Client
	Now        func() time.Time
}

type App struct {
	opts Options
	key  *rsa.PrivateKey
	http *httpx.Client
	log  *slog.Logger

	mu           sync.Mutex
	installation int64
	token        string
	expires      time.Time
}

func New(opts Options) (*App, error) {
	if opts.ClientID == "" {
		return nil, errors.New("GitHub App Client ID is empty")
	}
	key, err := ParseKey(opts.PrivateKey)
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.APIURL = strings.TrimRight(opts.APIURL, "/")
	a := &App{opts: opts, key: key, http: opts.HTTP, log: opts.Logger}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.http == nil {
		a.http = httpx.New(httpx.Options{
			Name: "github-app",
			Header: http.Header{
				"Accept":               {"application/vnd.github+json"},
				"X-Github-Api-Version": {"2022-11-28"},
			},
			Logger: a.log,
		})
	}
	return a, nil
}

// ParseKey reads an RSA private key in PEM form.  Secrets pasted into
// web forms sometimes lose their line breaks or gain surrounding
// whitespace, so both are tolerated.
func ParseKey(text string) (*rsa.PrivateKey, error) {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "\n") {
		text = strings.ReplaceAll(text, `\n`, "\n")
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("GitHub App private key: no PEM block found; paste the " +
			"whole .pem file, including the BEGIN and END lines")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("GitHub App private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App private key is not an RSA key")
	}
	return rk, nil
}

// JWT returns a token that authenticates as the App itself, valid for
// nine minutes.  It is backdated by a minute against clock skew.
func (a *App) JWT() (string, error) {
	now := a.opts.Now().Unix()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now - 60, "exp": now + 9*60, "iss": a.opts.ClientID,
	})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// Authorization returns the Authorization header value for API
// requests, renewing the token when needed.
func (a *App) Authorization(ctx context.Context) (string, error) {
	tok, err := a.Token(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + tok, nil
}

// Token returns an installation token for the repository.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.opts.Now().Add(renewBefore).Before(a.expires) {
		return a.token, nil
	}
	if a.token != "" {
		a.log.Info(fmt.Sprintf("The GitHub App token expires at %s; renewing it.",
			a.expires.UTC().Format(time.RFC3339)))
	}
	jwt, err := a.JWT()
	if err != nil {
		return "", err
	}
	if a.installation == 0 {
		if a.installation, err = a.findInstallation(ctx, jwt); err != nil {
			return "", err
		}
	}
	return a.newToken(ctx, jwt)
}

func (a *App) findInstallation(ctx context.Context, jwt string) (int64, error) {
	resp, err := a.http.Do(ctx, httpx.Request{
		URL:    a.opts.APIURL + "/repos/" + a.opts.Repo + "/installation",
		Header: http.Header{"Authorization": {"Bearer " + jwt}},
	})
	switch code := httpx.StatusCode(err); {
	case code == http.StatusNotFound:
		return 0, fmt.Errorf("the GitHub App %s is not installed on %s; install it "+
			"there (see SETUP.rst)", a.opts.ClientID, a.opts.Repo)
	case code == http.StatusUnauthorized:
		return 0, fmt.Errorf("GitHub rejected the App credentials; check "+
			"LEADTEST_APP_CLIENT_ID and LEADTEST_APP_PRIVATE_KEY: %w", err)
	case err != nil:
		return 0, fmt.Errorf("find the GitHub App installation on %s: %w", a.opts.Repo, err)
	}
	var inst struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.Unmarshal(resp.Body, &inst); err != nil || inst.ID == 0 {
		return 0, fmt.Errorf("decode the GitHub App installation of %s: %v",
			a.opts.Repo, err)
	}
	a.log.Info(fmt.Sprintf("Using GitHub App %s, installation %d on account %s.",
		a.opts.ClientID, inst.ID, inst.Account.Login))
	return inst.ID, nil
}

func (a *App) newToken(ctx context.Context, jwt string) (string, error) {
	name := a.opts.Repo[strings.LastIndex(a.opts.Repo, "/")+1:]
	body, err := json.Marshal(map[string]any{"repositories": []string{name}})
	if err != nil {
		return "", err
	}
	resp, err := a.http.Do(ctx, httpx.Request{
		Method: http.MethodPost,
		URL: fmt.Sprintf("%s/app/installations/%d/access_tokens",
			a.opts.APIURL, a.installation),
		Header: http.Header{"Authorization": {"Bearer " + jwt}},
		Body:   body,
	})
	if err != nil {
		if code := httpx.StatusCode(err); code == http.StatusNotFound ||
			code == http.StatusUnprocessableEntity {
			// Uninstalled, or the repository was removed from it.
			a.installation = 0
		}
		return "", fmt.Errorf("create a GitHub App token for %s: %w", a.opts.Repo, err)
	}
	var res struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(resp.Body, &res); err != nil || res.Token == "" {
		return "", fmt.Errorf("decode the GitHub App token for %s: %v", a.opts.Repo, err)
	}
	logx.AddSecret(res.Token)
	a.token, a.expires = res.Token, res.ExpiresAt
	a.log.Info(fmt.Sprintf("Got a GitHub App token for %s, valid until %s, "+
		"with permissions: %s.", a.opts.Repo, res.ExpiresAt.UTC().Format(time.RFC3339),
		formatPermissions(res.Permissions)))
	if missing := missingPermissions(res.Permissions); len(missing) > 0 {
		a.log.Warn(fmt.Sprintf("The GitHub App lacks permissions LeadTestKit needs: %s. "+
			"Add them in the App settings and accept them on the installation.",
			strings.Join(missing, ", ")))
	}
	return a.token, nil
}

// Required lists the repository permissions LeadTestKit uses.
var Required = map[string]string{
	"actions":   "write",
	"checks":    "read",
	"contents":  "write",
	"workflows": "write",
}

func missingPermissions(have map[string]string) []string {
	var out []string
	for name, level := range Required {
		if have[name] != level && !(level == "read" && have[name] == "write") {
			out = append(out, name+": "+level)
		}
	}
	sort.Strings(out)
	return out
}

func formatPermissions(p map[string]string) string {
	names := make([]string, 0, len(p))
	for name := range p {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		parts = append(parts, name+": "+p[name])
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

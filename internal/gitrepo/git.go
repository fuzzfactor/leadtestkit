// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"leadtestkit/internal/logx"
)

const defaultTimeout = 10 * time.Minute

type Git struct {
	Dir     string
	Env     []string
	Timeout time.Duration
	Log     *slog.Logger
}

type Error struct {
	Args   []string
	Code   int
	Stderr string
	Stdout string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Stdout)
	}
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, Tail(msg, 20))
}

func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// AuthEnv returns environment variables that make git send a token to
// one host without putting it into URLs, argv or config files.
func AuthEnv(host, token string) []string {
	if token == "" {
		return nil
	}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	logx.AddSecret(cred)
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://" + host + "/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + cred,
	}
}

func (g *Git) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func (g *Git) Run(ctx context.Context, args ...string) (string, error) {
	return g.RunInput(ctx, nil, args...)
}

func (g *Git) RunInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ASKPASS=true",
		"LC_ALL=C",
	)
	cmd.Env = append(cmd.Env, g.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	g.log().Debug(fmt.Sprintf("Running git %s in %s.", strings.Join(args, " "), g.Dir))
	err := cmd.Run()
	g.log().Debug(fmt.Sprintf("Git %s finished in %s (error: %v).", strings.Join(args, " "),
		time.Since(start).Round(time.Millisecond), err))
	if err != nil {
		code := -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if ctx.Err() != nil {
			return stdout.String(),
				fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		return stdout.String(), &Error{
			Args:   args,
			Code:   code,
			Stderr: logx.Redact(stderr.String()),
			Stdout: logx.Redact(stdout.String()),
		}
	}
	return stdout.String(), nil
}

func (g *Git) Out(ctx context.Context, args ...string) (string, error) {
	out, err := g.Run(ctx, args...)
	return strings.TrimSpace(out), err
}

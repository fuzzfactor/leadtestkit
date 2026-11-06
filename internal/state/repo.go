// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"leadtestkit/internal/gitrepo"
)

// ErrConflict means the remote state changed in a way that could not be
// merged; the caller should reload the state and reconcile.
var ErrConflict = errors.New("state branch conflict")

// Repo commits and pushes the state directory.
type Repo struct {
	Git    *gitrepo.Git
	Remote string
	Branch string
	// Author is "Name <email>".
	Author string
	Log    *slog.Logger
}

var authorRe = regexp.MustCompile(`^(.+?) <(.+)>$`)

func (r *Repo) identity() []string {
	m := authorRe.FindStringSubmatch(r.Author)
	if m == nil {
		return nil
	}
	return []string{
		"GIT_AUTHOR_NAME=" + m[1], "GIT_AUTHOR_EMAIL=" + m[2],
		"GIT_COMMITTER_NAME=" + m[1], "GIT_COMMITTER_EMAIL=" + m[2],
	}
}

// Commit records all changes in the state directory.  It returns false
// when there was nothing to commit.
func (r *Repo) Commit(ctx context.Context, msg string) (bool, error) {
	if _, err := r.Git.Run(ctx, "add", "-A", "."); err != nil {
		return false, err
	}
	if _, err := r.Git.Run(ctx, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	g := *r.Git
	g.Env = append(append([]string(nil), g.Env...), r.identity()...)
	if _, err := g.Run(ctx, "commit", "-q", "--no-verify", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

// Push publishes local commits.  If the remote moved (a human edited
// the state), local commits are rebased on top, with the bot's version
// winning conflicting hunks.  It reports whether a rebase happened, in
// which case the state must be reloaded from disk.
func (r *Repo) Push(ctx context.Context) (bool, error) {
	rebased := false
	ref := "HEAD:refs/heads/" + r.Branch
	for attempt := 0; attempt < 3; attempt++ {
		_, err := r.Git.Run(ctx, "push", "-q", r.Remote, ref)
		if err == nil {
			return rebased, nil
		}
		r.log().Warn("state push failed, rebasing", "err", err)
		if _, ferr := r.Git.Run(ctx, "fetch", "-q", r.Remote, r.Branch); ferr != nil {
			return rebased, fmt.Errorf("state push: %v; fetch: %w", err, ferr)
		}
		g := *r.Git
		g.Env = append(append([]string(nil), g.Env...), r.identity()...)
		if _, rerr := g.Run(ctx, "rebase", "-q", "-X", "theirs", "FETCH_HEAD"); rerr != nil {
			r.Git.Run(ctx, "rebase", "--abort")
			return rebased, r.resetToRemote(ctx, rerr)
		}
		if _, lerr := Load(r.Git.Dir); lerr != nil {
			return rebased, r.resetToRemote(ctx, lerr)
		}
		rebased = true
	}
	return rebased, fmt.Errorf("state push failed after rebases")
}

func (r *Repo) resetToRemote(ctx context.Context, cause error) error {
	r.log().Error("state merge failed, resetting to remote", "err", cause)
	if _, err := r.Git.Run(ctx, "reset", "-q", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("%w: %v; reset: %v", ErrConflict, cause, err)
	}
	return fmt.Errorf("%w: %v", ErrConflict, cause)
}

func (r *Repo) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

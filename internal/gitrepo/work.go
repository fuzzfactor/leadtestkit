// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// WorkRepo is the local repository used to apply series.  It is a
// blobless partial clone, so only the trees that are checked out are
// downloaded, and it can be cached between runs.
type WorkRepo struct {
	Git *Git
	// TestRemote is the HTTPS URL of the test fork.
	TestRemote string
	// TestAuth returns the environment that authenticates requests to
	// the test fork, see AuthEnv.  It is asked every time, so that
	// short-lived tokens can be renewed.
	TestAuth func(ctx context.Context) ([]string, error)
	// Committer is "Name <email>".
	Committer string

	initialized bool
}

var committerRe = regexp.MustCompile(`^(.+?) <(.+)>$`)

func (w *WorkRepo) env() []string {
	m := committerRe.FindStringSubmatch(w.Committer)
	if m == nil {
		return nil
	}
	return []string{"GIT_COMMITTER_NAME=" + m[1], "GIT_COMMITTER_EMAIL=" + m[2]}
}

func (w *WorkRepo) ensure(ctx context.Context) error {
	if w.initialized {
		return nil
	}
	if err := os.MkdirAll(w.Git.Dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(w.Git.Dir, ".git")); errors.Is(err, os.ErrNotExist) {
		w.Git.log().Info(fmt.Sprintf("Initializing the patch application repository in %s.",
			w.Git.Dir))
		if _, err := w.Git.Run(ctx, "init", "-q"); err != nil {
			return err
		}
	}
	w.abortAm(ctx)
	w.initialized = true
	return nil
}

func (w *WorkRepo) abortAm(ctx context.Context) {
	dir := filepath.Join(w.Git.Dir, ".git", "rebase-apply")
	if _, err := os.Stat(dir); err != nil {
		return
	}
	g := *w.Git
	g.Env = append(append([]string(nil), g.Env...), w.env()...)
	w.Git.log().Info("Aborting an unfinished git am from an earlier apply attempt.")
	if _, err := g.Run(ctx, "am", "--abort"); err != nil {
		os.RemoveAll(dir)
	}
}

func remoteName(url string) string {
	sum := sha256.Sum256([]byte(url))
	return "r" + hex.EncodeToString(sum[:6])
}

func (w *WorkRepo) remote(ctx context.Context, url string) (string, error) {
	if err := w.ensure(ctx); err != nil {
		return "", err
	}
	name := remoteName(url)
	if _, err := w.Git.Run(ctx, "remote", "get-url", name); err != nil {
		if _, err := w.Git.Run(ctx, "remote", "add", name, url); err != nil {
			return "", err
		}
		for _, kv := range [][2]string{
			{"remote." + name + ".promisor", "true"},
			{"remote." + name + ".partialclonefilter", "blob:none"},
		} {
			if _, err := w.Git.Run(ctx, "config", kv[0], kv[1]); err != nil {
				return "", err
			}
		}
	}
	return name, nil
}

func (w *WorkRepo) fetch(ctx context.Context, url, src, dst string) (string, error) {
	name, err := w.remote(ctx, url)
	if err != nil {
		return "", err
	}
	g := *w.Git
	if url == w.TestRemote {
		if g, err = w.testGit(ctx); err != nil {
			return "", err
		}
	}
	dst = "refs/leadtest-cache/" + name + "/" + dst
	w.Git.log().Info(fmt.Sprintf("Fetching %s from %s into the work repository.", src, url))
	if _, err := g.Run(ctx, "fetch", "-q", "--no-tags", "--filter=blob:none", name,
		"+"+src+":"+dst); err != nil {
		return "", err
	}
	return w.Git.Out(ctx, "rev-parse", "--verify", dst+"^{commit}")
}

func (w *WorkRepo) Resolve(ctx context.Context, url, branch string) (string, error) {
	return w.fetch(ctx, url, "refs/heads/"+branch, "heads/"+branch)
}

func (w *WorkRepo) ResolveTestRef(ctx context.Context, ref string) (string, error) {
	return w.fetch(ctx, w.TestRemote, ref, strings.TrimPrefix(ref, "refs/"))
}

func (w *WorkRepo) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	out, err := w.Git.Run(ctx, "show", rev+":"+path)
	return []byte(out), err
}

// Apply applies the emails on top of base one by one.  It returns the
// commit of every applied patch; on a failure it also returns the index
// of the failing patch and the git am output.
func (w *WorkRepo) Apply(
	ctx context.Context, base string, mboxes [][]byte,
) ([]string, int, string, error) {
	if err := w.ensure(ctx); err != nil {
		return nil, -1, "", err
	}
	if _, err := w.Git.Run(ctx, "checkout", "-q", "--detach", "-f", base); err != nil {
		return nil, -1, "", err
	}
	if _, err := w.Git.Run(ctx, "clean", "-q", "-fdx"); err != nil {
		return nil, -1, "", err
	}
	dir, err := os.MkdirTemp("", "leadtest-am-")
	if err != nil {
		return nil, -1, "", err
	}
	defer os.RemoveAll(dir)

	g := *w.Git
	g.Env = append(append([]string(nil), g.Env...), w.env()...)
	var shas []string
	for i, m := range mboxes {
		w.Git.log().Info(fmt.Sprintf("Applying email %d of %d with git am.",
			i+1, len(mboxes)))
		file := filepath.Join(dir, fmt.Sprintf("%04d.patch", i+1))
		if err := os.WriteFile(file, m, 0o644); err != nil {
			return shas, -1, "", err
		}
		if _, err := g.Run(ctx, "am", "-q", "-3", "--no-gpg-sign", file); err != nil {
			var gerr *Error
			if !errors.As(err, &gerr) {
				return shas, -1, "", err
			}
			w.abortAm(ctx)
			return shas, i, strings.TrimSpace(gerr.Stdout + "\n" + gerr.Stderr), nil
		}
		sha, err := w.Git.Out(ctx, "rev-parse", "HEAD")
		if err != nil {
			return shas, -1, "", err
		}
		shas = append(shas, sha)
	}
	return shas, -1, "", nil
}

// PushRefs force-pushes commits to refs of the test fork in one push.
func (w *WorkRepo) PushRefs(ctx context.Context, refs map[string]string) error {
	name, err := w.remote(ctx, w.TestRemote)
	if err != nil {
		return err
	}
	args := []string{"push", "-q", "-f", name}
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	for _, ref := range keys {
		w.Git.log().Info(fmt.Sprintf("Publishing recovery ref %s at %s.", ref, refs[ref]))
		args = append(args, refs[ref]+":"+ref)
	}
	g, err := w.testGit(ctx)
	if err != nil {
		return err
	}
	_, err = g.Run(ctx, args...)
	return err
}

// testGit returns the git runner for requests to the test fork.
func (w *WorkRepo) testGit(ctx context.Context) (Git, error) {
	g := *w.Git
	if w.TestAuth == nil {
		return g, nil
	}
	env, err := w.TestAuth(ctx)
	if err != nil {
		return g, fmt.Errorf("test fork credentials: %w", err)
	}
	g.Env = append(append([]string(nil), g.Env...), env...)
	return g, nil
}

// TestRefs fetches those of the given refs that exist in the test
// fork and returns their commits.
func (w *WorkRepo) TestRefs(ctx context.Context, refs []string) (map[string]string, error) {
	name, err := w.remote(ctx, w.TestRemote)
	if err != nil {
		return nil, err
	}
	g, err := w.testGit(ctx)
	if err != nil {
		return nil, err
	}
	w.Git.log().Info(fmt.Sprintf("Looking up %d recovery refs in the test fork.", len(refs)))
	out, err := g.Run(ctx, append([]string{"ls-remote", name}, refs...)...)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, r := range refs {
		wanted[r] = true
	}
	found := map[string]string{}
	fetch := []string{"fetch", "-q", "--no-tags", "--filter=blob:none", name}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok || !wanted[ref] {
			continue
		}
		found[ref] = sha
		dst := "refs/leadtest-cache/" + name + "/" + strings.TrimPrefix(ref, "refs/")
		fetch = append(fetch, "+"+ref+":"+dst)
	}
	if len(found) == 0 {
		return found, nil
	}
	w.Git.log().Info(fmt.Sprintf("Fetching %d recovery commits from the test fork.",
		len(found)))
	if _, err := g.Run(ctx, fetch...); err != nil {
		return nil, err
	}
	return found, nil
}

func (w *WorkRepo) Parent(ctx context.Context, sha string) (string, error) {
	return w.Git.Out(ctx, "rev-parse", "--verify", sha+"^1")
}

func (w *WorkRepo) ChangedFiles(ctx context.Context, from, to string) ([]string, error) {
	out, err := w.Git.Out(ctx, "diff", "--name-only", "--no-renames", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

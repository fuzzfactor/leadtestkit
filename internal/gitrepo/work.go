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
	// TestRemote is the HTTPS URL of the test repository.
	TestRemote string
	// TestAuth returns the environment that authenticates requests to
	// the test repository, see AuthEnv.  It is asked every time, so that
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
	w.abortRebase(ctx)
	w.initialized = true
	return nil
}

func (w *WorkRepo) abortRebase(ctx context.Context) {
	dir := filepath.Join(w.Git.Dir, ".git", "rebase-merge")
	if _, err := os.Stat(dir); err != nil {
		return
	}
	w.Git.log().Info("Aborting an unfinished git rebase from an earlier sync.")
	if _, err := w.Git.Run(ctx, "rebase", "--abort"); err != nil {
		os.RemoveAll(dir)
	}
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

// SameChange reports whether a commit makes the change of an email,
// comparing stable patch IDs, which ignore whitespace and line numbers.
func (w *WorkRepo) SameChange(ctx context.Context, sha string, mbox []byte) (bool, error) {
	diff, err := w.Git.Run(ctx, "diff-tree", "-p", sha)
	if err != nil {
		return false, err
	}
	commit, err := w.patchID(ctx, []byte(diff))
	if err != nil {
		return false, err
	}
	email, err := w.patchID(ctx, mbox)
	if err != nil {
		return false, err
	}
	return commit == email, nil
}

func (w *WorkRepo) patchID(ctx context.Context, diff []byte) (string, error) {
	out, err := w.Git.RunInput(ctx, diff, "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	return id, nil
}

// PushRefs force-pushes commits to refs of the test repository in one push.
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

// testGit returns the git runner for requests to the test repository.
func (w *WorkRepo) testGit(ctx context.Context) (Git, error) {
	g := *w.Git
	if w.TestAuth == nil {
		return g, nil
	}
	env, err := w.TestAuth(ctx)
	if err != nil {
		return g, fmt.Errorf("test repository credentials: %w", err)
	}
	g.Env = append(append([]string(nil), g.Env...), env...)
	return g, nil
}

// TestRefs fetches those of the given refs that exist in the test
// repository and returns their commits.
func (w *WorkRepo) TestRefs(ctx context.Context, refs []string) (map[string]string, error) {
	name, err := w.remote(ctx, w.TestRemote)
	if err != nil {
		return nil, err
	}
	g, err := w.testGit(ctx)
	if err != nil {
		return nil, err
	}
	w.Git.log().Info(fmt.Sprintf("Looking up %d recovery refs in the test repository.",
		len(refs)))
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
	w.Git.log().Info(fmt.Sprintf("Fetching %d recovery commits from the test repository.",
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

// SyncResult describes a sync of a branch of the test repository.
type SyncResult struct {
	Old, New string
	// Commits counts the commits of the branch that are not upstream.
	Commits int
}

// ConflictError is a rebase that needs a human.
type ConflictError struct {
	Files  []string
	Output string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflicts in %s", strings.Join(e.Files, ", "))
}

// SyncBranch rebases the commits of a branch of the test repository that are
// not upstream onto the upstream branch, like git pull --rebase, and
// force-pushes the result unless the branch changed in the meantime.
// Merge commits of earlier syncs are dropped.  New is Old when the
// branch already contains upstream.
func (w *WorkRepo) SyncBranch(ctx context.Context, upstreamURL, upstreamBranch,
	branch string) (SyncResult, error) {
	var res SyncResult
	up, err := w.Resolve(ctx, upstreamURL, upstreamBranch)
	if err != nil {
		return res, err
	}
	if res.Old, err = w.Resolve(ctx, w.TestRemote, branch); err != nil {
		return res, err
	}
	res.New = res.Old
	if res.Commits, err = w.count(ctx, "--no-merges", up+".."+res.Old); err != nil {
		return res, err
	}
	if _, err := w.Git.Run(ctx, "merge-base", "--is-ancestor", up, res.Old); err == nil {
		w.Git.log().Info(fmt.Sprintf("Branch %s of the test repository already contains "+
			"upstream %s.", branch, short(up)))
		return res, nil
	}
	if _, err := w.Git.Run(ctx, "checkout", "-q", "--detach", "-f", res.Old); err != nil {
		return res, err
	}
	if _, err := w.Git.Run(ctx, "clean", "-q", "-fdx"); err != nil {
		return res, err
	}
	g := *w.Git
	g.Env = append(append([]string(nil), g.Env...), w.env()...)
	w.Git.log().Info(fmt.Sprintf("Rebasing %d commits of %s onto upstream %s.",
		res.Commits, branch, short(up)))
	if _, err := g.Run(ctx, "rebase", "-q", "--no-gpg-sign", up); err != nil {
		var gerr *Error
		if !errors.As(err, &gerr) {
			return res, err
		}
		files, _ := w.Git.Out(ctx, "diff", "--name-only", "--diff-filter=U")
		w.abortRebase(ctx)
		return res, &ConflictError{Files: strings.Fields(files),
			Output: strings.TrimSpace(gerr.Stdout + "\n" + gerr.Stderr)}
	}
	if res.New, err = w.Git.Out(ctx, "rev-parse", "HEAD"); err != nil {
		return res, err
	}
	if res.Commits, err = w.count(ctx, up+".."+res.New); err != nil {
		return res, err
	}
	name, err := w.remote(ctx, w.TestRemote)
	if err != nil {
		return res, err
	}
	tg, err := w.testGit(ctx)
	if err != nil {
		return res, err
	}
	ref := "refs/heads/" + branch
	w.Git.log().Info(fmt.Sprintf("Pushing %s at %s over %s.", ref, short(res.New),
		short(res.Old)))
	_, err = tg.Run(ctx, "push", "-q", "--force-with-lease="+ref+":"+res.Old, name,
		res.New+":"+ref)
	return res, err
}

func (w *WorkRepo) count(ctx context.Context, revs ...string) (int, error) {
	out, err := w.Git.Out(ctx, append([]string{"rev-list", "--count"}, revs...)...)
	n := 0
	fmt.Sscan(out, &n)
	return n, err
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

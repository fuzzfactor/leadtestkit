// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"leadtestkit/internal/pw"
)

var testEnv = []string{
	"GIT_AUTHOR_NAME=Lorem Ipsum", "GIT_AUTHOR_EMAIL=lorem@example.org",
	"GIT_COMMITTER_NAME=Lorem Ipsum", "GIT_COMMITTER_EMAIL=lorem@example.org",
	"GIT_CONFIG_GLOBAL=/dev/null",
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), testEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup creates an upstream repository with a main branch and returns
// two patches made on top of it with format-patch, plus one that does
// not apply.
func setup(t *testing.T) (upstream, test string, patches [][]byte) {
	root := t.TempDir()
	upstream = filepath.Join(root, "upstream")
	test = filepath.Join(root, "test.git")
	src := filepath.Join(root, "src")
	run(t, root, "init", "-q", "--bare", test)
	run(t, root, "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "lorem.txt"), "lorem\nipsum\n")
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "initial")
	run(t, root, "clone", "-q", "--bare", src, upstream)

	write(t, filepath.Join(src, "lorem.txt"), "lorem\nipsum\ndolor\n")
	run(t, src, "commit", "-qam", "lorem: Add dolor.")
	write(t, filepath.Join(src, "sit.txt"), "sit\n")
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "sit: Add file.")
	for _, rev := range []string{"HEAD~1", "HEAD"} {
		patches = append(patches,
			[]byte(run(t, src, "format-patch", "--stdout", "-1", rev)+"\n"))
	}
	bad := strings.Replace(string(patches[0]), "-ipsum", "-amet", 1)
	bad = strings.Replace(bad, " ipsum\n", " amet\n", 1)
	patches = append(patches, []byte(bad))
	return
}

func newWork(t *testing.T, test string) *WorkRepo {
	return &WorkRepo{
		Git: &Git{Dir: filepath.Join(t.TempDir(), "work"),
			Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}},
		TestRemote: test,
		Committer:  "Robot <robot@example.org>",
	}
}

func TestApplyAndPush(t *testing.T) {
	upstream, test, patches := setup(t)
	ctx := context.Background()
	w := newWork(t, test)
	base, err := w.Resolve(ctx, upstream, "main")
	if err != nil || len(base) != 40 {
		t.Fatalf("base=%q err=%v", base, err)
	}
	shas, failed, out, err := w.Apply(ctx, base, patches[:2])
	if err != nil || failed != -1 || len(shas) != 2 {
		t.Fatalf("shas=%v failed=%d out=%q err=%v", shas, failed, out, err)
	}
	committer, _ := w.Git.Out(ctx, "log", "-1", "--format=%cn <%ce>|%an", shas[1])
	if committer != "Robot <robot@example.org>|Lorem Ipsum" {
		t.Errorf("committer = %q", committer)
	}
	files, err := w.ChangedFiles(ctx, base, shas[1])
	if err != nil || strings.Join(files, ",") != "lorem.txt,sit.txt" {
		t.Errorf("files=%v err=%v", files, err)
	}
	if err := w.PushRefs(ctx, map[string]string{
		"refs/leadtest/series_1": shas[1],
		"refs/leadtest/patch_1":  shas[0],
	}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, test, "rev-parse", "refs/leadtest/series_1"); got != shas[1] {
		t.Errorf("pushed %q, want %q", got, shas[1])
	}

	w2 := newWork(t, test)
	sha, err := w2.ResolveTestRef(ctx, "refs/leadtest/series_1")
	if err != nil || sha != shas[1] {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	data, err := w2.ReadFile(ctx, sha, "sit.txt")
	if err != nil || string(data) != "sit\n" {
		t.Errorf("data=%q err=%v", data, err)
	}
	found, err := w2.TestRefs(ctx, []string{"refs/leadtest/patch_1", "refs/leadtest/patch_2"})
	if err != nil || len(found) != 1 || found["refs/leadtest/patch_1"] != shas[0] {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if parent, err := w2.Parent(ctx, shas[1]); err != nil || parent != shas[0] {
		t.Errorf("parent=%q err=%v", parent, err)
	}
}

func TestApplyFailure(t *testing.T) {
	upstream, test, patches := setup(t)
	ctx := context.Background()
	w := newWork(t, test)
	base, _ := w.Resolve(ctx, upstream, "main")
	shas, failed, out, err := w.Apply(ctx, base, [][]byte{patches[2], patches[1]})
	if err != nil || failed != 0 || len(shas) != 0 || out == "" {
		t.Fatalf("shas=%v failed=%d out=%q err=%v", shas, failed, out, err)
	}
	// The repository must be usable again after a failure.
	shas, failed, out, err = w.Apply(ctx, base, patches[:2])
	if err != nil || failed != -1 || len(shas) != 2 {
		t.Fatalf("second apply: shas=%v failed=%d out=%q err=%v", shas, failed, out, err)
	}
}

func TestResolveMissingBranch(t *testing.T) {
	upstream, test, _ := setup(t)
	w := newWork(t, test)
	if _, err := w.Resolve(context.Background(), upstream, "branch-0.0"); err == nil {
		t.Error("expected error for missing branch")
	}
}

func TestTestAuthAskedForEveryRequest(t *testing.T) {
	upstream, test, patches := setup(t)
	ctx := context.Background()
	w := newWork(t, test)
	calls := 0
	w.TestAuth = func(ctx context.Context) ([]string, error) {
		calls++
		return []string{"LEADTEST_LOREM=1"}, nil
	}
	base, _ := w.Resolve(ctx, upstream, "main")
	if calls != 0 {
		t.Errorf("credentials for the test repository were sent to upstream")
	}
	shas, _, _, err := w.Apply(ctx, base, patches[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := w.PushRefs(ctx, map[string]string{"refs/leadtest/patch_1": shas[0]}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.TestRefs(ctx, []string{"refs/leadtest/patch_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ResolveTestRef(ctx, "refs/leadtest/patch_1"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("credentials asked %d times, want once per test repository request", calls)
	}
	w.TestAuth = func(ctx context.Context) ([]string, error) {
		return nil, errors.New("lorem key rejected")
	}
	err = w.PushRefs(ctx, map[string]string{"refs/leadtest/patch_2": shas[0]})
	if err == nil || !strings.Contains(err.Error(), "lorem key rejected") {
		t.Errorf("err = %v", err)
	}
}

func TestSameChange(t *testing.T) {
	upstream, test, patches := setup(t)
	ctx := context.Background()
	w := newWork(t, test)
	base, _ := w.Resolve(ctx, upstream, "main")
	shas, _, _, err := w.Apply(ctx, base, patches[:2])
	if err != nil || len(shas) != 2 {
		t.Fatalf("shas=%v err=%v", shas, err)
	}
	for i, want := range []bool{true, false} {
		same, err := w.SameChange(ctx, shas[0], patches[i])
		if err != nil || same != want {
			t.Errorf("patch %d: same=%t err=%v", i, same, err)
		}
	}
}

func TestAuthEnv(t *testing.T) {
	env := AuthEnv("github.com", "lorem-token")
	if len(env) != 3 || !strings.HasPrefix(env[2],
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic ") ||
		strings.Contains(strings.Join(env, " "), "lorem-token") {
		t.Errorf("env = %v", env)
	}
	if AuthEnv("github.com", "") != nil {
		t.Error("empty token must not produce env")
	}
}

// syncSetup creates an upstream repository and a test repository whose main
// has a commit of its own, merged with upstream once, like Sync fork
// does.  It returns the paths and a clone of upstream to advance it.
func syncSetup(t *testing.T) (upstream, test, upSrc string) {
	root := t.TempDir()
	upstream = filepath.Join(root, "upstream.git")
	test = filepath.Join(root, "test.git")
	upSrc = filepath.Join(root, "up")
	fork := filepath.Join(root, "fork")
	run(t, root, "init", "-q", "-b", "main", upSrc)
	write(t, filepath.Join(upSrc, "lorem.txt"), "lorem\nipsum\n")
	run(t, upSrc, "add", ".")
	run(t, upSrc, "commit", "-qm", "initial")
	run(t, root, "clone", "-q", "--bare", upSrc, upstream)
	run(t, upSrc, "remote", "add", "origin", upstream)

	run(t, root, "clone", "-q", upstream, fork)
	write(t, filepath.Join(fork, "ci.yml"), "lorem: ci\n")
	run(t, fork, "add", ".")
	run(t, fork, "commit", "-qm", "ci: Add the leadtest workflow.")
	write(t, filepath.Join(upSrc, "sit.txt"), "sit\n")
	run(t, upSrc, "add", ".")
	run(t, upSrc, "commit", "-qm", "sit: Add file.")
	run(t, upSrc, "push", "-q", "origin", "main")
	run(t, fork, "pull", "-q", "--no-rebase", "--no-edit", "origin", "main")
	run(t, root, "clone", "-q", "--bare", fork, test)
	return
}

func TestSyncBranch(t *testing.T) {
	upstream, test, upSrc := syncSetup(t)
	write(t, filepath.Join(upSrc, "amet.txt"), "amet\n")
	run(t, upSrc, "add", ".")
	run(t, upSrc, "commit", "-qm", "amet: Add file.")
	run(t, upSrc, "push", "-q", "origin", "main")

	ctx := context.Background()
	w := newWork(t, test)
	res, err := w.SyncBranch(ctx, upstream, "main", "main")
	if err != nil || res.Commits != 1 || res.New == res.Old {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := run(t, test, "rev-parse", "main"); got != res.New {
		t.Fatalf("main = %s, want %s", got, res.New)
	}
	// The commit of the fork is on top of upstream, without merges.
	log := run(t, test, "log", "--format=%s|%cn|%an", "main")
	want := "ci: Add the leadtest workflow.|Robot|Lorem Ipsum\n" +
		"amet: Add file.|Lorem Ipsum|Lorem Ipsum\n" +
		"sit: Add file.|Lorem Ipsum|Lorem Ipsum\n" +
		"initial|Lorem Ipsum|Lorem Ipsum"
	if log != want {
		t.Fatalf("history:\n%s", log)
	}

	again, err := w.SyncBranch(ctx, upstream, "main", "main")
	if err != nil || again.New != res.New || again.Old != res.New || again.Commits != 1 {
		t.Fatalf("second sync: %+v, %v", again, err)
	}
}

func TestSyncBranchConflict(t *testing.T) {
	upstream, test, upSrc := syncSetup(t)
	write(t, filepath.Join(upSrc, "ci.yml"), "ipsum: ci\n")
	run(t, upSrc, "add", ".")
	run(t, upSrc, "commit", "-qm", "ci: Add another workflow.")
	run(t, upSrc, "push", "-q", "origin", "main")
	before := run(t, test, "rev-parse", "main")

	w := newWork(t, test)
	_, err := w.SyncBranch(context.Background(), upstream, "main", "main")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || strings.Join(conflict.Files, ",") != "ci.yml" {
		t.Fatalf("err = %v", err)
	}
	if got := run(t, test, "rev-parse", "main"); got != before {
		t.Errorf("main changed after a conflict")
	}
	// The work repository is usable again.
	if _, err := w.Resolve(context.Background(), upstream, "main"); err != nil {
		t.Fatal(err)
	}
}

// TestApplyKeepsAuthors applies patch emails from a list that rewrote
// From for DMARC: one sent on behalf of its author, named in a "From:"
// line of the body, and one by its author.
func TestApplyKeepsAuthors(t *testing.T) {
	upstream, test, _ := setup(t)
	ctx := context.Background()
	rewritten := func(subject, content, diff string) []byte {
		p := &pw.PatchDetail{
			Headers: map[string]any{
				"From":     "Dolor via dev <dev@lists.example.org>",
				"Reply-To": "dolor@example.org",
				"Subject":  subject,
				"Date":     "Mon, 2 Mar 2026 10:00:00 +0100",
			},
			Content: content,
			Diff:    diff,
		}
		p.ID, p.MsgID = 1, "<1@example.org>"
		mbox, err := pw.BuildMbox(p)
		if err != nil {
			t.Fatal(err)
		}
		return mbox
	}
	patches := [][]byte{
		rewritten("[PATCH 1/2] lorem: Add dolor.",
			"From: Sit Amet <sit@example.org>\n\nAdd dolor.\n\n"+
				"Signed-off-by: Sit Amet <sit@example.org>\n"+
				"Signed-off-by: Dolor <dolor@example.org>\n",
			"diff --git a/lorem.txt b/lorem.txt\n--- a/lorem.txt\n+++ b/lorem.txt\n"+
				"@@ -1,2 +1,3 @@\n lorem\n ipsum\n+dolor\n"),
		rewritten("[PATCH 2/2] sit: Add file.",
			"Add file.\n\nSigned-off-by: Dolor <dolor@example.org>\n",
			"diff --git a/sit.txt b/sit.txt\nnew file mode 100644\n--- /dev/null\n"+
				"+++ b/sit.txt\n@@ -0,0 +1 @@\n+sit\n"),
	}
	w := newWork(t, test)
	base, err := w.Resolve(ctx, upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	shas, failed, out, err := w.Apply(ctx, base, patches)
	if err != nil || failed != -1 || len(shas) != 2 {
		t.Fatalf("shas=%v failed=%d out=%q err=%v", shas, failed, out, err)
	}
	for i, want := range []string{"Sit Amet <sit@example.org>|lorem: Add dolor.",
		"Dolor <dolor@example.org>|sit: Add file."} {
		got, _ := w.Git.Out(ctx, "log", "-1", "--format=%an <%ae>|%s", shas[i])
		if got != want {
			t.Errorf("patch %d: author and subject %q, want %q", i+1, got, want)
		}
	}
	msg, _ := w.Git.Out(ctx, "log", "-1", "--format=%b", shas[0])
	if strings.Contains(msg, "From:") {
		t.Errorf("the in-body From is left in the message:\n%s", msg)
	}
}

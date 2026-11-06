// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		patches = append(patches, []byte(run(t, src, "format-patch", "--stdout", "-1", rev)+"\n"))
	}
	bad := strings.Replace(string(patches[0]), "-ipsum", "-amet", 1)
	bad = strings.Replace(bad, " ipsum\n", " amet\n", 1)
	patches = append(patches, []byte(bad))
	return
}

func newWork(t *testing.T, test string) *WorkRepo {
	return &WorkRepo{
		Git:        &Git{Dir: filepath.Join(t.TempDir(), "work"), Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}},
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

func TestAuthEnv(t *testing.T) {
	env := AuthEnv("github.com", "lorem-token")
	if len(env) != 3 || !strings.HasPrefix(env[2], "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic ") ||
		strings.Contains(strings.Join(env, " "), "lorem-token") {
		t.Errorf("env = %v", env)
	}
	if AuthEnv("github.com", "") != nil {
		t.Error("empty token must not produce env")
	}
}

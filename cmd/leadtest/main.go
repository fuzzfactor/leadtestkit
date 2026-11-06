// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/config"
	"leadtestkit/internal/gh"
	"leadtestkit/internal/gitrepo"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/logx"
	"leadtestkit/internal/mail"
	"leadtestkit/internal/pages"
	"leadtestkit/internal/pipeline"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

const version = "0.1"

func usage() {
	fmt.Fprintf(os.Stderr, `usage: leadtest <command> [options]

commands:
  run           run the CI loop on a state checkout
  validate      check config.yaml and the state
  render-pages  generate the GitHub Pages site from the state
  version       print the version
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	httpx.UserAgent = "leadtestkit/" + version
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "render-pages":
		err = cmdRenderPages(os.Args[2:])
	case "version":
		fmt.Println("leadtest", version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "leadtest:", logx.Redact(err.Error()))
		os.Exit(1)
	}
}

func loadConfig(stateDir string) (*config.Config, error) {
	return config.Load(filepath.Join(stateDir, "config.yaml"))
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	stateDir := fs.String("state", "state", "state branch checkout")
	fs.Parse(args)
	cfg, err := loadConfig(*stateDir)
	if err != nil {
		return err
	}
	st, err := state.Load(*stateDir)
	if err != nil {
		return err
	}
	months, err := st.HistoryMonths()
	if err != nil {
		return err
	}
	for _, m := range months {
		if _, err := st.History(m); err != nil {
			return err
		}
	}
	fmt.Printf("config ok: %s (%s), %d queued series, %d history months\n",
		cfg.Name, cfg.Patchwork.Project, len(st.Queue), len(months))
	return nil
}

func cmdRenderPages(args []string) error {
	fs := flag.NewFlagSet("render-pages", flag.ExitOnError)
	stateDir := fs.String("state", "state", "state branch checkout")
	out := fs.String("out", "_site", "output directory")
	fs.Parse(args)
	logx.Setup(false)
	cfg, err := loadConfig(*stateDir)
	if errors.Is(err, config.ErrNotConfigured) {
		slog.Info("not configured, no pages")
		return nil
	}
	if err != nil {
		return err
	}
	if !cfg.Pages.Enabled {
		slog.Info("pages disabled")
		return nil
	}
	st, err := state.Load(*stateDir)
	if err != nil {
		return err
	}
	if err := pages.Render(st, cfg, *out, time.Now(), runURL()); err != nil {
		return err
	}
	return githubOutput("pages", "true")
}

func runURL() string {
	server, repo, id := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || id == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, id)
}

func githubOutput(key, value string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s=%s\n", key, value)
	return err
}

type runner struct {
	cfg      *config.Config
	stateDir string
	repo     *state.Repo
	p        *pipeline.Pipeline
	log      *slog.Logger
}

// checkpoint reloads the state when the remote state changed.
func (r *runner) checkpoint(ctx context.Context, stage string) error {
	if err := r.p.Store.Save(); err != nil {
		return err
	}
	msg := fmt.Sprintf("leadtest: %s\n\n%s", r.p.Stats.Summary(), stage)
	committed, err := r.repo.Commit(ctx, msg)
	if err != nil || !committed {
		return err
	}
	rebased, err := r.repo.Push(ctx)
	if rebased || errors.Is(err, state.ErrConflict) {
		st, lerr := state.Load(r.stateDir)
		if lerr != nil {
			return fmt.Errorf("reload state: %w", lerr)
		}
		r.p.Store = st
		r.log.Warn("state reloaded after a concurrent change")
	}
	return err
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	stateDir := fs.String("state", "state", "state branch checkout")
	workDir := fs.String("workdir", filepath.Join(os.TempDir(), "leadtest-work"), "work directory")
	once := fs.Bool("once", false, "run a single step")
	dryRun := fs.Bool("dry-run", false, "do not push, post checks or send emails")
	noCommit := fs.Bool("no-commit", false, "save the state but do not commit or push it")
	debug := fs.Bool("debug", false, "debug logging")
	fs.Parse(args)
	log := logx.Setup(*debug)

	cfg, err := loadConfig(*stateDir)
	if errors.Is(err, config.ErrNotConfigured) {
		log.Info("no config.yaml on the state branch, nothing to do")
		return nil
	}
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		log.Info("disabled in config.yaml, nothing to do")
		return nil
	}
	cfg.LoadSecrets(os.Getenv)
	if *dryRun {
		cfg.Mode.Push, cfg.Mode.Checks, cfg.Mode.Email = "off", "log", "off"
	}
	if err := cfg.CheckSecrets(); err != nil {
		return err
	}
	log.Info("starting", "version", version, "project", cfg.Patchwork.Project,
		"test_repo", cfg.TestRepo.Repo, "push", cfg.Mode.Push, "checks", cfg.Mode.Checks,
		"email", cfg.Mode.Email)

	st, err := state.Load(*stateDir)
	if err != nil {
		return err
	}
	p, err := buildPipeline(cfg, st, *workDir, log)
	if err != nil {
		return err
	}
	r := &runner{
		cfg:      cfg,
		stateDir: *stateDir,
		p:        p,
		log:      log,
		repo: &state.Repo{
			Git:    &gitrepo.Git{Dir: *stateDir, Log: log},
			Remote: "origin",
			Branch: "state",
			Author: cfg.Runner.CommitAuthor,
			Log:    log,
		},
	}
	p.Checkpoint = r.checkpoint
	if *noCommit {
		p.Checkpoint = func(ctx context.Context, stage string) error { return p.Store.Save() }
	}

	duration := cfg.Runner.LoopDuration.D()
	interval := cfg.Runner.LoopInterval.D()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, duration+10*time.Minute)
	defer cancel()

	deadline := time.Now().Add(duration)
	for {
		final := *once || time.Until(deadline) < interval
		p.Step(ctx, final)
		if final || ctx.Err() != nil {
			break
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}

	// Always try to save, even after cancellation.
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer saveCancel()
	cpErr := p.Checkpoint(saveCtx, "run finished")
	writeSummary(cfg, p)
	log.Info("finished", "summary", p.Stats.Summary())
	if cpErr != nil {
		return fmt.Errorf("saving state: %w", cpErr)
	}
	return nil
}

func buildPipeline(cfg *config.Config, st *state.Store, workDir string, log *slog.Logger) (*pipeline.Pipeline, error) {
	pwc, err := pw.New(pw.Options{
		APIURL:   cfg.Patchwork.APIURL,
		Project:  cfg.Patchwork.Project,
		Token:    cfg.Secrets.PatchworkToken,
		MinDelay: cfg.Patchwork.MinDelay.D(),
		Logger:   log,
	})
	if err != nil {
		return nil, err
	}
	ghc, err := gh.New(gh.Options{
		APIURL: cfg.TestRepo.APIURL,
		Repo:   cfg.TestRepo.Repo,
		Token:  cfg.Secrets.GitHubToken,
		Logger: log,
	})
	if err != nil {
		return nil, err
	}
	newArchive := func(base, name string) pipeline.Archive {
		if base == "" {
			return nil
		}
		return archive.NewPipermail(base, httpx.New(httpx.Options{
			Name:      name,
			MinDelay:  cfg.Archive.MinDelay.D(),
			CurlFirst: true,
			Logger:    log,
		}))
	}
	var sender mail.Sender = &mail.Log{Log: log}
	if !cfg.Mode.EmailOff() {
		// The allowlist is what keeps test reports away from real people.
		var allowed []string
		if cfg.Mode.TestEmail() {
			allowed = cfg.Mode.AllowedRecipients
		}
		sender = &mail.SMTP{
			Host:     cfg.SMTP.Host,
			Port:     cfg.SMTP.Port,
			Security: cfg.SMTP.Security,
			Username: cfg.Secrets.SMTPUsername,
			Password: cfg.Secrets.SMTPPassword,
			Allowed:  allowed,
			Log:      log,
		}
	}
	work := &gitrepo.WorkRepo{
		Git: &gitrepo.Git{
			Dir: filepath.Join(workDir, "repo"),
			Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"},
			Log: log,
		},
		TestRemote: fmt.Sprintf("https://%s/%s.git", cfg.TestRepo.GitHost, cfg.TestRepo.Repo),
		TestEnv:    gitrepo.AuthEnv(cfg.TestRepo.GitHost, cfg.Secrets.GitHubToken),
		Committer:  cfg.Runner.CommitAuthor,
	}
	p := &pipeline.Pipeline{
		Cfg:   cfg,
		Store: st,
		PW:    pwc,
		GH:    ghc,
		Repo:  work,
		Mail:  sender,
		Log:   log,
	}
	if r := newArchive(cfg.Archive.ReportsURL, "reports-archive"); r != nil {
		p.Reports = r
	}
	if d := newArchive(cfg.Archive.DevURL, "dev-archive"); d != nil {
		p.Dev = d
	}
	return p, nil
}

func writeSummary(cfg *config.Config, p *pipeline.Pipeline) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	counts := map[string]int{}
	for _, s := range p.Store.Queue {
		counts[s.Status]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## LeadTestKit: %s\n\n", cfg.Name)
	fmt.Fprintf(&b, "Mode: push `%s`, checks `%s`, email `%s`\n\n", cfg.Mode.Push, cfg.Mode.Checks, cfg.Mode.Email)
	fmt.Fprintf(&b, "This run: %s\n\n", p.Stats.Summary())
	b.WriteString("| Queue | Series |\n|---|---|\n")
	for _, st := range []string{state.SeriesNew, state.SeriesQueued, state.SeriesWaiting, state.SeriesTesting} {
		fmt.Fprintf(&b, "| %s | %d |\n", st, counts[st])
	}
	var failing []string
	for _, s := range p.Store.Queue {
		if s.Failure != nil {
			failing = append(failing, fmt.Sprintf("- series %d: %s: %s (%d attempts)",
				s.ID, s.Failure.Op, s.Failure.LastError, s.Failure.Attempts))
		}
	}
	if len(p.Stats.Errors) > 0 || len(failing) > 0 {
		b.WriteString("\n### Problems\n\n")
		for _, e := range p.Stats.Errors {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		for _, f := range failing {
			b.WriteString(f + "\n")
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(logx.Redact(b.String()))
}

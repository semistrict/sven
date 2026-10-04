// Command sven vibe checks your commits at the door. It asks a System One
// model yes/no questions about each staged file, one file per request and
// many at once, and turns the commit away if any rule is likely broken.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
		"strings"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/internal/cache"
	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/internal/diff"
	"github.com/semistrict/sven/internal/git"
	"github.com/semistrict/sven/internal/provider"
)

const (
	exitPass     = 0
	exitRejected = 1
	exitError    = 2

	// chunkBytes keeps each request's diff well inside the models' 32k token
	// state limit; smaller states are also judged more accurately.
	chunkBytes  = 32 * 1024
	concurrency = 8

	// cacheDir holds remembered answers, relative to the work tree root.
	cacheDir = ".sven/cache"

	hookMarker = "# sven: vibe checks your commits at the door."
	hookScript = "#!/bin/sh\n" + hookMarker + " Skip once with --no-verify.\nexec sven check --cached\n"
)

// turnedAway ends every run that exits non-zero.
const turnedAway = "sven: heute leider nicht.\n      (git commit --no-verify gets you in anyway)\n"

const usage = `sven vibe checks your commits at the door.

Usage:
  sven [check] [-config file] [-v] [path...]      judge unstaged changes, like git diff
  sven check --cached [path...]                   judge staged changes
  sven check --rev range [path...]                judge a revision range
  git diff | sven check --patch                   judge a diff from standard input
  sven install-git-hook [-force]                  install as the git pre-commit hook
  sven init                                       write .sven.yaml with the built-in rules

Environment:
  TYPESAFE_API_KEY                                for provider typesafe (Jev)
  CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN     for provider cloudflare (Clef)
  SVEN_PROVIDER, SVEN_MODEL                       override .sven.yaml
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "check"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "check":
		var rejected bool
		rejected, err = check(ctx, args, stdin, stdout, stderr)
		if err == nil && rejected {
			return exitRejected
		}
	case "install-git-hook":
		err = installGitHook(ctx, args, stdout, stderr)
	case "init":
		err = initConfig(ctx, args, stdout, stderr)
	case "help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprint(stderr, usage)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitPass
	}
	if err != nil {
		fmt.Fprintf(stderr, "sven: %v\n", err)
		fmt.Fprint(stderr, turnedAway)
		return exitError
	}
	return exitPass
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("sven "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func check(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (rejected bool, err error) {
	fs := flags("check", stderr)
	cached := fs.Bool("cached", false, "judge staged changes, as git diff --cached shows them")
	rev := fs.String("rev", "", "judge a revision range, such as origin/main...HEAD")
	patch := fs.Bool("patch", false, "judge a unified diff read from standard input")
	configPath := fs.String("config", "", "root config file (default <work tree>/"+config.FileName+")")
	verbose := fs.Bool("v", false, "show every verdict, not just violations")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	paths := fs.Args()
	if sources := btoi(*cached) + btoi(*rev != "") + btoi(*patch); sources > 1 {
		return false, errors.New("use only one of --cached, --rev and --patch")
	}
	if *patch && len(paths) > 0 {
		return false, errors.New("--patch reads every file from the patch; leave out paths")
	}

	root, err := git.Root(ctx)
	if err != nil {
		return false, err
	}
	tree, err := config.Load(root, cmp.Or(*configPath, filepath.Join(root, config.FileName)))
	if err != nil {
		return false, err
	}
	var raw []byte
	switch {
	case *patch:
		raw, err = io.ReadAll(stdin)
	case *cached:
		raw, err = git.Diff(ctx, append([]string{"--cached", "--"}, paths...)...)
	case *rev != "":
		raw, err = git.Diff(ctx, append([]string{*rev, "--"}, paths...)...)
	default:
		raw, err = git.Diff(ctx, append([]string{"--"}, paths...)...)
	}
	if err != nil {
		return false, err
	}
	files, err := diff.Parse(bytes.NewReader(raw))
	if err != nil {
		return false, fmt.Errorf("parsing diff: %w", err)
	}
	var targets []bouncer.Target
	for _, f := range files {
		c, err := tree.For(path.Dir(f.Path))
		if err != nil {
			return false, err
		}
		if len(c.Rules) > 0 && !c.Excluded(f.Path) {
			targets = append(targets, bouncer.Target{File: f, Rules: c.Rules})
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "sven: nothing to check.")
		return false, nil
	}

	client, err := provider.New(tree.Provider, tree.Model)
	if err != nil {
		return false, err
	}
	evaluator, err := cache.New(filepath.Join(root, cacheDir), client.Endpoint()+" "+client.Model(), client)
	if err != nil {
		return false, err
	}
	b := bouncer.Bouncer{Evaluator: evaluator, ChunkBytes: chunkBytes, Concurrency: concurrency}
	report, err := b.Check(ctx, targets)
	if err != nil {
		return false, err
	}
	printReport(stdout, report, *verbose)
	fmt.Fprintf(stdout, "sven: %s\n", report.Usage.Summary(client.Model()))
	return report.Rejected(), nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func printReport(w io.Writer, report bouncer.Report, verbose bool) {
	shown := report.Violations()
	if verbose {
		shown = report.Verdicts
	}
	path := ""
	for _, v := range shown {
		if v.Path != path {
			path = v.Path
			fmt.Fprintf(w, "  %s\n", path)
		}
		why := ""
		if v.Level() != bouncer.OK {
			why = cmp.Or(v.Rule.Violation, v.Rule.Question)
		}
		line := fmt.Sprintf("    %s %-22s %3.0f%%  %s", marks[v.Level()], v.Rule.ID, v.P*100, why)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if len(shown) > 0 {
		fmt.Fprintln(w)
	}
	switch {
	case report.Rejected():
		fmt.Fprint(w, turnedAway)
	case len(report.Violations()) > 0:
		fmt.Fprintln(w, "sven: Na jut, rin mit dir. Aber benimm dich.")
	default:
		fmt.Fprintln(w, "sven: Na logen. Rin mit dir.")
	}
}

var marks = map[bouncer.Level]string{bouncer.OK: "✓", bouncer.Warn: "!", bouncer.Error: "✗"}

func installGitHook(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flags("install-git-hook", stderr)
	force := fs.Bool("force", false, "replace an existing pre-commit hook")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := git.HooksDir(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "pre-commit")
	existing, err := os.ReadFile(path)
	if err == nil && !bytes.Contains(existing, []byte(hookMarker)) && !*force {
		return fmt.Errorf("%s already exists: add `sven check --cached` to it, or replace it with -force", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(hookScript), 0o755); err != nil {
		return err
	}
	// WriteFile keeps an existing file's mode.
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "sven: installed %s\n", path)
	return nil
}

func initConfig(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := flags("init", stderr).Parse(args); err != nil {
		return err
	}
	root, err := git.Root(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(root, config.FileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(config.Default); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "sven: wrote %s\n", path)
	return nil
}

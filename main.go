// Command sven vibe checks your commits at the door. It asks a System One
// model yes/no questions about each staged file, one file per request and
// many at once, and turns the commit away if any rule is likely broken.
package main

import (
	"bufio"
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
	"slices"
	"strings"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/internal/cache"
	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/internal/diff"
	"github.com/semistrict/sven/internal/git"
	"github.com/semistrict/sven/internal/provider"
	"github.com/semistrict/sven/systemone"
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

const usage = `sven vibe checks your commits at the door.

Usage:
  sven [check] [--config file] [-v] [--no-color] [git diff args]
                                                  judge what git diff shows, e.g.
                                                  --cached, main...HEAD, -- paths
  sven check --all [-- path...]                   judge every tracked file
  git diff | sven check --patch                   judge a diff from standard input
  sven install-git-hook [-force]                  install as the git pre-commit hook
  sven init [--allow-request-storage]             write .sven.yaml, asking whether the
                                                  free sven API may store requests

Environment:
  TYPESAFE_API_KEY                                for provider typesafe (Jev)
  CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN     for provider cloudflare (Clef)
  SVEN_PROVIDER, SVEN_MODEL                       override .sven.yaml
  NO_COLOR, CLICOLOR_FORCE                        turn color off, or on when piped
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
		rejected, err = check(ctx, args, stdin, stdout, stderr, colors(stdout, args))
		if err == nil && rejected {
			return exitRejected
		}
	case "install-git-hook":
		err = installGitHook(ctx, args, stdout, stderr)
	case "init":
		err = initConfig(ctx, args, stdin, stdout, stderr)
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
		fmt.Fprintln(stderr, colors(stderr, args).red(fmt.Sprintf("sven: %v", err)))
		return exitError
	}
	return exitPass
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("sven "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func check(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, p palette) (rejected bool, err error) {
	fs := flags("check", stderr)
	patch := fs.Bool("patch", false, "judge a unified diff read from standard input")
	all := fs.Bool("all", false, "judge every tracked file, as if newly added")
	configPath := fs.String("config", "", "root config file (default <work tree>/"+config.FileName+")")
	verbose := fs.Bool("v", false, "show every verdict, not just violations")
	fs.Bool("no-color", false, "never color output (also NO_COLOR=1)")
	own, diffArgs := splitArgs(args)
	if err := fs.Parse(own); err != nil {
		return false, err
	}
	if *patch && *all {
		return false, errors.New("use either --patch or --all")
	}
	if *patch && len(diffArgs) > 0 {
		return false, errors.New("--patch reads the diff from standard input; leave out git diff arguments")
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
	if *patch {
		raw, err = io.ReadAll(stdin)
	} else {
		if *all {
			var empty string
			if empty, err = git.EmptyTree(ctx); err != nil {
				return false, err
			}
			diffArgs = append([]string{empty}, diffArgs...)
		}
		raw, err = git.Diff(ctx, diffArgs...)
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
		fmt.Fprintln(stdout, "sven: nothing to check: "+emptyBecause(*patch, *all, diffArgs, len(files)))
		return false, nil
	}

	client, err := provider.New(tree.Provider, tree.Model, tree.AllowRequestStorage)
	if err != nil {
		return false, err
	}
	evaluator, err := cache.New(filepath.Join(root, cacheDir), client.Endpoint()+" "+client.Model(), client)
	if err != nil {
		return false, err
	}
	b := bouncer.Bouncer{Evaluator: evaluator, ChunkBytes: chunkBytes, Concurrency: concurrency}
	prog := startProgress(stderr, colors(stderr, args), len(targets), func(u systemone.Usage) string {
		return price(client, u)
	})
	if prog != nil {
		b.Judged = prog.judged
	}
	report, err := b.Check(ctx, targets)
	prog.finish()
	if err != nil {
		return false, err
	}
	printReport(stdout, report, *verbose, p)
	fmt.Fprintln(stdout, p.dim("sven: "+report.Usage.Summary(client.Name())))
	return report.Rejected(), nil
}

// price is what usage costs on client, for the status line.
func price(client *systemone.Client, u systemone.Usage) string {
	if client.Name() == systemone.SvenName {
		return "free"
	}
	if cost, ok := systemone.Cost(client.Model(), u); ok {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("%d tokens", u.InputTokens)
}

// emptyBecause explains why a check found nothing to judge.
func emptyBecause(patch, all bool, diffArgs []string, files int) string {
	opts := diffArgs
	if i := slices.Index(diffArgs, "--"); i >= 0 {
		opts = diffArgs[:i]
	}
	switch {
	case files > 0:
		return "every changed file is excluded or has no rules."
	case patch:
		return "the patch changes no files."
	case all:
		return "no tracked files."
	case slices.Contains(opts, "--cached") || slices.Contains(opts, "--staged"):
		return "no staged changes."
	case len(opts) == 0:
		return "no unstaged changes. For staged ones, use sven check --cached; new files need git add first."
	}
	return "git diff " + strings.Join(diffArgs, " ") + " shows no changes."
}

// splitArgs separates sven's own flags from the arguments it passes on to
// git diff.
func splitArgs(args []string) (own, diffArgs []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return own, append(diffArgs, args[i:]...)
		case a == "-v" || a == "--patch" || a == "--all" || a == "--no-color" || a == "-h" || a == "--help" || strings.HasPrefix(a, "--config="):
			own = append(own, a)
		case a == "--config" && i+1 < len(args):
			own = append(own, a, args[i+1])
			i++
		default:
			diffArgs = append(diffArgs, a)
		}
	}
	return own, diffArgs
}

func printReport(w io.Writer, report bouncer.Report, verbose bool, p palette) {
	shown := report.Violations()
	if verbose {
		shown = report.Verdicts
	}
	paint := map[bouncer.Level]func(string) string{bouncer.OK: p.green, bouncer.Warn: p.yellow, bouncer.Error: p.red}
	path := ""
	for _, v := range shown {
		if v.Path != path {
			path = v.Path
			fmt.Fprintf(w, "  %s\n", p.bold(path))
		}
		why := ""
		if v.Level() != bouncer.OK {
			why = cmp.Or(v.Rule.Violation, v.Rule.Question)
		}
		verdict := paint[v.Level()](fmt.Sprintf("%s %-22s", marks[v.Level()], v.Rule.ID))
		line := fmt.Sprintf("    %s %3.0f%%  %s", verdict, v.P*100, why)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if len(shown) > 0 {
		fmt.Fprintln(w)
	}
	switch {
	case report.Rejected():
		printTurnedAway(w, p)
	case len(report.Violations()) > 0:
		fmt.Fprintln(w, p.yellow("sven: Na jut, rin mit dir. Aber benimm dich."))
	default:
		fmt.Fprintln(w, p.green("sven: Na logen. Rin mit dir."))
	}
}

// printTurnedAway ends a check that found error-level violations.
func printTurnedAway(w io.Writer, p palette) {
	fmt.Fprintln(w, p.bold(p.red("sven: heute leider nicht.")))
	fmt.Fprintln(w, p.dim("      (git commit --no-verify gets you in anyway)"))
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

const consentPrompt = `sven checks your changes with the free sven API unless you use your own key.
The free API stores the requests and responses it handles, encrypted, to
improve sven. Requests are your diffs.

Allow the free sven API to store this project's requests and responses? [y/N] `

func initConfig(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flags("init", stderr)
	allow := fs.Bool("allow-request-storage", false, "agree to the free sven API storing requests and responses, without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := git.Root(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(root, config.FileName)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if !*allow {
		fmt.Fprint(stdout, consentPrompt)
		answer, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		*allow = answer == "y" || answer == "yes"
	}

	content := string(config.Default)
	next := "the free sven API will check this project's changes"
	if *allow {
		content = strings.Replace(content, "# allow_request_storage: true", "allow_request_storage: true", 1)
	} else {
		content = strings.Replace(content, "provider: sven", "provider: typesafe", 1)
		next = "set TYPESAFE_API_KEY, or change provider to cloudflare"
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "sven: wrote %s: %s\n", path, next)
	return nil
}

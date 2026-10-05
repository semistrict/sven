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
	"log/slog"
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
	// exitInterrupted is the shell's code for a process stopped by Ctrl-C.
	exitInterrupted = 130

	// chunkBytes keeps each request's diff well inside the models' 32k token
	// state limit; smaller states are also judged more accurately.
	chunkBytes      = 32 * 1024
	defaultParallel = 8

	// cacheDir holds remembered answers, relative to the work tree root.
	cacheDir = ".sven/cache"

	hookMarker = "# sven: vibe checks your commits at the door."
	hookScript = "#!/bin/sh\n" + hookMarker + " Skip once with --no-verify.\nexec sven check --cached\n"
)

const usage = `sven vibe checks your commits at the door.

Usage:
  sven [check] [options] [git diff args]          judge what git diff shows, e.g.
                                                  --cached, main...HEAD, -- paths
  sven check --all [options] [-- path...]         judge every tracked file
  sven check --commit sha [options] [-- path...]  judge what one commit changed
  git diff | sven check --patch [options]         judge a diff from standard input
  sven install-git-hook [-force]                  install as the git pre-commit hook
  sven init [--allow-request-storage]             write .sven.yaml, asking whether the
                                                  free sven API may store requests

Check options, which override .sven.yaml:
  --with rule,...                                 turn rules on, e.g. --with sus
  --no rule,...                                   turn rules off
  --only rule,...                                 ask only these rules
  --errors-only                                   only hard failures; no warnings
  --advice text                                   tell the model about the code
  --lines                                         show the lines that break each rule
  --parallel n                                    requests at once (default 8)
  --provider name, --model name                   who answers
  --config file                                   root config file
  -v                                              show every verdict
  --no-color                                      never color output

Environment:
  TYPESAFE_API_KEY                                for provider typesafe (Jev)
  CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN     for provider cloudflare (Clef)
  SVEN_PROVIDER, SVEN_MODEL                       override .sven.yaml
  NO_COLOR, CLICOLOR_FORCE                        turn color off, or on when piped
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// After the first Ctrl-C, a second one quits at once.
	go func() {
		<-ctx.Done()
		stop()
	}()
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
	if errors.Is(err, errInterrupted) || (err != nil && ctx.Err() != nil) {
		return exitInterrupted
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
	commit := fs.String("commit", "", "judge what one commit changed, such as HEAD or a sha")
	lines := fs.Bool("lines", false, "show the lines that break each rule, dropping violations no line is likely to cause")
	configPath := fs.String("config", "", "root config file (default <work tree>/"+config.FileName+")")
	verbose := fs.Bool("v", false, "show every verdict, not just violations")
	fs.Bool("no-color", false, "never color output (also NO_COLOR=1)")
	parallel := fs.Int("parallel", defaultParallel, "how many requests to send to the model at once")
	providerName := fs.String("provider", "", "sven, typesafe, or cloudflare, instead of .sven.yaml's")
	model := fs.String("model", "", "the model to ask, instead of .sven.yaml's")
	var o config.Overrides
	fs.Var((*ruleList)(&o.With), "with", "turn rules on, such as ones off by default: --with sus")
	fs.Var((*ruleList)(&o.No), "no", "turn rules off: --no sus,emoji")
	fs.Var((*ruleList)(&o.Only), "only", "ask only these rules")
	fs.BoolVar(&o.ErrorsOnly, "errors-only", false, "only hard failures: skip warn-only rules, never warn")
	fs.Func("advice", "tell the model something about the code, after .sven.yaml's advice", func(a string) error {
		o.Advice = append(o.Advice, a)
		return nil
	})
	own, diffArgs := splitArgs(fs, args)
	if err := fs.Parse(own); err != nil {
		return false, err
	}
	if *parallel < 1 {
		return false, fmt.Errorf("--parallel %d: want at least 1", *parallel)
	}
	sources := 0
	for _, on := range []bool{*patch, *all, *commit != ""} {
		if on {
			sources++
		}
	}
	if sources > 1 {
		return false, errors.New("use only one of --patch, --all, and --commit")
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
	tree.Overrides = o
	tree.Provider = cmp.Or(*providerName, os.Getenv("SVEN_PROVIDER"), tree.Provider)
	tree.Model = cmp.Or(*model, os.Getenv("SVEN_MODEL"), tree.Model)
	if _, err := tree.For("."); err != nil {
		return false, err
	}
	raw, err := read(ctx, stdin, *patch, *all, *commit, diffArgs)
	if err != nil {
		return false, err
	}
	files, err := diff.Parse(bytes.NewReader(raw))
	if err != nil {
		return false, fmt.Errorf("parsing diff: %w", err)
	}
	var targets []bouncer.Target
	diffs := map[string]diff.File{}
	for _, f := range files {
		diffs[f.Path] = f
		c, err := tree.For(path.Dir(f.Path))
		if err != nil {
			return false, err
		}
		if len(c.Rules) > 0 && !c.Excluded(f.Path) {
			targets = append(targets, bouncer.Target{File: f, Rules: c.Rules, Advice: c.Advice})
		}
	}
	if unknown := tree.Unknown(); len(unknown) > 0 {
		return false, fmt.Errorf("no rule named %s; the rules are %s", strings.Join(unknown, ", "), strings.Join(tree.Known(), ", "))
	}
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "sven: nothing to check: "+emptyBecause(*patch, *all, *commit, diffArgs, len(files)))
		return false, nil
	}

	if *lines && tree.Provider == config.Sven {
		return false, errors.New("--lines asks questions the free sven API doesn't answer; use your own key with --provider typesafe and TYPESAFE_API_KEY")
	}
	client, err := provider.New(tree.Provider, tree.Model, tree.AllowRequestStorage)
	if err != nil {
		return false, err
	}
	prog := startProgress(stderr, colors(stderr, args), len(targets), func(u systemone.Usage) string {
		return price(client, u)
	})
	var inFlight func(int)
	if prog != nil {
		inFlight = prog.requests
	}
	// The cache answers first, so only real requests wait for a slot.
	evaluator, err := cache.Open(filepath.Join(root, cacheDir), client.Endpoint()+" "+client.Model(),
		bouncer.Limit(client, *parallel, inFlight))
	if err != nil {
		prog.finish()
		return false, err
	}
	// Answers are saved even when the check is interrupted, so the next run
	// picks up where this one stopped. Failing to save only costs requests.
	defer func() {
		if err := evaluator.Close(); err != nil {
			slog.Warn("sven: saving answers", "err", err)
		}
	}()
	b := bouncer.Bouncer{Evaluator: evaluator, ChunkBytes: chunkBytes, Lines: *lines}
	if prog != nil {
		b.Judged = prog.judged
	}
	report, err := b.Check(ctx, targets)
	prog.finish()
	if err != nil && ctx.Err() == nil {
		return false, err
	}
	// With --all, each file is judged whole, so a diffstat says nothing.
	printVerdicts(stdout, report, diffs, !*all, *verbose, p)
	if err != nil {
		fmt.Fprintln(stdout, p.yellow(fmt.Sprintf("sven: interrupted after judging %d of %d files. Run again to pick up where it stopped.", report.Files, len(targets))))
	} else {
		printVerdict(stdout, report, p)
	}
	if err == nil || report.Usage.InputTokens > 0 {
		fmt.Fprintln(stdout, p.dim("sven: "+report.Usage.Summary(client.Name())))
	}
	if err != nil {
		return false, errInterrupted
	}
	return report.Rejected(), nil
}

// read returns the diff to judge: standard input with --patch, or what git
// diff shows for diffArgs, from the empty tree with --all, or across one
// commit with --commit.
func read(ctx context.Context, stdin io.Reader, patch, all bool, commit string, diffArgs []string) ([]byte, error) {
	var revs []string
	switch {
	case patch:
		return io.ReadAll(stdin)
	case all:
		empty, err := git.EmptyTree(ctx)
		if err != nil {
			return nil, err
		}
		revs = []string{empty}
	case commit != "":
		parent, id, err := git.Commit(ctx, commit)
		if err != nil {
			return nil, err
		}
		revs = []string{parent, id}
	}
	return git.Diff(ctx, append(revs, diffArgs...)...)
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
func emptyBecause(patch, all bool, commit string, diffArgs []string, files int) string {
	opts, paths := diffArgs, []string(nil)
	if i := slices.Index(diffArgs, "--"); i >= 0 {
		opts, paths = diffArgs[:i], diffArgs[i+1:]
	}
	switch {
	case files > 0:
		return "every changed file is excluded or has no rules."
	case patch:
		return "the patch changes no files."
	case all:
		return "no tracked files."
	case commit != "" && len(paths) > 0:
		return "commit " + commit + " changes nothing in " + strings.Join(paths, " ") + "."
	case commit != "":
		return "commit " + commit + " changes no files."
	case slices.Contains(opts, "--cached") || slices.Contains(opts, "--staged"):
		return "no staged changes."
	case len(opts) == 0:
		return "no unstaged changes. For staged ones, use sven check --cached; new files need git add first."
	}
	return "git diff " + strings.Join(diffArgs, " ") + " shows no changes."
}

// splitArgs separates sven's own flags, those defined in fs, from the
// arguments it passes on to git diff.
func splitArgs(fs *flag.FlagSet, args []string) (own, diffArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return own, append(diffArgs, args[i:]...)
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if !strings.HasPrefix(a, "-") || (f == nil && name != "h" && name != "help") {
			diffArgs = append(diffArgs, a)
			continue
		}
		own = append(own, a)
		if f != nil && !isBool(f) && !hasValue && i+1 < len(args) {
			i++
			own = append(own, args[i])
		}
	}
	return own, diffArgs
}

func isBool(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// ruleList collects rule ids from a repeatable, comma-separated flag.
type ruleList []string

func (l *ruleList) String() string { return strings.Join(*l, ",") }

func (l *ruleList) Set(v string) error {
	for id := range strings.SplitSeq(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			*l = append(*l, id)
		}
	}
	return nil
}

// errInterrupted ends a check stopped by Ctrl-C, after its partial report.
var errInterrupted = errors.New("interrupted")

// printVerdicts lists the violations in report, or every verdict if verbose,
// under each file's path and, if stat, its diffstat. Lines found behind a
// violation show in their place in the file's diff.
func printVerdicts(w io.Writer, report bouncer.Report, diffs map[string]diff.File, stat, verbose bool, p palette) {
	shown := report.Violations()
	if verbose {
		shown = report.Verdicts
	}
	paint := map[bouncer.Level]func(string) string{bouncer.OK: p.green, bouncer.Warn: p.yellow, bouncer.Error: p.red}
	path := ""
	for _, v := range shown {
		if v.Path != path {
			path = v.Path
			header := p.bold(path)
			if stat {
				added, removed := diffs[path].Stat()
				header += " " + p.green(fmt.Sprintf("+%d", added)) + " " + p.red(fmt.Sprintf("-%d", removed))
			}
			fmt.Fprintf(w, "  %s\n", header)
		}
		why := ""
		switch {
		case v.Level() != bouncer.OK:
			why = cmp.Or(v.Rule.Violation, v.Rule.Question)
		case v.Located:
			why = "no line is likely to break it"
		}
		verdict := paint[v.Level()](fmt.Sprintf("%s %-22s", marks[v.Level()], v.Rule.ID))
		line := fmt.Sprintf("    %s %3.0f%%  %s", verdict, v.P*100, why)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
		if len(v.Lines) > 0 {
			printLines(w, diffs[v.Path], v.Lines, p)
		}
	}
	if len(shown) > 0 {
		fmt.Fprintln(w)
	}
}

// contextLines is how many lines of the diff show around each line found.
const contextLines = 2

// printLines shows the lines found behind a violation in f's diff, with
// contextLines around each, marked with > and brighter than the rest. A
// dimmed ... stands for the lines skipped.
func printLines(w io.Writer, f diff.File, found []bouncer.Line, p palette) {
	isFound := func(number int, text string) bool {
		return slices.ContainsFunc(found, func(l bouncer.Line) bool { return l.Line == number && l.Text == text })
	}
	printed, skipped := false, false
	for _, h := range f.Hunks {
		numbers := h.Numbers()
		show := make([]bool, len(h.Lines))
		for i, l := range h.Lines {
			if isFound(numbers[i], l) {
				for k := max(0, i-contextLines); k <= min(len(h.Lines)-1, i+contextLines); k++ {
					show[k] = true
				}
			}
		}
		for i, l := range h.Lines {
			if !show[i] {
				skipped = true
				continue
			}
			if printed && skipped {
				fmt.Fprintln(w, "            "+p.dim("..."))
			}
			printed, skipped = true, false
			fmt.Fprintln(w, diffLine(numbers[i], l, isFound(numbers[i], l), p))
		}
		// The gap between hunks is skipped too.
		skipped = true
	}
}

// diffLine renders one line of a diff with its number, colored as git
// colors it, or brighter and marked if found.
func diffLine(number int, text string, found bool, p palette) string {
	n := "     "
	if number > 0 {
		n = fmt.Sprintf("%5d", number)
	}
	text = strings.TrimRight(strings.ReplaceAll(text, "\t", "    "), " ")
	paint, mark := p.dim, " "
	switch {
	case found && strings.HasPrefix(text, "+"):
		paint, mark = func(s string) string { return p.bold(p.brightGreen(s)) }, ">"
	case found:
		paint, mark = func(s string) string { return p.bold(p.brightRed(s)) }, ">"
	case strings.HasPrefix(text, "+"):
		paint = p.green
	case strings.HasPrefix(text, "-"):
		paint = p.red
	}
	return strings.TrimRight("      "+p.bold(mark)+" "+p.dim(n)+" "+paint(text), " ")
}

// printVerdict ends a complete check: turned away, let in with warnings, or
// let in.
func printVerdict(w io.Writer, report bouncer.Report, p palette) {
	switch {
	case report.Rejected():
		printTurnedAway(w, p)
	case len(report.Violations()) > 0:
		fmt.Fprintln(w, p.yellow("! sven: na jut, rin mit dir. aber benimm dich."))
	default:
		fmt.Fprintln(w, p.green("✓ sven: na logen. rin mit dir."))
	}
}

// printTurnedAway ends a check that found error-level violations.
func printTurnedAway(w io.Writer, p palette) {
	fmt.Fprintln(w, p.bold(p.red("✗ sven: heute leider nicht.")))
	fmt.Fprintln(w, p.dim("        (git commit --no-verify gets you in anyway)"))
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

	content := config.Starter(*allow)
	next := "the free sven API will check this project's changes"
	if !*allow {
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

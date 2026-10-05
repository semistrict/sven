// Command evals measures how well a config's rules and model judge a set of
// labeled diffs. It needs the same credentials as sven.
//
//	go run ./evals [-config file] [-cases dir] [-cache dir] [-v]
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
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/internal/cache"
	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/internal/diff"
	"github.com/semistrict/sven/internal/provider"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("evals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "config whose rules and model to evaluate (default: built-in)")
	casesDir := fs.String("cases", "evals/cases", "directory of labeled cases")
	verbose := fs.Bool("v", false, "list every probability, not just mistakes")
	cacheDir := fs.String("cache", filepath.Join(".sven", "cache"), "where to remember answers")
	catchall := fs.String("catchall", "sus", "comma-separated rules expected to fire on every case that breaks any rule")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := evaluate(ctx, *configPath, *casesDir, *cacheDir, strings.Split(*catchall, ","), *verbose, stdout); err != nil {
		fmt.Fprintf(stderr, "evals: %v\n", err)
		return 1
	}
	return 0
}

func evaluate(ctx context.Context, configPath, casesDir, cacheDir string, catchall []string, verbose bool, w io.Writer) error {
	tree, err := config.Load(".", configPath)
	if err != nil {
		return err
	}
	cfg, err := tree.For(".")
	if err != nil {
		return err
	}
	labels, err := config.BuiltinIDs()
	if err != nil {
		return err
	}
	cases, err := loadCases(casesDir, labels)
	if err != nil {
		return err
	}
	client, err := provider.New(cmp.Or(os.Getenv("SVEN_PROVIDER"), tree.Provider), cmp.Or(os.Getenv("SVEN_MODEL"), tree.Model), tree.AllowRequestStorage)
	if err != nil {
		return err
	}
	evaluator, err := cache.New(cacheDir, client.Endpoint()+" "+client.Model(), bouncer.Limit(client, 8, nil))
	if err != nil {
		return err
	}
	targets := make([]bouncer.Target, len(cases))
	for i, c := range cases {
		advice := cfg.Advice
		if c.Advice != "" {
			advice = strings.TrimSpace(advice + "\n\n" + c.Advice)
		}
		targets[i] = bouncer.Target{File: c.file, Rules: cfg.Rules, Advice: advice}
	}
	b := bouncer.Bouncer{Evaluator: evaluator, ChunkBytes: 1 << 20}
	report, err := b.Check(ctx, targets)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s with %d rules on %d cases\n", client.Model(), len(cfg.Rules), len(cases))
	fmt.Fprintf(w, "cost: %s\n\n", report.Usage.Summary(client.Name()))
	write(w, judge(cases, labels, catchall, report), verbose)
	return nil
}

// Case is one labeled diff: the rules it breaks, and that it keeps the rest.
type Case struct {
	Name   string   `yaml:"-"`
	Path   string   `yaml:"path"`
	Diff   string   `yaml:"diff"`
	Expect []string `yaml:"expect"`
	// Why explains a label that isn't obvious.
	Why string `yaml:"why"`
	// Advice is what the project's .sven.yaml tells the model.
	Advice string `yaml:"advice"`

	file diff.File
}

func loadCases(dir string, labels []string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no cases in %s", dir)
	}
	cases := make([]Case, len(paths))
	for i, path := range paths {
		if cases[i], err = loadCase(path, labels); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return cases, nil
}

func loadCase(path string, labels []string) (Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Case{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Case
	if err := dec.Decode(&c); err != nil {
		return Case{}, err
	}
	c.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
	for _, id := range c.Expect {
		if !slices.Contains(labels, id) {
			return Case{}, fmt.Errorf("expects unknown rule %q", id)
		}
	}
	header := fmt.Sprintf("diff --git a/%[1]s b/%[1]s\n--- a/%[1]s\n+++ b/%[1]s\n", c.Path)
	files, err := diff.Parse(strings.NewReader(header + c.Diff))
	if err != nil {
		return Case{}, err
	}
	if len(files) != 1 || files[0].Path != c.Path {
		return Case{}, fmt.Errorf("diff must be hunks of one file, starting with @@")
	}
	c.file = files[0]
	return c, nil
}

// Outcome is one case judged against one rule.
type Outcome struct {
	Case    Case
	Verdict bouncer.Verdict
	// Labeled is whether cases are labeled with the rule, and Expected
	// whether this case breaks it.
	Labeled, Expected bool
}

// judge pairs each verdict with its label. A catch-all rule is expected to
// fire on every case that breaks any rule. Report verdicts come ordered by
// target, then rule, and targets are the cases in order.
func judge(cases []Case, labels, catchall []string, report bouncer.Report) []Outcome {
	outcomes := make([]Outcome, len(report.Verdicts))
	perCase := len(report.Verdicts) / len(cases)
	for i, v := range report.Verdicts {
		c := cases[i/perCase]
		outcomes[i] = Outcome{
			Case:     c,
			Verdict:  v,
			Labeled:  slices.Contains(labels, v.Rule.ID),
			Expected: slices.Contains(c.Expect, v.Rule.ID) || (slices.Contains(catchall, v.Rule.ID) && len(c.Expect) > 0),
		}
	}
	return outcomes
}

// predict says whether an outcome counts as a violation.
type predict func(Outcome) bool

func configured(o Outcome) bool { return o.Verdict.Level() != bouncer.OK }

func at(threshold float64) predict {
	return func(o Outcome) bool { return o.Verdict.P >= threshold }
}

// tally is a confusion matrix.
type tally struct{ tp, fp, fn, tn int }

func (t *tally) add(predicted, expected bool) {
	switch {
	case predicted && expected:
		t.tp++
	case predicted:
		t.fp++
	case expected:
		t.fn++
	default:
		t.tn++
	}
}

func (t tally) precision() string { return ratio(t.tp, t.tp+t.fp) }
func (t tally) recall() string    { return ratio(t.tp, t.tp+t.fn) }

func ratio(n, d int) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", float64(n)/float64(d))
}

// byRule tallies labeled outcomes per rule, in rule order.
func byRule(outcomes []Outcome, p predict) ([]string, map[string]*tally) {
	var ids []string
	tallies := map[string]*tally{}
	for _, o := range outcomes {
		if !o.Labeled {
			continue
		}
		id := o.Verdict.Rule.ID
		if tallies[id] == nil {
			ids = append(ids, id)
			tallies[id] = &tally{}
		}
		tallies[id].add(p(o), o.Expected)
	}
	return ids, tallies
}

func (t *tally) merge(o tally) { t.tp, t.fp, t.fn, t.tn = t.tp+o.tp, t.fp+o.fp, t.fn+o.fn, t.tn+o.tn }

// caseResult is whether a case was flagged by any rule, and should have been.
type caseResult struct {
	Case               Case
	predicted, worstAt int
	expected           bool
}

// byCase judges whole cases: flagged if any rule is violated, expected to be
// if the case breaks any labeled rule.
func byCase(outcomes []Outcome, p predict) []caseResult {
	var results []caseResult
	for i, o := range outcomes {
		if len(results) == 0 || results[len(results)-1].Case.Name != o.Case.Name {
			results = append(results, caseResult{Case: o.Case, worstAt: i, expected: len(o.Case.Expect) > 0})
		}
		r := &results[len(results)-1]
		if p(o) {
			r.predicted++
		}
		if o.Verdict.P > outcomes[r.worstAt].Verdict.P {
			r.worstAt = i
		}
	}
	return results
}

func caseTally(results []caseResult) tally {
	var t tally
	for _, r := range results {
		t.add(r.predicted > 0, r.expected)
	}
	return t
}

var sweep = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}

const row = "%-22s %4d %4d %4d %4d %9s %6s\n"

func write(w io.Writer, outcomes []Outcome, verbose bool) {
	ids, tallies := byRule(outcomes, configured)
	fmt.Fprintf(w, "%-22s %4s %4s %4s %4s %9s %6s\n", "rule", "tp", "fp", "fn", "tn", "precision", "recall")
	var total tally
	for _, id := range ids {
		t := *tallies[id]
		total.merge(t)
		fmt.Fprintf(w, row, id, t.tp, t.fp, t.fn, t.tn, t.precision(), t.recall())
	}
	fmt.Fprintf(w, row, "all labeled rules", total.tp, total.fp, total.fn, total.tn, total.precision(), total.recall())
	cases := byCase(outcomes, configured)
	c := caseTally(cases)
	fmt.Fprintf(w, row, "any rule, per case", c.tp, c.fp, c.fn, c.tn, c.precision(), c.recall())

	fmt.Fprintf(w, "\n%-9s %16s %16s\n", "", "labeled rules", "cases")
	fmt.Fprintf(w, "%-9s %9s %6s %9s %6s\n", "threshold", "precision", "recall", "precision", "recall")
	for _, threshold := range sweep {
		_, tallies := byRule(outcomes, at(threshold))
		var r tally
		for _, t := range tallies {
			r.merge(*t)
		}
		c := caseTally(byCase(outcomes, at(threshold)))
		fmt.Fprintf(w, "%-9.1f %9s %6s %9s %6s\n", threshold, r.precision(), r.recall(), c.precision(), c.recall())
	}

	heading := onceHeading(w, "\nby rule")
	for _, o := range outcomes {
		correct := configured(o) == o.Expected
		if o.Labeled && (!correct || verbose) {
			heading()
			writeMistake(w, correct, o.Expected, o.Case, o.Verdict)
		}
	}
	heading = onceHeading(w, "\nby case, with its most probable rule")
	for _, r := range cases {
		correct := (r.predicted > 0) == r.expected
		if !correct || verbose {
			heading()
			writeMistake(w, correct, r.expected, r.Case, outcomes[r.worstAt].Verdict)
		}
	}
}

// onceHeading returns a function that prints title the first time it's
// called.
func onceHeading(w io.Writer, title string) func() {
	var once sync.Once
	return func() { once.Do(func() { fmt.Fprintln(w, title) }) }
}

func writeMistake(w io.Writer, correct, expected bool, c Case, v bouncer.Verdict) {
	mark := "ok"
	switch {
	case correct:
	case expected:
		mark = "missed"
	default:
		mark = "false alarm"
	}
	fmt.Fprintf(w, "%-11s %-30s %-22s %3.0f%%\n", mark, c.Name, v.Rule.ID, v.P*100)
	if !correct && c.Why != "" {
		fmt.Fprintf(w, "%11s %s\n", "", c.Why)
	}
}

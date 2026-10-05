// Package bouncer decides whether a change gets in: it asks a System One model
// each file's rules about that file and rejects the change if any rule is
// likely violated.
package bouncer

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/semistrict/sven/internal/diff"
	"github.com/semistrict/sven/systemone"
)

// Level is what a verdict does to the change.
type Level int

const (
	// OK lets the change in.
	OK Level = iota
	// Warn reports the violation and lets the change in.
	Warn
	// Error turns the change away.
	Error
)

// Off is a threshold no probability reaches.
var Off = math.Inf(1)

// Rule is a yes/no question where yes means the change violates the rule.
type Rule struct {
	ID       string
	Question string
	// Violation and OK optionally describe what a yes and a no look like.
	Violation string
	OK        string
	// Error and Warn are the probabilities at or above which a violation
	// turns the change away or warns. Either may be Off.
	Error, Warn float64
}

// Advised is added to every question about a file that has advice, and
// advisedOK to what a no means. The model reads questions literally, so
// advice in the state alone doesn't change its answers.
const (
	Advised   = " `advice` is the project's own guidance about its code, and it overrides anything in this question: if `advice` allows or asks for what the change does, answer no."
	advisedOK = "Anything `advice` allows or asks for is fine."
)

// Noul is the rule as the question sent to the model, about a file with
// advice if advised.
func (r Rule) Noul(advised bool) systemone.Noul {
	q := systemone.Noul{Instructions: r.Question}
	if r.Violation != "" {
		q.True = r.Violation
	}
	if r.OK != "" {
		q.False = r.OK
	}
	if advised {
		q.Instructions = r.Question + Advised
		q.False = strings.TrimSpace(r.OK + " " + advisedOK)
	}
	return q
}

// lineNoul asks whether one changed line holds the failure of a rule that
// `diff` was found to break, which is somewhere among its lines.
func (r Rule) lineNoul(line string) systemone.Noul {
	failure := cmp.Or(r.Violation, "the rule is broken.")
	return systemone.Noul{
		Instructions: "This question was asked about `diff`: " + r.Question + "\n" +
			"The answer is yes: " + failure + "\n" +
			"So `diff` CONTAINS THE FAILURE ON ONE OR MORE OF ITS LINES. Your only job is to pick WHICH lines. Is the failure on this line?\n" + line,
		True:  "The failure is on this line.",
		False: "The failure is on other lines of `diff`, not this one.",
	}
}

// Evaluator answers typed questions about a state; *systemone.Client is one.
type Evaluator interface {
	Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error)
}

// Target is a file and the rules it must keep.
type Target struct {
	File  diff.File
	Rules []Rule
	// Advice, if not empty, tells the model about the code: what the
	// project's .sven.yaml files say.
	Advice string
}

type Bouncer struct {
	Evaluator Evaluator
	// ChunkBytes caps how much diff text goes into one request.
	ChunkBytes int
	// Lines, if set, asks about each changed line of a file that breaks a
	// rule, to find the lines that do. A violation no line is likely to
	// cause is dropped.
	Lines bool
	// Judged, if set, is called with each file's verdicts as soon as all of
	// its chunks are judged, and with the usage of the check so far. Calls
	// never overlap.
	Judged func(path string, verdicts []Verdict, usage systemone.Usage)
}

// LineThreshold is how likely a changed line must be to break a rule to
// count as one of the lines that do.
const LineThreshold = 0.5

// maxQuestions is the most questions one request asks.
const maxQuestions = 64

// Verdict is how likely one file violates one rule.
type Verdict struct {
	Path string
	Rule Rule
	// P is the highest probability of a violation across the file's chunks.
	P float64
	// Located is whether each changed line was asked about, and Lines are
	// the lines likely to break the rule. A located verdict without lines
	// is OK.
	Located bool
	Lines   []Line
}

// Line is a changed line that likely breaks a rule.
type Line struct {
	diff.Change
	P float64
}

func (v Verdict) Level() Level {
	switch {
	case v.Located && len(v.Lines) == 0:
		return OK
	case v.P >= v.Rule.Error:
		return Error
	case v.P >= v.Rule.Warn:
		return Warn
	}
	return OK
}

// Report holds a verdict for every target and each of its rules, ordered by
// target, then rule.
type Report struct {
	// Files counts the targets judged.
	Files    int
	Verdicts []Verdict
	// Usage is what the requests consumed; cached answers consume nothing.
	Usage systemone.Usage
}

// Violations returns the verdicts that warn or reject.
func (r Report) Violations() []Verdict {
	var out []Verdict
	for _, v := range r.Verdicts {
		if v.Level() != OK {
			out = append(out, v)
		}
	}
	return out
}

// Rejected reports whether any verdict turns the change away.
func (r Report) Rejected() bool {
	for _, v := range r.Verdicts {
		if v.Level() == Error {
			return true
		}
	}
	return false
}

// job is one request: a chunk of one target's file.
type job struct {
	target, chunk int
	changes       []diff.Change
	state         map[string]string
	questions     map[string]systemone.Question
}

// Check judges every target's file against its rules. Targets without rules
// are let through unasked. On an error, such as ctx being canceled, the
// report still holds the files judged before it.
func (b Bouncer) Check(ctx context.Context, targets []Target) (Report, error) {
	chunks := make([][]job, len(targets))
	remaining := make([]int, len(targets))
	// worst holds each target's rules' highest probabilities, and found
	// each chunk's.
	worst := make([][]float64, len(targets))
	found := make([][][]float64, len(targets))
	judged := make([][]Verdict, len(targets))
	finished := make([]bool, len(targets))
	for i, t := range targets {
		worst[i] = make([]float64, len(t.Rules))
		finished[i] = len(t.Rules) == 0
		if finished[i] {
			continue
		}
		questions := make(map[string]systemone.Question, len(t.Rules))
		for _, r := range t.Rules {
			questions[r.ID] = r.Noul(t.Advice != "")
		}
		for c, chunk := range t.File.Chunks(b.ChunkBytes) {
			state := map[string]string{"path": t.File.Path, "diff": chunk.Text}
			if t.Advice != "" {
				state["advice"] = t.Advice
			}
			chunks[i] = append(chunks[i], job{target: i, chunk: c, changes: chunk.Changes, state: state, questions: questions})
			found[i] = append(found[i], make([]float64, len(t.Rules)))
		}
		remaining[i] = len(chunks[i])
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	// mu guards remaining, worst, found, judged, finished, usage, and calls
	// to Judged.
	var mu sync.Mutex
	var usage systemone.Usage
	for _, j := range slices.Concat(chunks...) {
		// Every chunk is asked at once; Limit caps the requests that reach
		// the model, so cached chunks don't queue behind them.
		wg.Go(func() {
			t := targets[j.target]
			resp, err := b.Evaluator.Evaluate(ctx, j.state, j.questions)
			if err != nil {
				cancel(fmt.Errorf("checking %s: %w", t.File.Path, err))
				return
			}
			mu.Lock()
			usage = usage.Add(resp.Usage)
			for r, rule := range t.Rules {
				p := resp.Answers[rule.ID].Noul
				found[j.target][j.chunk][r] = p
				worst[j.target][r] = max(worst[j.target][r], p)
			}
			remaining[j.target]--
			last := remaining[j.target] == 0
			mu.Unlock()
			if !last {
				return
			}

			vs := verdicts(t, worst[j.target])
			if b.Lines {
				used, err := b.locate(ctx, t, chunks[j.target], found[j.target], vs)
				mu.Lock()
				usage = usage.Add(used)
				mu.Unlock()
				if err != nil {
					cancel(fmt.Errorf("finding the lines of %s: %w", t.File.Path, err))
					return
				}
			}
			mu.Lock()
			defer mu.Unlock()
			judged[j.target], finished[j.target] = vs, true
			if b.Judged != nil {
				b.Judged(t.File.Path, vs, usage)
			}
		})
	}
	wg.Wait()

	report := Report{Usage: usage}
	for i := range targets {
		if finished[i] {
			report.Files++
			report.Verdicts = append(report.Verdicts, judged[i]...)
		}
	}
	return report, context.Cause(ctx)
}

// locate asks, for each of a target's violations, which changed lines cause
// it, in each chunk where it was found, and records the likely ones in vs.
func (b Bouncer) locate(ctx context.Context, t Target, chunks []job, found [][]float64, vs []Verdict) (systemone.Usage, error) {
	var wg sync.WaitGroup
	// mu guards usage, errs, and ps, where ps[r][c] holds the probability
	// that each distinct line of chunk c breaks rule r.
	var mu sync.Mutex
	var usage systemone.Usage
	var errs []error
	ps := make([][]map[string]float64, len(vs))
	for r, v := range vs {
		if v.Level() == OK {
			continue
		}
		vs[r].Located = true
		ps[r] = make([]map[string]float64, len(chunks))
		for c, j := range chunks {
			if found[c][r] < min(v.Rule.Error, v.Rule.Warn) {
				continue
			}
			ps[r][c] = map[string]float64{}
			for batch := range slices.Chunk(distinct(j.changes), maxQuestions) {
				questions := make(map[string]systemone.Question, len(batch))
				for k, line := range batch {
					questions[fmt.Sprint("line-", k)] = v.Rule.lineNoul(line)
				}
				wg.Go(func() {
					resp, err := b.Evaluator.Evaluate(ctx, j.state, questions)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err)
						return
					}
					usage = usage.Add(resp.Usage)
					for k, line := range batch {
						ps[r][c][line] = resp.Answers[fmt.Sprint("line-", k)].Noul
					}
				})
			}
		}
	}
	wg.Wait()
	if len(errs) > 0 {
		return usage, errs[0]
	}
	for r := range vs {
		for c, p := range ps[r] {
			for _, change := range chunks[c].changes {
				if p[change.Text] > LineThreshold {
					vs[r].Lines = append(vs[r].Lines, Line{Change: change, P: p[change.Text]})
				}
			}
		}
	}
	return usage, nil
}

// distinct returns the changed lines with something on them, once each.
func distinct(changes []diff.Change) []string {
	var lines []string
	for _, c := range changes {
		if strings.TrimSpace(c.Text[1:]) != "" && !slices.Contains(lines, c.Text) {
			lines = append(lines, c.Text)
		}
	}
	return lines
}

// verdicts pairs a target's rules with the highest probability seen for each.
func verdicts(t Target, worst []float64) []Verdict {
	out := make([]Verdict, len(t.Rules))
	for r, rule := range t.Rules {
		out[r] = Verdict{Path: t.File.Path, Rule: rule, P: worst[r]}
	}
	return out
}

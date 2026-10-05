// Package bouncer decides whether a change gets in: it asks a System One model
// each file's rules about that file and rejects the change if any rule is
// likely violated.
package bouncer

import (
	"context"
	"fmt"
	"math"
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

// Noul is the rule as the question sent to the model.
func (r Rule) Noul() systemone.Question {
	q := systemone.Noul{Instructions: r.Question}
	if r.Violation != "" {
		q.True = r.Violation
	}
	if r.OK != "" {
		q.False = r.OK
	}
	return q
}

// Evaluator answers typed questions about a state; *systemone.Client is one.
type Evaluator interface {
	Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error)
}

// Target is a file and the rules it must keep.
type Target struct {
	File  diff.File
	Rules []Rule
}

type Bouncer struct {
	Evaluator Evaluator
	// ChunkBytes caps how much diff text goes into one request.
	ChunkBytes int
	// Judged, if set, is called with each file's verdicts as soon as all of
	// its chunks are judged, and with the usage of the check so far. Calls
	// never overlap.
	Judged func(path string, verdicts []Verdict, usage systemone.Usage)
}

// Verdict is how likely one file violates one rule.
type Verdict struct {
	Path string
	Rule Rule
	// P is the highest probability of a violation across the file's chunks.
	P float64
}

func (v Verdict) Level() Level {
	switch {
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
	target    int
	state     map[string]string
	questions map[string]systemone.Question
}

// Check judges every target's file against its rules. Targets without rules
// are let through unasked. On an error, such as ctx being canceled, the
// report still holds the files judged before it.
func (b Bouncer) Check(ctx context.Context, targets []Target) (Report, error) {
	var jobs []job
	remaining := make([]int, len(targets))
	worst := make([][]float64, len(targets))
	for i, t := range targets {
		worst[i] = make([]float64, len(t.Rules))
		if len(t.Rules) == 0 {
			continue
		}
		questions := make(map[string]systemone.Question, len(t.Rules))
		for _, r := range t.Rules {
			questions[r.ID] = r.Noul()
		}
		for _, chunk := range t.File.Chunks(b.ChunkBytes) {
			jobs = append(jobs, job{
				target:    i,
				state:     map[string]string{"path": t.File.Path, "diff": chunk},
				questions: questions,
			})
			remaining[i]++
		}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	// mu guards worst, remaining, usage, and calls to Judged.
	var mu sync.Mutex
	var usage systemone.Usage
	for _, j := range jobs {
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
			defer mu.Unlock()
			usage = usage.Add(resp.Usage)
			for r, rule := range t.Rules {
				worst[j.target][r] = max(worst[j.target][r], resp.Answers[rule.ID].Noul)
			}
			remaining[j.target]--
			if remaining[j.target] == 0 && b.Judged != nil {
				b.Judged(t.File.Path, verdicts(t, worst[j.target]), usage)
			}
		})
	}
	wg.Wait()

	report := Report{Usage: usage}
	for i, t := range targets {
		if remaining[i] == 0 {
			report.Files++
			report.Verdicts = append(report.Verdicts, verdicts(t, worst[i])...)
		}
	}
	return report, context.Cause(ctx)
}

// verdicts pairs a target's rules with the highest probability seen for each.
func verdicts(t Target, worst []float64) []Verdict {
	out := make([]Verdict, len(t.Rules))
	for r, rule := range t.Rules {
		out[r] = Verdict{Path: t.File.Path, Rule: rule, P: worst[r]}
	}
	return out
}

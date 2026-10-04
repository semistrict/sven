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
	// Concurrency caps requests in flight.
	Concurrency int
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

// job is one request: a chunk of one target's file, and the probability it
// violates each of the target's rules.
type job struct {
	target    int
	state     map[string]string
	questions map[string]systemone.Question
	p         []float64
}

// Check judges every target's file against its rules. Targets without rules
// are let through unasked.
func (b Bouncer) Check(ctx context.Context, targets []Target) (Report, error) {
	var jobs []job
	for i, t := range targets {
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
				p:         make([]float64, len(t.Rules)),
			})
		}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, b.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var usage systemone.Usage
	for i := range jobs {
		j := &jobs[i]
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			t := targets[j.target]
			resp, err := b.Evaluator.Evaluate(ctx, j.state, j.questions)
			if err != nil {
				cancel(fmt.Errorf("checking %s: %w", t.File.Path, err))
				return
			}
			for r, rule := range t.Rules {
				j.p[r] = resp.Answers[rule.ID].Noul
			}
			mu.Lock()
			usage = usage.Add(resp.Usage)
			mu.Unlock()
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return Report{}, err
	}

	worst := make([][]float64, len(targets))
	for i, t := range targets {
		worst[i] = make([]float64, len(t.Rules))
	}
	for _, j := range jobs {
		for r, p := range j.p {
			worst[j.target][r] = max(worst[j.target][r], p)
		}
	}
	report := Report{Usage: usage}
	for i, t := range targets {
		for r, rule := range t.Rules {
			report.Verdicts = append(report.Verdicts, Verdict{Path: t.File.Path, Rule: rule, P: worst[i][r]})
		}
	}
	return report, nil
}

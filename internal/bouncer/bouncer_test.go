package bouncer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/sven/internal/diff"
	"github.com/semistrict/sven/systemone"
)

// judge answers every noul with p(state, question id), recording each request.
type judge struct {
	p        func(state map[string]string, id string) float64
	mu       sync.Mutex
	requests []request
	err      error
}

type request struct {
	state map[string]string
	ids   []string
}

func (j *judge) Evaluate(_ context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	s := state.(map[string]string)
	var ids []string
	for id := range questions {
		ids = append(ids, id)
	}
	j.mu.Lock()
	j.requests = append(j.requests, request{s, ids})
	j.mu.Unlock()
	if j.err != nil {
		return nil, j.err
	}
	resp := &systemone.Response{Answers: map[string]systemone.Answer{}}
	for id := range questions {
		resp.Answers[id] = systemone.Answer{Type: systemone.KindNoul, Noul: j.p(s, id)}
	}
	return resp, nil
}

var (
	debug = Rule{ID: "debug", Question: "Does `diff` add debug prints?", Error: 0.7, Warn: Off}
	todo  = Rule{ID: "todo", Question: "Does `diff` add TODOs?", Error: 0.9, Warn: Off}
	rules = []Rule{debug, todo}
)

func target(path string, rules []Rule, lines ...string) Target {
	return Target{File: diff.File{Path: path, Hunks: []diff.Hunk{{Header: "@@ -1 +1 @@", Lines: lines}}}, Rules: rules}
}

func contains(word string, p float64) func(map[string]string, string) float64 {
	return func(s map[string]string, _ string) float64 {
		if strings.Contains(s["diff"], word) {
			return p
		}
		return 0.1
	}
}

func TestCheck(t *testing.T) {
	j := &judge{p: func(s map[string]string, id string) float64 {
		switch {
		case id == "debug" && strings.Contains(s["diff"], "println"):
			return 0.95
		case id == "todo" && strings.Contains(s["diff"], "TODO"):
			return 0.8
		}
		return 0.05
	}}
	b := Bouncer{Evaluator: j, ChunkBytes: 1000, Concurrency: 2}

	report, err := b.Check(t.Context(), []Target{
		target("a.go", rules, "+println(x)", "+// TODO"),
		target("removed.go", rules, "-println(x)"),
		target("b.go", rules, "+fine()"),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []Verdict{
		{Path: "a.go", Rule: debug, P: 0.95},
		{Path: "a.go", Rule: todo, P: 0.8},
		{Path: "removed.go", Rule: debug, P: 0.95},
		{Path: "removed.go", Rule: todo, P: 0.05},
		{Path: "b.go", Rule: debug, P: 0.05},
		{Path: "b.go", Rule: todo, P: 0.05},
	}
	if !reflect.DeepEqual(report.Verdicts, want) {
		t.Errorf("Verdicts =\n%+v\nwant\n%+v", report.Verdicts, want)
	}
	if got, want := report.Violations(), []Verdict{want[0], want[2]}; !reflect.DeepEqual(got, want) {
		t.Errorf("Violations = %+v, want %+v", got, want)
	}
	if !report.Rejected() {
		t.Error("Rejected = false, want true")
	}
	if len(j.requests) != 3 {
		t.Errorf("sent %d requests, want one per file", len(j.requests))
	}
}

func TestCheckAsksEachFileItsOwnRules(t *testing.T) {
	j := &judge{p: contains("x", 0)}
	b := Bouncer{Evaluator: j, ChunkBytes: 1000, Concurrency: 1}

	report, err := b.Check(t.Context(), []Target{
		target("dir/a.go", []Rule{debug}, "+x"),
		target("docs/b.md", nil, "+x"),
		target("c.go", []Rule{todo}, "+x"),
	})
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(j.requests, func(a, b request) int { return strings.Compare(a.state["path"], b.state["path"]) })
	want := []request{
		{map[string]string{"path": "c.go", "diff": "@@ -1 +1 @@\n+x\n"}, []string{"todo"}},
		{map[string]string{"path": "dir/a.go", "diff": "@@ -1 +1 @@\n+x\n"}, []string{"debug"}},
	}
	if !reflect.DeepEqual(j.requests, want) {
		t.Errorf("requests = %+v, want %+v", j.requests, want)
	}
	if paths := []string{report.Verdicts[0].Path, report.Verdicts[1].Path}; len(report.Verdicts) != 2 || paths[0] != "dir/a.go" || paths[1] != "c.go" {
		t.Errorf("Verdicts = %+v", report.Verdicts)
	}
}

func TestCheckTakesWorstChunk(t *testing.T) {
	j := &judge{p: contains("println", 0.99)}
	b := Bouncer{Evaluator: j, ChunkBytes: 30, Concurrency: 4}
	big := Target{File: diff.File{Path: "big.go", Hunks: []diff.Hunk{
		{Header: "@@ -1 +1 @@", Lines: []string{"+fine()", "+fine()"}},
		{Header: "@@ -9 +9 @@", Lines: []string{"+println(x)"}},
	}}, Rules: []Rule{debug}}

	report, err := b.Check(t.Context(), []Target{big})
	if err != nil {
		t.Fatal(err)
	}

	if len(j.requests) != 2 {
		t.Errorf("sent %d requests, want 2 chunks", len(j.requests))
	}
	want := []Verdict{{Path: "big.go", Rule: debug, P: 0.99}}
	if !reflect.DeepEqual(report.Verdicts, want) {
		t.Errorf("Verdicts = %+v, want %+v", report.Verdicts, want)
	}
}

func TestCheckError(t *testing.T) {
	j := &judge{err: errors.New("boom")}
	b := Bouncer{Evaluator: j, ChunkBytes: 1000, Concurrency: 1}

	_, err := b.Check(t.Context(), []Target{target("a.go", rules, "+x")})

	if err == nil || err.Error() != "checking a.go: boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestLevels(t *testing.T) {
	sus := Rule{ID: "sus", Question: "Is `diff` sus?", Error: 0.95, Warn: 0.7}
	warnOnly := Rule{ID: "skip", Question: "Does `diff` skip tests?", Error: Off, Warn: 0.5}
	for _, tc := range []struct {
		rule Rule
		p    float64
		want Level
	}{
		{sus, 0.96, Error},
		{sus, 0.95, Error},
		{sus, 0.9, Warn},
		{sus, 0.7, Warn},
		{sus, 0.69, OK},
		{warnOnly, 1, Warn},
		{warnOnly, 0.4, OK},
	} {
		if got := (Verdict{Rule: tc.rule, P: tc.p}).Level(); got != tc.want {
			t.Errorf("%s at %v: Level = %v, want %v", tc.rule.ID, tc.p, got, tc.want)
		}
	}
}

func TestWarningsDoNotReject(t *testing.T) {
	warnDebug := Rule{ID: "debug", Question: "Does `diff` add debug prints?", Error: Off, Warn: 0.5}
	j := &judge{p: contains("println", 0.95)}
	b := Bouncer{Evaluator: j, ChunkBytes: 1000, Concurrency: 1}

	report, err := b.Check(t.Context(), []Target{target("a.go", []Rule{warnDebug}, "+println(x)")})
	if err != nil {
		t.Fatal(err)
	}

	want := []Verdict{{Path: "a.go", Rule: warnDebug, P: 0.95}}
	if got := report.Violations(); !reflect.DeepEqual(got, want) {
		t.Errorf("Violations = %+v, want %+v", got, want)
	}
	if report.Rejected() {
		t.Error("Rejected = true, want false")
	}
}

func TestUsageAddsUp(t *testing.T) {
	j := &judge{p: contains("x", 0)}
	b := Bouncer{Evaluator: usageOf{j, systemone.Usage{InputTokens: 300, OutputTokens: 2}}, ChunkBytes: 1000, Concurrency: 4}

	report, err := b.Check(t.Context(), []Target{target("a.go", rules, "+x"), target("b.go", rules, "+y")})
	if err != nil {
		t.Fatal(err)
	}
	if want := (systemone.Usage{InputTokens: 600, OutputTokens: 4}); report.Usage != want {
		t.Errorf("Usage = %+v, want %+v", report.Usage, want)
	}
}

// usageOf reports usage on every response.
type usageOf struct {
	next  Evaluator
	usage systemone.Usage
}

func (u usageOf) Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	resp, err := u.next.Evaluate(ctx, state, questions)
	if err != nil {
		return nil, err
	}
	resp.Usage = u.usage
	return resp, nil
}

func TestJudgedOncePerFileAfterAllItsChunks(t *testing.T) {
	j := &judge{p: contains("println", 0.99)}
	var judged []Verdict
	calls := map[string]int{}
	b := Bouncer{Evaluator: j, ChunkBytes: 30, Concurrency: 4, Judged: func(path string, vs []Verdict, _ systemone.Usage) {
		calls[path]++
		judged = append(judged, vs...)
	}}
	big := Target{File: diff.File{Path: "big.go", Hunks: []diff.Hunk{
		{Header: "@@ -1 +1 @@", Lines: []string{"+fine()", "+fine()"}},
		{Header: "@@ -9 +9 @@", Lines: []string{"+println(x)"}},
	}}, Rules: []Rule{debug}}

	report, err := b.Check(t.Context(), []Target{big, target("small.go", []Rule{debug}, "+fine()")})
	if err != nil {
		t.Fatal(err)
	}

	if want := map[string]int{"big.go": 1, "small.go": 1}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	slices.SortFunc(judged, func(a, b Verdict) int { return strings.Compare(a.Path, b.Path) })
	if !reflect.DeepEqual(judged, report.Verdicts) {
		t.Errorf("judged = %+v, want the report's %+v", judged, report.Verdicts)
	}
	if judged[0].P != 0.99 {
		t.Errorf("big.go P = %v, want the worst chunk's 0.99", judged[0].P)
	}
}

// interrupter answers a.go, then cancels the check while b.go is in flight.
type interrupter struct {
	cancel   context.CancelFunc
	answered chan struct{}
}

func (i interrupter) Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	if state.(map[string]string)["path"] == "a.go" {
		defer close(i.answered)
		return &systemone.Response{
			Answers: map[string]systemone.Answer{"debug": {Type: systemone.KindNoul, Noul: 0.95}},
			Usage:   systemone.Usage{InputTokens: 100},
		}, nil
	}
	<-i.answered
	i.cancel()
	return nil, ctx.Err()
}

func TestInterruptedCheckKeepsJudgedFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	b := Bouncer{Evaluator: interrupter{cancel, make(chan struct{})}, ChunkBytes: 1000, Concurrency: 2}

	report, err := b.Check(ctx, []Target{target("a.go", []Rule{debug}, "+x"), target("b.go", []Rule{debug}, "+y")})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	want := Report{Files: 1, Verdicts: []Verdict{{Path: "a.go", Rule: debug, P: 0.95}}, Usage: systemone.Usage{InputTokens: 100}}
	if !reflect.DeepEqual(report, want) {
		t.Errorf("report = %+v, want %+v", report, want)
	}
}

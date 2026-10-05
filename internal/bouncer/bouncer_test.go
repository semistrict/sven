package bouncer

import (
	"context"
	"errors"
	"fmt"
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
	b := Bouncer{Evaluator: j, ChunkBytes: 1000}

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
	b := Bouncer{Evaluator: j, ChunkBytes: 1000}

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
	b := Bouncer{Evaluator: j, ChunkBytes: 30}
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
	b := Bouncer{Evaluator: j, ChunkBytes: 1000}

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
	b := Bouncer{Evaluator: j, ChunkBytes: 1000}

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
	b := Bouncer{Evaluator: usageOf{j, systemone.Usage{InputTokens: 300, OutputTokens: 2}}, ChunkBytes: 1000}

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
	b := Bouncer{Evaluator: j, ChunkBytes: 30, Judged: func(path string, vs []Verdict, _ systemone.Usage) {
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
	b := Bouncer{Evaluator: interrupter{cancel, make(chan struct{})}, ChunkBytes: 1000}

	report, err := b.Check(ctx, []Target{target("a.go", []Rule{debug}, "+x"), target("b.go", []Rule{debug}, "+y")})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	want := Report{Files: 1, Verdicts: []Verdict{{Path: "a.go", Rule: debug, P: 0.95}}, Usage: systemone.Usage{InputTokens: 100}}
	if !reflect.DeepEqual(report, want) {
		t.Errorf("report = %+v, want %+v", report, want)
	}
}

func TestLimitCapsRequestsInFlight(t *testing.T) {
	var mu sync.Mutex
	inFlight, most, starts := 0, 0, 0
	limited := Limit(&judge{p: contains("x", 0)}, 2, func(delta int) {
		mu.Lock()
		defer mu.Unlock()
		inFlight += delta
		most = max(most, inFlight)
		if delta > 0 {
			starts++
		}
	})
	b := Bouncer{Evaluator: limited, ChunkBytes: 1000}
	var targets []Target
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		targets = append(targets, target(name, rules, "+x"))
	}

	if _, err := b.Check(t.Context(), targets); err != nil {
		t.Fatal(err)
	}

	if inFlight != 0 || starts != 5 || most < 1 || most > 2 {
		t.Errorf("in flight at end = %d, starts = %d, most at once = %d; want 0, 5, and at most the limit of 2", inFlight, starts, most)
	}
}

func TestLimitGivesUpWaitingWhenCanceled(t *testing.T) {
	b := blocker{holding: make(chan struct{}), release: make(chan struct{})}
	defer close(b.release)
	limited := Limit(b, 1, nil)
	go limited.Evaluate(t.Context(), "first", nil)
	<-b.holding
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := limited.Evaluate(ctx, "second", nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// blocker says when a request holds its slot, and holds it until release
// closes.
type blocker struct{ holding, release chan struct{} }

func (b blocker) Evaluate(context.Context, any, map[string]systemone.Question) (*systemone.Response, error) {
	b.holding <- struct{}{}
	<-b.release
	return &systemone.Response{}, nil
}

// recorder answers no to everything and keeps the last request.
type recorder struct {
	state     any
	questions map[string]systemone.Question
}

func (r *recorder) Evaluate(_ context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	r.state, r.questions = state, questions
	return &systemone.Response{Answers: map[string]systemone.Answer{}}, nil
}

func TestAdviceGoesWithTheDiffAndOverridesTheQuestions(t *testing.T) {
	r := &recorder{}
	b := Bouncer{Evaluator: r, ChunkBytes: 1000}
	tgt := target("a.go", []Rule{{ID: "debug", Question: "Does `diff` add debug prints?", OK: "No prints."}}, "+x")
	tgt.Advice = "Prints are our output."

	if _, err := b.Check(t.Context(), []Target{tgt}); err != nil {
		t.Fatal(err)
	}

	wantState := map[string]string{"path": "a.go", "diff": "@@ -1 +1 @@\n+x\n", "advice": "Prints are our output."}
	if !reflect.DeepEqual(r.state, wantState) {
		t.Errorf("state = %v, want %v", r.state, wantState)
	}
	wantQuestions := map[string]systemone.Question{"debug": systemone.Noul{
		Instructions: "Does `diff` add debug prints? `advice` is the project's own guidance about its code, and it overrides anything in this question: if `advice` allows or asks for what the change does, answer no.",
		False:        "No prints. Anything `advice` allows or asks for is fine.",
	}}
	if !reflect.DeepEqual(r.questions, wantQuestions) {
		t.Errorf("questions = %#v, want %#v", r.questions, wantQuestions)
	}
}

func TestNoAdviceLeavesTheQuestionsAlone(t *testing.T) {
	r := &recorder{}
	b := Bouncer{Evaluator: r, ChunkBytes: 1000}

	if _, err := b.Check(t.Context(), []Target{target("a.go", []Rule{debug}, "+x")}); err != nil {
		t.Fatal(err)
	}

	if want := map[string]string{"path": "a.go", "diff": "@@ -1 +1 @@\n+x\n"}; !reflect.DeepEqual(r.state, want) {
		t.Errorf("state = %v, want %v", r.state, want)
	}
	if want := map[string]systemone.Question{"debug": systemone.Noul{Instructions: debug.Question}}; !reflect.DeepEqual(r.questions, want) {
		t.Errorf("questions = %#v, want %#v", r.questions, want)
	}
}

// lineJudge answers each rule question with rule[id], and each question
// about one line with line[that line], recording the line questions asked.
type lineJudge struct {
	rule map[string]float64
	line map[string]float64

	mu    sync.Mutex
	asked []string
}

func (j *lineJudge) Evaluate(_ context.Context, _ any, questions map[string]systemone.Question) (*systemone.Response, error) {
	resp := &systemone.Response{Answers: map[string]systemone.Answer{}}
	for id, q := range questions {
		p, ok := j.rule[id]
		if instructions := q.(systemone.Noul).Instructions.(string); strings.Contains(instructions, "CONTAINS THE FAILURE") {
			line := instructions[strings.LastIndex(instructions, "\n")+1:]
			j.mu.Lock()
			j.asked = append(j.asked, line)
			j.mu.Unlock()
			p, ok = j.line[line], true
		}
		if !ok {
			return nil, fmt.Errorf("unexpected question %s", id)
		}
		resp.Answers[id] = systemone.Answer{Type: systemone.KindNoul, Noul: p}
	}
	return resp, nil
}

func TestLinesFindTheLinesBehindAViolation(t *testing.T) {
	j := &lineJudge{
		rule: map[string]float64{"debug": 0.95, "todo": 0.1},
		line: map[string]float64{"+println(x)": 0.9, "+y := 2": 0.2, "-z := 3": 0.1},
	}
	b := Bouncer{Evaluator: j, ChunkBytes: 1000, Lines: true}
	tgt := Target{File: diff.File{Path: "a.go", Hunks: []diff.Hunk{
		{Header: "@@ -10,2 +10,4 @@", Lines: []string{" keep", "+println(x)", "+y := 2", "+", "-z := 3", "+println(x)"}},
	}}, Rules: rules}

	report, err := b.Check(t.Context(), []Target{tgt})
	if err != nil {
		t.Fatal(err)
	}

	want := []Verdict{
		{Path: "a.go", Rule: debug, P: 0.95, Located: true, Lines: []Line{
			{Change: diff.Change{Line: 11, Text: "+println(x)"}, P: 0.9},
			{Change: diff.Change{Line: 14, Text: "+println(x)"}, P: 0.9},
		}},
		{Path: "a.go", Rule: todo, P: 0.1},
	}
	if !reflect.DeepEqual(report.Verdicts, want) {
		t.Errorf("Verdicts =\n%+v\nwant\n%+v", report.Verdicts, want)
	}
	// Each distinct line with something on it is asked about once, and only
	// for the rule that fired.
	slices.Sort(j.asked)
	if want := []string{"+println(x)", "+y := 2", "-z := 3"}; !slices.Equal(j.asked, want) {
		t.Errorf("asked about %q, want %q", j.asked, want)
	}
	if !report.Rejected() {
		t.Error("Rejected = false, want true")
	}
}

func TestLinesDropAViolationNoLineCauses(t *testing.T) {
	j := &lineJudge{
		rule: map[string]float64{"debug": 0.95, "todo": 0.1},
		line: map[string]float64{"+fine()": 0.3},
	}
	b := Bouncer{Evaluator: j, ChunkBytes: 1000, Lines: true}

	report, err := b.Check(t.Context(), []Target{target("a.go", rules, "+fine()")})
	if err != nil {
		t.Fatal(err)
	}

	if got := report.Verdicts[0]; !got.Located || len(got.Lines) != 0 || got.Level() != OK {
		t.Errorf("debug verdict = %+v, level %v; want located, without lines, OK", got, got.Level())
	}
	if report.Rejected() {
		t.Error("Rejected = true, want false")
	}
}

func TestLinesAskOnlyInChunksWhereTheRuleFired(t *testing.T) {
	j := &lineJudge{line: map[string]float64{"+println(a)": 0.9}}
	b := Bouncer{Evaluator: &chunkJudge{j}, ChunkBytes: 30, Lines: true}
	tgt := Target{File: diff.File{Path: "a.go", Hunks: []diff.Hunk{
		{Header: "@@ -1 +1 @@", Lines: []string{"+println(a)"}},
		{Header: "@@ -9 +9 @@", Lines: []string{"+fine(b)"}},
	}}, Rules: []Rule{debug}}

	report, err := b.Check(t.Context(), []Target{tgt})
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"+println(a)"}; !slices.Equal(j.asked, want) {
		t.Errorf("asked about %q, want %q", j.asked, want)
	}
	if got := report.Verdicts[0].Lines; len(got) != 1 || got[0].Line != 1 {
		t.Errorf("Lines = %+v, want line 1", got)
	}
}

// chunkJudge answers rule questions by whether the chunk prints, and line
// questions with its lineJudge.
type chunkJudge struct{ *lineJudge }

func (c *chunkJudge) Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	p := 0.1
	if strings.Contains(state.(map[string]string)["diff"], "println") {
		p = 0.95
	}
	c.rule = map[string]float64{"debug": p}
	return c.lineJudge.Evaluate(ctx, state, questions)
}

package cache

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/semistrict/sven/systemone"
)

// counter answers every noul with 0.5 and records which ids it was asked.
type counter struct{ asked [][]string }

func (c *counter) Evaluate(_ context.Context, _ any, questions map[string]systemone.Question) (*systemone.Response, error) {
	resp := &systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{}}
	var ids []string
	for id := range questions {
		ids = append(ids, id)
		resp.Answers[id] = systemone.Answer{Type: systemone.KindNoul, Noul: 0.5}
	}
	slices.Sort(ids)
	c.asked = append(c.asked, ids)
	return resp, nil
}

var (
	state     = map[string]string{"path": "a.go", "diff": "+x"}
	questions = map[string]systemone.Question{
		"debug": systemone.Noul{Instructions: "Debug prints?"},
		"todo":  systemone.Noul{Instructions: "TODOs?"},
	}
)

func newCache(t *testing.T, dir, namespace string, next *counter) *Evaluator {
	t.Helper()
	e, err := New(dir, namespace, next)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func evaluate(t *testing.T, e *Evaluator, state any, questions map[string]systemone.Question) *systemone.Response {
	t.Helper()
	resp, err := e.Evaluate(t.Context(), state, questions)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRepeatedInputsAreAskedOnce(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}
	e := newCache(t, dir, "jev", next)

	first := evaluate(t, e, state, questions)
	second := evaluate(t, newCache(t, dir, "jev", next), state, questions)

	if want := [][]string{{"debug", "todo"}}; !reflect.DeepEqual(next.asked, want) {
		t.Errorf("asked = %v, want %v", next.asked, want)
	}
	if !reflect.DeepEqual(first.Answers, second.Answers) {
		t.Errorf("cached answers = %+v, want %+v", second.Answers, first.Answers)
	}
}

func TestOnlyNewQuestionsAreAsked(t *testing.T) {
	next := &counter{}
	e := newCache(t, t.TempDir(), "jev", next)

	evaluate(t, e, state, questions)
	changed := map[string]systemone.Question{
		"debug": questions["debug"],
		"todo":  systemone.Noul{Instructions: "TODOs left behind?"},
	}
	resp := evaluate(t, e, state, changed)

	if want := [][]string{{"debug", "todo"}, {"todo"}}; !reflect.DeepEqual(next.asked, want) {
		t.Errorf("asked = %v, want %v", next.asked, want)
	}
	if len(resp.Answers) != 2 {
		t.Errorf("answers = %+v, want 2", resp.Answers)
	}
}

func TestStateAndNamespaceAreKeys(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}

	evaluate(t, newCache(t, dir, "jev", next), state, questions)
	evaluate(t, newCache(t, dir, "jev", next), map[string]string{"path": "a.go", "diff": "+y"}, questions)
	evaluate(t, newCache(t, dir, "clef", next), state, questions)

	if len(next.asked) != 3 {
		t.Errorf("asked %d times, want 3", len(next.asked))
	}
}

func TestCorruptEntryIsAskedAgain(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}
	e := newCache(t, dir, "jev", next)
	evaluate(t, e, state, questions)
	path, err := e.path(state, questions["debug"])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := evaluate(t, e, state, questions)

	if want := [][]string{{"debug", "todo"}, {"debug"}}; !reflect.DeepEqual(next.asked, want) {
		t.Errorf("asked = %v, want %v", next.asked, want)
	}
	if resp.Answers["debug"].Noul != 0.5 {
		t.Errorf("debug = %+v", resp.Answers["debug"])
	}
}

func TestCacheIgnoresItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".sven", "cache")
	newCache(t, dir, "jev", &counter{})

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || string(got) != "*\n" {
		t.Errorf(".gitignore = %q, %v", got, err)
	}
}

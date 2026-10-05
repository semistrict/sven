package cache

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sven/systemone"
)

// counter answers every noul with 0.5 and records which ids it was asked.
type counter struct {
	mu    sync.Mutex
	asked [][]string
}

func (c *counter) Evaluate(_ context.Context, _ any, questions map[string]systemone.Question) (*systemone.Response, error) {
	resp := &systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{}}
	var ids []string
	for id := range questions {
		ids = append(ids, id)
		resp.Answers[id] = systemone.Answer{Type: systemone.KindNoul, Noul: 0.5}
	}
	slices.Sort(ids)
	c.mu.Lock()
	c.asked = append(c.asked, ids)
	c.mu.Unlock()
	return resp, nil
}

var (
	state     = map[string]string{"path": "a.go", "diff": "+x"}
	questions = map[string]systemone.Question{
		"debug": systemone.Noul{Instructions: "Debug prints?"},
		"todo":  systemone.Noul{Instructions: "TODOs?"},
	}
)

func open(t *testing.T, dir, namespace string, next *counter) *Evaluator {
	t.Helper()
	e, err := Open(dir, namespace, next)
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

// run is one run of sven: it opens the cache, asks, and closes it.
func run(t *testing.T, dir, namespace string, next *counter, state any, questions map[string]systemone.Question) *systemone.Response {
	t.Helper()
	e := open(t, dir, namespace, next)
	resp := evaluate(t, e, state, questions)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	return resp
}

func segments(t *testing.T, dir string) []string {
	t.Helper()
	names, err := segmentNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestRepeatedInputsAreAskedOnce(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}

	first := run(t, dir, "jev", next, state, questions)
	second := run(t, dir, "jev", next, state, questions)

	if want := [][]string{{"debug", "todo"}}; !reflect.DeepEqual(next.asked, want) {
		t.Errorf("asked = %v, want %v", next.asked, want)
	}
	want := map[string]systemone.Answer{
		"debug": {Type: systemone.KindNoul, Noul: 0.5},
		"todo":  {Type: systemone.KindNoul, Noul: 0.5},
	}
	if !reflect.DeepEqual(first.Answers, want) || !reflect.DeepEqual(second.Answers, want) {
		t.Errorf("answers = %+v, then %+v; want %+v", first.Answers, second.Answers, want)
	}
	if second.Usage != (systemone.Usage{}) || second.Model != "" {
		t.Errorf("cached response = %+v, want no model or usage", second)
	}
}

func TestAnswersAreReusedWithinARun(t *testing.T) {
	next := &counter{}
	e := open(t, t.TempDir(), "jev", next)

	evaluate(t, e, state, questions)
	evaluate(t, e, state, questions)

	if len(next.asked) != 1 {
		t.Errorf("asked %d times, want 1", len(next.asked))
	}
}

func TestOnlyNewQuestionsAreAsked(t *testing.T) {
	next := &counter{}
	e := open(t, t.TempDir(), "jev", next)

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

	run(t, dir, "jev", next, state, questions)
	run(t, dir, "jev", next, map[string]string{"path": "a.go", "diff": "+y"}, questions)
	run(t, dir, "clef", next, state, questions)

	if len(next.asked) != 3 {
		t.Errorf("asked %d times, want 3", len(next.asked))
	}
}

func TestEachRunWritesOneSegment(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}

	run(t, dir, "jev", next, state, questions)
	run(t, dir, "jev", next, map[string]string{"path": "b.go", "diff": "+y"}, questions)

	if got := segments(t, dir); len(got) != 2 {
		t.Errorf("segments = %q, want one per run", got)
	}
}

func TestBigRunsSaveAsTheyGo(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}
	many := map[string]systemone.Question{}
	for i := range flushEvery {
		many[fmt.Sprint("q", i)] = systemone.Noul{Instructions: fmt.Sprint("Question ", i, "?")}
	}

	// Never closed, as when sven is killed.
	evaluate(t, open(t, dir, "jev", next), state, many)
	evaluate(t, open(t, dir, "jev", next), state, many)

	if len(next.asked) != 1 {
		t.Errorf("asked %d times, want 1", len(next.asked))
	}
}

func TestManySegmentsAreMergedKeepingEveryAnswer(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}
	states := make([]map[string]string, maxSegments+1)
	for i := range states {
		states[i] = map[string]string{"path": fmt.Sprint(i, ".go"), "diff": "+x"}
		run(t, dir, "jev", next, states[i], questions)
	}

	if got := segments(t, dir); len(got) != 1 {
		t.Fatalf("segments = %q, want them merged into one", got)
	}
	e := open(t, dir, "jev", next)
	for _, s := range states {
		evaluate(t, e, s, questions)
	}
	if len(next.asked) != len(states) {
		t.Errorf("asked %d times, want %d: once per state", len(next.asked), len(states))
	}
}

func TestOverMaxBytesKeepsTheMostRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	// Room for 3 answers in half of maxBytes; the segments hold 304 bytes.
	maxBytes := 2 * (len(magic) + 3*recordSize)
	write := func(at time.Time, keys ...byte) {
		var entries []entry
		for _, b := range keys {
			entries = append(entries, entry{key{b}, float64(b) / 10})
		}
		if err := writeSegment(dir, segmentName(at), entries); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	write(start, 1, 2, 3)
	write(start.Add(time.Second), 4, 5)
	// 1 was used again most recently: it's in the newest segment too.
	write(start.Add(2*time.Second), 6, 1)

	if err := tidy(dir, maxBytes); err != nil {
		t.Fatal(err)
	}

	names := segments(t, dir)
	if len(names) != 1 {
		t.Fatalf("segments = %q, want one", names)
	}
	entries, err := readSegment(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	// Newest segment first: 1 and 6, then 4, the first of the next by key.
	want := []entry{{key{1}, 0.1}, {key{4}, 0.4}, {key{6}, 0.6}}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("kept %v, want %v", entries, want)
	}
}

func TestMergedSegmentIsOlderThanLaterRuns(t *testing.T) {
	dir := t.TempDir()
	for i := range maxSegments + 1 {
		if err := writeSegment(dir, segmentName(time.Unix(int64(i), 0)), []entry{{key{byte(i)}, 0.5}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tidy(dir, MaxBytes); err != nil {
		t.Fatal(err)
	}
	later := segmentName(time.Unix(int64(maxSegments+1), 0))

	if got := segments(t, dir); len(got) != 1 || got[0] > later {
		t.Errorf("segments = %q, want one sorting before %s", got, later)
	}
}

func TestCorruptSegmentIsIgnored(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}
	run(t, dir, "jev", next, state, questions)
	for _, name := range segments(t, dir) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("sven/a1\ntruncated"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp := run(t, dir, "jev", next, state, questions)

	if len(next.asked) != 2 {
		t.Errorf("asked %d times, want 2", len(next.asked))
	}
	if resp.Answers["debug"].Noul != 0.5 {
		t.Errorf("debug = %+v", resp.Answers["debug"])
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	if err := writeSegment(dir, "s"+suffix, []entry{{key{9}, 0.9}, {key{1}, 0.1}, {key{5}, 0.5}}); err != nil {
		t.Fatal(err)
	}
	s, err := openSegment(filepath.Join(dir, "s"+suffix))
	if err != nil {
		t.Fatal(err)
	}
	defer s.f.Close()

	for _, tc := range []struct {
		key key
		p   float64
		ok  bool
	}{{key{1}, 0.1, true}, {key{5}, 0.5, true}, {key{9}, 0.9, true}, {key{0}, 0, false}, {key{4}, 0, false}, {key{10}, 0, false}} {
		p, ok, err := s.find(tc.key)
		if err != nil || p != tc.p || ok != tc.ok {
			t.Errorf("find(%v) = %v, %v, %v; want %v, %v", tc.key[0], p, ok, err, tc.p, tc.ok)
		}
	}
}

func TestConvertsTheOldCache(t *testing.T) {
	dir := t.TempDir()
	next := &counter{}
	e := open(t, dir, "jev", next)
	k, err := e.key(state, questions["debug"])
	if err != nil {
		t.Fatal(err)
	}
	name := hex.EncodeToString(k[:])
	for f, content := range map[string]string{
		name[:2] + "/" + name + ".json":            `{"type":"noul","noul":0.25}`,
		"cd/" + strings.Repeat("cd", 32) + ".json": "{not json",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp := run(t, dir, "jev", next, state, questions)

	if want := [][]string{{"todo"}}; !reflect.DeepEqual(next.asked, want) {
		t.Errorf("asked = %v, want %v", next.asked, want)
	}
	if got := resp.Answers["debug"]; got.Noul != 0.25 {
		t.Errorf("debug = %+v, want the old cache's 0.25", got)
	}
	for _, d := range []string{name[:2], "cd"} {
		if _, err := os.Stat(filepath.Join(dir, d)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists: %v", d, err)
		}
	}
}

func TestRemovesAbandonedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"keep/me.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old, fresh := filepath.Join(dir, tmpPrefix+"old"), filepath.Join(dir, tmpPrefix+"fresh")
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-2 * abandonedAfter)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}

	run(t, dir, "jev", &counter{}, state, questions)

	for f, want := range map[string]bool{"keep/me.txt": true, tmpPrefix + "old": false, tmpPrefix + "fresh": true} {
		if _, err := os.Stat(filepath.Join(dir, f)); (err == nil) != want {
			t.Errorf("%s exists: %v, want %v", f, err == nil, want)
		}
	}
}

func TestCacheIgnoresItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".sven", "cache")
	open(t, dir, "jev", &counter{})

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || string(got) != "*\n" {
		t.Errorf(".gitignore = %q, %v", got, err)
	}
}

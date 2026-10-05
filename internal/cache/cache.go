// Package cache remembers System One answers on disk, so unchanged files
// aren't asked about again.
//
// Answers live in segments: files of fixed-size records sorted by key, each
// written once by one run and never changed. A run looks keys up newest
// segment first, and writes every answer it used, fresh or cached, to its
// own segment, so the newest segments always hold what was used most
// recently. When segments pile up or outgrow MaxBytes, they are merged into
// one, keeping the most recently used answers.
package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

const (
	// MaxBytes is about how much disk the cache may use.
	MaxBytes = 40 << 20
	// maxSegments is how many segments may pile up before they're merged.
	maxSegments = 16
	// flushEvery is how many answers a run holds before writing a segment,
	// so a run that's killed loses at most that many.
	flushEvery = 1000
)

// Evaluator answers from the cache where it can and asks next the rest.
// Noul answers are cached one by one, keyed by namespace, state and
// question, so editing one rule re-asks only that rule. Answers that came
// from the cache have no model or usage in the response. Close it when done,
// to save what it learned.
type Evaluator struct {
	next      bouncer.Evaluator
	dir       string
	namespace string
	// maxBytes is MaxBytes, but for tests.
	maxBytes int

	mu sync.Mutex
	// segments are open newest first; pending holds answers used since the
	// last segment this run wrote.
	segments []*segment
	pending  map[key]float64
}

// Open caches next's answers in dir, which it creates and keeps out of git.
// Namespace separates endpoints and models.
func Open(dir, namespace string, next bouncer.Evaluator) (*Evaluator, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		return nil, err
	}
	convertOneFilePerAnswer(dir)
	e := &Evaluator{next: next, dir: dir, namespace: namespace, maxBytes: MaxBytes, pending: map[key]float64{}}
	names, err := segmentNames(dir)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Backward(names) {
		s, err := openSegment(filepath.Join(dir, name))
		if err != nil {
			slog.Warn("sven: ignoring cache file", "err", err)
			continue
		}
		e.segments = append(e.segments, s)
	}
	return e, nil
}

func (e *Evaluator) Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	resp := &systemone.Response{Answers: map[string]systemone.Answer{}}
	missing := map[string]systemone.Question{}
	keys := map[string]key{}
	for id, q := range questions {
		if q.Kind() != systemone.KindNoul {
			missing[id] = q
			continue
		}
		k, err := e.key(state, q)
		if err != nil {
			return nil, err
		}
		keys[id] = k
		p, ok, err := e.find(k)
		if err != nil {
			return nil, err
		}
		if ok {
			resp.Answers[id] = systemone.Answer{Type: systemone.KindNoul, Noul: p}
			e.use(k, p)
		} else {
			missing[id] = q
		}
	}
	if len(missing) == 0 {
		return resp, nil
	}
	fresh, err := e.next.Evaluate(ctx, state, missing)
	if err != nil {
		return nil, err
	}
	resp.Model, resp.Usage = fresh.Model, fresh.Usage
	for id := range missing {
		a := fresh.Answers[id]
		resp.Answers[id] = a
		if k, ok := keys[id]; ok && a.Type == systemone.KindNoul {
			e.use(k, a.Noul)
		}
	}
	return resp, nil
}

func (e *Evaluator) key(state any, q systemone.Question) (key, error) {
	raw, err := json.Marshal(struct {
		Namespace string
		State     any
		Question  systemone.Question
	}{e.namespace, state, q})
	if err != nil {
		return key{}, fmt.Errorf("cache key: %w", err)
	}
	return sha256.Sum256(raw), nil
}

// find looks k up among this run's answers, then in the segments.
func (e *Evaluator) find(k key) (float64, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.pending[k]; ok {
		return p, true, nil
	}
	for _, s := range e.segments {
		if p, ok, err := s.find(k); err != nil || ok {
			return p, ok, err
		}
	}
	return 0, false, nil
}

// use records that this run used an answer, writing a segment once enough
// have piled up. Failing to write only costs requests later, so it's logged.
func (e *Evaluator) use(k key, p float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending[k] = p
	if len(e.pending) >= flushEvery {
		if err := e.flush(); err != nil {
			slog.Warn("sven: not caching answers", "err", err)
		}
	}
}

// flush writes the pending answers as a new segment, and opens it.
func (e *Evaluator) flush() error {
	if len(e.pending) == 0 {
		return nil
	}
	entries := make([]entry, 0, len(e.pending))
	for k, p := range e.pending {
		entries = append(entries, entry{k, p})
	}
	name := segmentName(time.Now())
	if err := writeSegment(e.dir, name, entries); err != nil {
		return err
	}
	e.pending = map[key]float64{}
	s, err := openSegment(filepath.Join(e.dir, name))
	if err != nil {
		return err
	}
	e.segments = slices.Insert(e.segments, 0, s)
	return nil
}

// Close saves the answers this run used, and merges segments if there are
// too many or they're too big.
func (e *Evaluator) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.flush()
	for _, s := range e.segments {
		err = errors.Join(err, s.f.Close())
	}
	e.segments = nil
	return errors.Join(err, tidy(e.dir, e.maxBytes))
}

// tidy merges the segments in dir into one if there are more than
// maxSegments or they take more than maxBytes, keeping the most recently
// used answers that fit in half of maxBytes. It also removes segments left
// half-written by runs that were killed.
func tidy(dir string, maxBytes int) error {
	removeAbandoned(dir)
	names, err := segmentNames(dir)
	if err != nil {
		return err
	}
	var size int64
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		size += info.Size()
	}
	if len(names) <= maxSegments && size <= int64(maxBytes) {
		return nil
	}

	// Newest first, so each key keeps its most recent answer, and the
	// answers kept are those used most recently.
	seen := map[key]bool{}
	var kept []entry
	keep := (maxBytes/2 - len(magic)) / recordSize
	for _, name := range slices.Backward(names) {
		entries, err := readSegment(filepath.Join(dir, name))
		if err != nil {
			slog.Warn("sven: dropping cache file", "err", err)
			continue
		}
		for _, en := range entries {
			if !seen[en.key] && len(kept) < keep {
				seen[en.key] = true
				kept = append(kept, en)
			}
		}
	}
	// The merged segment takes the newest segment's time, so segments
	// written later still count as newer.
	if err := writeSegment(dir, segmentName(segmentTime(names[len(names)-1])), kept); err != nil {
		return err
	}
	for _, name := range names {
		// A run tidying at the same time may have removed it already.
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// segmentName names a segment written at t: names sort by time, and a
// random part keeps runs that write at once apart.
func segmentName(t time.Time) string {
	return fmt.Sprintf("%020d-%s%s", t.UnixNano(), rand.Text()[:8], suffix)
}

// segmentTime is the time a segment's name records.
func segmentTime(name string) time.Time {
	var nanos int64
	if _, err := fmt.Sscanf(name, "%020d-", &nanos); err != nil {
		return time.Now()
	}
	return time.Unix(0, nanos)
}

// segmentNames lists the segments in dir, oldest first.
func segmentNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, en := range entries {
		if en.Type().IsRegular() && strings.HasSuffix(en.Name(), suffix) {
			names = append(names, en.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// abandonedAfter is how old a half-written segment must be to have been
// left by a run that was killed, rather than one still writing it.
const abandonedAfter = time.Hour

// removeAbandoned removes segments left half-written by killed runs.
func removeAbandoned(dir string) {
	tmps, err := filepath.Glob(filepath.Join(dir, tmpPrefix+"*"))
	if err != nil {
		slog.Warn("sven: listing cache files", "err", err)
		return
	}
	for _, tmp := range tmps {
		info, err := os.Stat(tmp)
		if err == nil && time.Since(info.ModTime()) > abandonedAfter {
			err = os.Remove(tmp)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("sven: removing abandoned cache file", "err", err)
		}
	}
}

// oneFilePerAnswer matches the directories where sven used to cache each
// answer in its own file: JSON named by its key in hex, under the key's
// first two hex digits.
var oneFilePerAnswer = regexp.MustCompile(`^[0-9a-f]{2}$`)

// convertOneFilePerAnswer moves the cache sven kept before segments into
// one, older than any other, and removes it.
func convertOneFilePerAnswer(dir string) {
	dirs, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("sven: listing cache files", "err", err)
		return
	}
	var old []string
	var entries []entry
	for _, d := range dirs {
		if !d.IsDir() || !oneFilePerAnswer.MatchString(d.Name()) {
			continue
		}
		old = append(old, filepath.Join(dir, d.Name()))
		files, err := filepath.Glob(filepath.Join(dir, d.Name(), "*.json"))
		if err != nil {
			slog.Warn("sven: listing cache files", "err", err)
			continue
		}
		for _, f := range files {
			if en, ok := oldAnswer(f); ok {
				entries = append(entries, en)
			}
		}
	}
	if len(old) == 0 {
		return
	}
	if len(entries) > 0 {
		if err := writeSegment(dir, segmentName(time.Unix(0, 0)), entries); err != nil {
			slog.Warn("sven: converting the old cache", "err", err)
			return
		}
	}
	for _, d := range old {
		if err := os.RemoveAll(d); err != nil {
			slog.Warn("sven: removing the old cache", "err", err)
		}
	}
}

// oldAnswer reads one answer of the old cache, if it's a noul.
func oldAnswer(path string) (entry, bool) {
	var en entry
	raw, err := os.ReadFile(path)
	var a systemone.Answer
	if err == nil {
		err = json.Unmarshal(raw, &a)
	}
	if err == nil {
		_, err = hex.Decode(en.key[:], []byte(strings.TrimSuffix(filepath.Base(path), ".json")))
	}
	if err != nil {
		slog.Warn("sven: skipping old cache entry", "path", path, "err", err)
		return en, false
	}
	en.p = a.Noul
	return en, a.Type == systemone.KindNoul
}

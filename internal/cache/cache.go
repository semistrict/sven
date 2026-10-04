// Package cache remembers System One answers on disk, so unchanged files
// aren't asked about again.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

// Evaluator answers from the cache where it can and asks next the rest.
// Questions are cached one by one, keyed by namespace, state and question,
// so editing one rule re-asks only that rule. Answers that came from the
// cache have no model or usage in the response.
type Evaluator struct {
	next      bouncer.Evaluator
	dir       string
	namespace string
}

// New caches next's answers in dir, which it creates and keeps out of git.
// Namespace separates endpoints and models.
func New(dir, namespace string, next bouncer.Evaluator) (*Evaluator, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		return nil, err
	}
	return &Evaluator{next: next, dir: dir, namespace: namespace}, nil
}

func (e *Evaluator) Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	resp := &systemone.Response{Answers: map[string]systemone.Answer{}}
	missing := map[string]systemone.Question{}
	paths := map[string]string{}
	for id, q := range questions {
		path, err := e.path(state, q)
		if err != nil {
			return nil, err
		}
		paths[id] = path
		if a, ok := load(path); ok && a.Type == q.Kind() {
			resp.Answers[id] = a
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
		resp.Answers[id] = fresh.Answers[id]
		store(paths[id], fresh.Answers[id])
	}
	return resp, nil
}

func (e *Evaluator) path(state any, q systemone.Question) (string, error) {
	key, err := json.Marshal(struct {
		Namespace string
		State     any
		Question  systemone.Question
	}{e.namespace, state, q})
	if err != nil {
		return "", fmt.Errorf("cache key: %w", err)
	}
	sum := sha256.Sum256(key)
	name := hex.EncodeToString(sum[:])
	return filepath.Join(e.dir, name[:2], name+".json"), nil
}

// load reads a cached answer. A missing or unreadable entry is a miss.
func load(path string) (systemone.Answer, bool) {
	var a systemone.Answer
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, false
	}
	if err == nil {
		err = json.Unmarshal(raw, &a)
	}
	if err != nil {
		slog.Warn("sven: ignoring cache entry", "path", path, "err", err)
		return a, false
	}
	return a, true
}

// store writes an answer atomically. Failing to cache only costs a request
// next time, so it's logged rather than returned.
func store(path string, a systemone.Answer) {
	if err := write(path, a); err != nil {
		slog.Warn("sven: not caching answer", "path", path, "err", err)
	}
}

func write(path string, a systemone.Answer) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return os.Rename(tmp.Name(), path)
}

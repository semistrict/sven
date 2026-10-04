// Package systemonetest provides an in-process System One server for tests.
// It validates requests against the protocol, answers noul questions with a
// judge function, and rejects everything else.
package systemonetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Judge returns the probability of yes for one noul question about state.
type Judge func(state map[string]any, instructions string) float64

type Server struct {
	*httptest.Server
	// Token is the bearer token every request must carry.
	Token string

	t        testing.TB
	judge    Judge
	envelope bool

	mu    sync.Mutex
	paths []string
}

// NewServer starts a server answering like TypeSafe's API.
func NewServer(t testing.TB, judge Judge) *Server {
	return start(t, judge, false)
}

// NewCloudflareServer starts a server that wraps answers in Cloudflare's
// REST envelope.
func NewCloudflareServer(t testing.TB, judge Judge) *Server {
	return start(t, judge, true)
}

func start(t testing.TB, judge Judge, envelope bool) *Server {
	s := &Server{Token: "test-token", t: t, judge: judge, envelope: envelope}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Paths returns the URL path of every request received, in order.
func (s *Server) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

type question struct {
	Type         string          `json:"type"`
	Instructions any             `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

type request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]question `json:"questions"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, `{"detail":"invalid API key"}`, http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, `{"detail":"want application/json"}`, http.StatusUnsupportedMediaType)
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.invalid(w, "body: %v", err)
		return
	}
	state, ok := req.State.(map[string]any)
	switch {
	case req.Model == "":
		s.invalid(w, "model: field required")
		return
	case !ok:
		s.invalid(w, "state: this fake only accepts objects")
		return
	case len(req.Questions) == 0 || len(req.Questions) > 64:
		s.invalid(w, "questions: want 1 to 64, got %d", len(req.Questions))
		return
	}

	answers := map[string]any{}
	for id, q := range req.Questions {
		instructions, ok := q.Instructions.(string)
		if q.Type != "noul" || !ok || instructions == "" {
			s.invalid(w, "questions.%s: this fake only answers nouls with string instructions", id)
			return
		}
		answers[id] = map[string]any{"type": "noul", "noul": s.judge(state, instructions)}
	}
	var body any = map[string]any{
		"model":   req.Model,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 100, "output_tokens": 2 * len(answers)},
	}
	if s.envelope {
		body = map[string]any{"result": body, "success": true, "errors": []any{}, "messages": []any{}}
	}
	s.write(w, http.StatusOK, body)
}

func (s *Server) invalid(w http.ResponseWriter, format string, args ...any) {
	s.write(w, http.StatusUnprocessableEntity, map[string]string{"detail": fmt.Sprintf(format, args...)})
}

func (s *Server) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.t.Errorf("systemonetest: writing response: %v", err)
	}
}

package systemone

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Response bodies are taken from TypeSafe's API reference.
const (
	noulResponse = `{
	  "model": "jev-1.13.0",
	  "answers": {"is_urgent": {"type": "noul", "noul": 0.95}},
	  "usage": {"input_tokens": 296, "output_tokens": 20}
	}`
	mixedResponse = `{
	  "model": "jev-1.13.0",
	  "answers": {
	    "department": {
	      "type": "choice",
	      "choice": "billing",
	      "probabilities": {"billing": 0.88, "technical": 0.12, "sales": 0.0},
	      "confidence": 0.81
	    },
	    "frustration": {
	      "type": "score",
	      "score": 1.05,
	      "legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
	      "probabilities": {"0": 0.0, "1": 0.95, "2": 0.05},
	      "confidence": 0.92
	    }
	  },
	  "usage": {"input_tokens": 318, "output_tokens": 34}
	}`
)

type recorded struct {
	path, auth, contentType string
	body                    map[string]any
}

// serve answers every request with status and body, recording what it got.
func serve(t *testing.T, status int, body string, header http.Header) (*httptest.Server, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		rec := recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
		if err := json.Unmarshal(raw, &rec.body); err != nil {
			t.Errorf("request is not JSON: %v: %s", err, raw)
		}
		got = append(got, rec)
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestTypeSafeRequest(t *testing.T) {
	srv, got := serve(t, http.StatusOK, noulResponse, nil)
	c := TypeSafe("ts-key", WithBaseURL(srv.URL+"/"))

	resp, err := c.Evaluate(t.Context(), "Help! My payouts have been failing for 3 days.", map[string]Question{
		"is_urgent": Noul{Instructions: "Does this convey urgency?", True: "Explicitly time-sensitive", False: "No urgency expressed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := (*got)[0]
	if req.path != "/v1/systemone" || req.auth != "Bearer ts-key" || req.contentType != "application/json" {
		t.Errorf("request = %s auth=%q type=%q", req.path, req.auth, req.contentType)
	}
	want := `{"model":"jev-latest","questions":{"is_urgent":{"criteria":{"false":"No urgency expressed","true":"Explicitly time-sensitive"},"instructions":"Does this convey urgency?","type":"noul"}},"state":"Help! My payouts have been failing for 3 days."}`
	if b, _ := json.Marshal(req.body); string(b) != want {
		t.Errorf("body = %s\nwant  %s", b, want)
	}
	if resp.Model != "jev-1.13.0" || resp.Answers["is_urgent"].Noul != 0.95 || resp.Usage.InputTokens != 296 {
		t.Errorf("response = %+v", resp)
	}
}

func TestChoiceAndScore(t *testing.T) {
	srv, got := serve(t, http.StatusOK, mixedResponse, nil)
	c := TypeSafe("k", WithBaseURL(srv.URL))

	resp, err := c.Evaluate(t.Context(), map[string]any{"ticket": "Help!"}, map[string]Question{
		"department": Choice{Instructions: "Which team should handle this?", Options: map[string]any{
			"billing": "Payments, invoicing, refunds", "technical": nil, "sales": "Pricing",
		}},
		"frustration": Score{Instructions: "How frustrated is the customer?", Levels: []any{"Calm", "Frustrated", "Very angry"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"department":{"criteria":{"billing":"Payments, invoicing, refunds","sales":"Pricing","technical":null},"instructions":"Which team should handle this?","type":"choice"},"frustration":{"criteria":["Calm","Frustrated","Very angry"],"instructions":"How frustrated is the customer?","type":"score"}}`
	if b, _ := json.Marshal((*got)[0].body["questions"]); string(b) != want {
		t.Errorf("questions = %s\nwant        %s", b, want)
	}
	dept, frus := resp.Answers["department"], resp.Answers["frustration"]
	if dept.Choice != "billing" || dept.Probabilities["technical"] != 0.12 || dept.Confidence != 0.81 {
		t.Errorf("department = %+v", dept)
	}
	if frus.Score != 1.05 || frus.Legend["2"] != "Very angry" || frus.Probabilities["1"] != 0.95 || frus.Confidence != 0.92 {
		t.Errorf("frustration = %+v", frus)
	}
}

func TestCloudflareEnvelope(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"result":`+noulResponse+`,"success":true,"errors":[],"messages":[]}`, nil)
	c := Cloudflare("acct", "cf-token", WithBaseURL(srv.URL), WithModel("clef"))

	resp, err := c.Evaluate(t.Context(), "state", map[string]Question{"is_urgent": Noul{Instructions: "Urgent?"}})
	if err != nil {
		t.Fatal(err)
	}

	req := (*got)[0]
	if req.path != "/accounts/acct/ai/run/@cf/cloudflare/clef" || req.auth != "Bearer cf-token" || req.body["model"] != "clef" {
		t.Errorf("request = %s auth=%q model=%v", req.path, req.auth, req.body["model"])
	}
	if resp.Answers["is_urgent"].Noul != 0.95 {
		t.Errorf("response = %+v", resp)
	}
}

func TestCloudflareUnsuccessful(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, `{"success":false,"errors":[{"code":2021,"message":"Insufficient balance"}],"result":null}`, nil)
	c := Cloudflare("acct", "t", WithBaseURL(srv.URL))

	_, err := c.Evaluate(t.Context(), "state", map[string]Question{"q": Noul{Instructions: "?"}})

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "Insufficient balance (code 2021)" {
		t.Fatalf("err = %v", err)
	}
}

func TestValidationErrorIsNotRetried(t *testing.T) {
	srv, got := serve(t, http.StatusUnprocessableEntity, `{"detail":"questions.q.instructions: field required"}`, nil)
	c := TypeSafe("k", WithBaseURL(srv.URL))

	_, err := c.Evaluate(t.Context(), "state", map[string]Question{"q": Noul{}})

	if err == nil || err.Error() != `system one API: 422 Unprocessable Entity: {"detail":"questions.q.instructions: field required"}` {
		t.Fatalf("err = %v", err)
	}
	if len(*got) != 1 {
		t.Errorf("sent %d requests, want 1", len(*got))
	}
}

func TestRetriesOverload(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(529)
			return
		}
		io.WriteString(w, noulResponse)
	}))
	t.Cleanup(srv.Close)
	c := TypeSafe("k", WithBaseURL(srv.URL))
	c.backoff = time.Millisecond

	resp, err := c.Evaluate(t.Context(), "state", map[string]Question{"is_urgent": Noul{Instructions: "?"}})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || resp.Answers["is_urgent"].Noul != 0.95 {
		t.Errorf("attempts = %d, response = %+v", attempts, resp)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	srv, got := serve(t, http.StatusTooManyRequests, `{"detail":"slow down"}`, http.Header{"Retry-After": {"0"}})
	c := TypeSafe("k", WithBaseURL(srv.URL))
	c.backoff = time.Millisecond

	_, err := c.Evaluate(t.Context(), "state", map[string]Question{"q": Noul{Instructions: "?"}})

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v", err)
	}
	if len(*got) != defaultMaxAttempts {
		t.Errorf("sent %d requests, want %d", len(*got), defaultMaxAttempts)
	}
}

func TestMissingAnswer(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, noulResponse, nil)
	c := TypeSafe("k", WithBaseURL(srv.URL))

	_, err := c.Evaluate(t.Context(), "state", map[string]Question{"other": Noul{Instructions: "?"}})

	if err == nil || err.Error() != `response has no answer for question "other"` {
		t.Fatalf("err = %v", err)
	}
}

func TestMismatchedAnswerKind(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, noulResponse, nil)
	c := TypeSafe("k", WithBaseURL(srv.URL))

	_, err := c.Evaluate(t.Context(), "state", map[string]Question{"is_urgent": Score{Instructions: "?", Levels: []any{"a", "b"}}})

	if err == nil || err.Error() != `question "is_urgent" is a score but its answer is a noul` {
		t.Fatalf("err = %v", err)
	}
}

func TestNoulWithoutCriteria(t *testing.T) {
	b, err := json.Marshal(Noul{Instructions: "Urgent?"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"noul","instructions":"Urgent?"}`; string(b) != want {
		t.Errorf("Noul = %s, want %s", b, want)
	}
}

func TestCost(t *testing.T) {
	usage := Usage{InputTokens: 2_000_000, OutputTokens: 40}
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"jev-latest", 0.084},
		{"jev-1.13.0", 0.084},
		{"clef-flash", 0.18},
		{"clef", 0.48},
	} {
		if got, ok := Cost(tc.model, usage); !ok || got != tc.want {
			t.Errorf("Cost(%s) = %v, %v; want %v", tc.model, got, ok, tc.want)
		}
	}
	if _, ok := Cost("gpt-6-luna", usage); ok {
		t.Error("Cost(gpt-6-luna) is known, want unknown")
	}
}

func TestUsageSummary(t *testing.T) {
	for _, tc := range []struct {
		usage Usage
		model string
		want  string
	}{
		{Usage{InputTokens: 183_400}, "jev-latest", "183400 input tokens on jev-latest, $0.007703"},
		{Usage{InputTokens: 1_000}, "gpt-6-luna", "1000 input tokens on gpt-6-luna"},
		{Usage{}, "clef-flash", "every answer came from the cache, $0"},
	} {
		if got := tc.usage.Summary(tc.model); got != tc.want {
			t.Errorf("Summary = %q, want %q", got, tc.want)
		}
	}
}

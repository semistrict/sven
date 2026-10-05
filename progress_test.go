package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

func TestStatusMidway(t *testing.T) {
	s := status{
		total:    120,
		done:     30,
		rejected: 2,
		warned:   1,
		inFlight: 8,
		recent: []flagged{
			{path: "auth/login.go", level: bouncer.Error, rules: []string{"debug-leftovers", "sus"}},
			{path: "auth/login_test.go", level: bouncer.Warn, rules: []string{"weakened-tests"}},
		},
		cost:    "$0.0013",
		elapsed: 15 * time.Second,
		frame:   3,
		width:   100,
	}

	want := []string{
		"⠸ sven at the door  ████░░░░░░░░░░░░ 30/120  ✗ 2  ! 1  0:15 · ~0:45 left · $0.0013",
		"  8 requests in flight · recently flagged:",
		"  ✗ auth/login.go  debug-leftovers, sus",
		"  ! auth/login_test.go  weakened-tests",
	}
	if got := s.render(false); !reflect.DeepEqual(got, want) {
		t.Errorf("render =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStatusShowsLastFileUntilSomethingIsFlagged(t *testing.T) {
	s := status{total: 2, done: 2, last: "README.md", cost: "free", elapsed: 61 * time.Second, width: 100}

	want := []string{
		"⠋ sven at the door  ████████████████ 2/2  ✗ 0  ! 0  1:01 · free",
		"  0 requests in flight · last in: README.md",
	}
	if got := s.render(false); !reflect.DeepEqual(got, want) {
		t.Errorf("render =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStatusFitsNarrowTerminals(t *testing.T) {
	s := status{
		total:    10,
		done:     1,
		inFlight: 3,
		recent:   []flagged{{path: "a/very/long/path/to/some/file.go", level: bouncer.Error, rules: []string{"sus"}}},
		cost:     "free",
		width:    24,
	}

	want := []string{
		"⠋ sven at the door  █░░…",
		"  3 requests in flight …",
		"  ✗ a/very/long/path/to…",
	}
	if got := s.render(false); !reflect.DeepEqual(got, want) {
		t.Errorf("render =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestProgressNeedsATerminal(t *testing.T) {
	var out strings.Builder
	if g := startProgress(&out, false, 3, func(systemone.Usage) string { return "" }); g != nil {
		t.Error("startProgress on a buffer = non-nil, want nil")
	}
}

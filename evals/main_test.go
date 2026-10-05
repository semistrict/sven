package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/systemone/systemonetest"
)

func TestCasesCoverEveryRule(t *testing.T) {
	labels, err := config.BuiltinIDs()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadCases("cases", labels)
	if err != nil {
		t.Fatal(err)
	}
	positives := map[string]int{}
	clean := 0
	for _, c := range cases {
		for _, id := range c.Expect {
			positives[id]++
		}
		if len(c.Expect) == 0 {
			clean++
		}
	}
	for _, id := range labels {
		if positives[id] < 2 {
			t.Errorf("rule %s has %d positive cases, want at least 2", id, positives[id])
		}
	}
	if clean < 10 {
		t.Errorf("%d cases break no rule, want at least 10", clean)
	}
}

// TestReport runs the evals against a fake model that only recognizes
// println, so it misses breakpoint() and false-alarms on CLI output.
func TestReport(t *testing.T) {
	srv := systemonetest.NewServer(t, func(state map[string]any, instructions string) float64 {
		if strings.Contains(instructions, "leftover debugging code") && strings.Contains(state["diff"].(string), "+\tfmt.Print") {
			return 0.9
		}
		return 0.1
	})
	t.Setenv("SVEN_PROVIDER", "typesafe")
	t.Setenv("SVEN_MODEL", "")
	t.Setenv("TYPESAFE_API_KEY", srv.Token)
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)
	cfg := t.TempDir() + "/sven.yaml"
	writeFile(t, cfg, "inherit_rules: false\nrules:\n  - id: debug-leftovers\n    question: Do the lines added in `diff` include leftover debugging code?\n")
	cases := t.TempDir()
	writeFile(t, cases+"/print.yaml", "path: a.go\nexpect: [debug-leftovers]\ndiff: |\n  @@ -1 +1 @@\n  +\tfmt.Println(x)\n")
	writeFile(t, cases+"/breakpoint.yaml", "path: b.py\nexpect: [debug-leftovers]\ndiff: |\n  @@ -1 +1 @@\n  +breakpoint()\n")
	writeFile(t, cases+"/cli.yaml", "path: c.go\nexpect: []\nwhy: CLIs print.\ndiff: |\n  @@ -1 +1 @@\n  +\tfmt.Printf(\"%d\\n\", n)\n")
	writeFile(t, cases+"/clean.yaml", "path: d.go\nexpect: []\ndiff: |\n  @@ -1 +1 @@\n  +\tx++\n")

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-config", cfg, "-cases", cases, "-cache", t.TempDir()}, &stdout, &stderr)

	want := `jev-latest with 1 rules on 4 cases
cost: 400 input tokens on jev-latest, $0.000017

rule                     tp   fp   fn   tn precision recall
debug-leftovers           1    1    1    1      0.50   0.50
all labeled rules         1    1    1    1      0.50   0.50
any rule, per case        1    1    1    1      0.50   0.50

             labeled rules            cases
threshold precision recall precision recall
0.1            0.50   1.00      0.50   1.00
0.2            0.50   0.50      0.50   0.50
0.3            0.50   0.50      0.50   0.50
0.4            0.50   0.50      0.50   0.50
0.5            0.50   0.50      0.50   0.50
0.6            0.50   0.50      0.50   0.50
0.7            0.50   0.50      0.50   0.50
0.8            0.50   0.50      0.50   0.50
0.9            0.50   0.50      0.50   0.50

by rule
missed      breakpoint                     debug-leftovers         10%
false alarm cli                            debug-leftovers         90%
            CLIs print.

by case, with its most probable rule
missed      breakpoint                     debug-leftovers         10%
false alarm cli                            debug-leftovers         90%
            CLIs print.
`
	if code != 0 || stdout.String() != want || stderr.String() != "" {
		t.Errorf("code %d\nstdout:\n%s\nstderr:\n%s\nwant stdout:\n%s", code, stdout.String(), stderr.String(), want)
	}
}

func TestRejectsUnknownRule(t *testing.T) {
	labels, err := config.BuiltinIDs()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir+"/x.yaml", "path: a.go\nexpect: [no-such-rule]\ndiff: |\n  @@ -1 +1 @@\n  +x\n")

	_, err = loadCases(dir, labels)

	if want := dir + `/x.yaml: expects unknown rule "no-such-rule"`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

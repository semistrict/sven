package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sven/internal/bouncer"
)

// tree writes files, keyed by slash path, under a fresh root and loads it.
func tree(t *testing.T, files map[string]string) (*Tree, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := Load(root, filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	return tr, root
}

func ids(rules []bouncer.Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.ID)
	}
	return out
}

// builtinIDs lists the built-in rules, in order.
var builtinIDs = func() []string {
	l, err := decode(Default)
	if err != nil {
		panic(err)
	}
	var ids []string
	for _, r := range l.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}()

// without returns ids minus some, keeping order.
func without(ids []string, drop ...string) []string {
	var out []string
	for _, id := range ids {
		if !slices.Contains(drop, id) {
			out = append(out, id)
		}
	}
	return out
}

func rule(t *testing.T, rules []bouncer.Rule, id string) bouncer.Rule {
	t.Helper()
	for _, r := range rules {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no rule %s in %v", id, ids(rules))
	return bouncer.Rule{}
}

// builtinExclude lists the built-in exclude globs.
var builtinExclude = func() []string {
	l, err := decode(Default)
	if err != nil {
		panic(err)
	}
	return l.Exclude
}()

func TestDefaults(t *testing.T) {
	tr, _ := tree(t, nil)
	c, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Provider != Sven || tr.Model != "" {
		t.Errorf("provider, model = %q, %q", tr.Provider, tr.Model)
	}
	if got, want := ids(c.Rules), without(builtinIDs, "sus"); !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(c.Exclude, builtinExclude) {
		t.Errorf("exclude = %v", c.Exclude)
	}
	first := c.Rules[0]
	if first.Error != 0.7 || first.Warn != bouncer.Off || first.Violation == "" || first.OK == "" {
		t.Errorf("first rule = %+v", first)
	}
	for _, r := range c.Rules {
		if r.ID == "weakened-tests" && (r.Error != bouncer.Off || r.Warn != 0.5) {
			t.Errorf("weakened-tests = %+v, want warn only", r)
		}
	}
}

func TestEmptyRootFile(t *testing.T) {
	tr, _ := tree(t, map[string]string{FileName: ""})
	c, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(c.Rules), without(builtinIDs, "sus"); !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
}

func TestLayering(t *testing.T) {
	tr, _ := tree(t, map[string]string{
		FileName: `
provider: cloudflare
model: clef
error: 0.6
warn: 0.4
exclude: ["**/*.gen.go"]
rules:
  - id: narrating-comments
    disabled: true
  - id: no-yelling
    question: Does ` + "`diff`" + ` add comments in all caps?
`,
		"web/" + FileName: `
error: 0.8
exclude: ["dist/**"]
rules:
  - id: no-yelling
    question: Does ` + "`diff`" + ` add JSX comments in all caps?
    error: off
    warn: 0.3
  - id: no-any
    question: Does ` + "`diff`" + ` add the TypeScript any type?
`,
		"web/legacy/" + FileName: `
rules:
  - id: narrating-comments
    question: Do comments narrate?
  - id: no-any
    disabled: true
`,
	})

	root, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	web, err := tr.For("web/src")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := tr.For("web/legacy/old")
	if err != nil {
		t.Fatal(err)
	}

	if tr.Provider != Cloudflare || tr.Model != "clef" {
		t.Errorf("provider, model = %q, %q", tr.Provider, tr.Model)
	}
	if got, want := ids(root.Rules), append(without(builtinIDs, "narrating-comments", "sus"), "no-yelling"); !reflect.DeepEqual(got, want) {
		t.Errorf("root rules = %v, want %v", got, want)
	}
	if got, want := ids(web.Rules), append(without(builtinIDs, "narrating-comments", "sus"), "no-yelling", "no-any"); !reflect.DeepEqual(got, want) {
		t.Errorf("web rules = %v, want %v", got, want)
	}
	if got, want := ids(legacy.Rules), append(without(builtinIDs, "sus"), "no-yelling"); !reflect.DeepEqual(got, want) {
		t.Errorf("legacy rules = %v, want %v", got, want)
	}

	if r := root.Rules[0]; r.Error != 0.6 || r.Warn != 0.4 {
		t.Errorf("root thresholds = %v, %v; want 0.6, 0.4", r.Error, r.Warn)
	}
	if r := web.Rules[0]; r.Error != 0.8 || r.Warn != 0.4 {
		t.Errorf("web thresholds = %v, %v; want 0.8, 0.4", r.Error, r.Warn)
	}
	yelling := rule(t, web.Rules, "no-yelling")
	if want := (bouncer.Rule{ID: "no-yelling", Question: "Does `diff` add JSX comments in all caps?", Error: bouncer.Off, Warn: 0.3}); yelling != want {
		t.Errorf("web no-yelling = %+v, want %+v", yelling, want)
	}
	if got := rule(t, legacy.Rules, "narrating-comments"); got.Question != "Do comments narrate?" || got.Violation != "" || got.Error != 0.8 {
		t.Errorf("legacy narrating-comments = %+v", got)
	}

	if want := append(builtinExclude[:len(builtinExclude):len(builtinExclude)], "**/*.gen.go", "web/dist/**"); !reflect.DeepEqual(legacy.Exclude, want) {
		t.Errorf("legacy exclude = %v, want %v", legacy.Exclude, want)
	}
}

func TestDirectoryWithoutConfigInherits(t *testing.T) {
	tr, _ := tree(t, map[string]string{"a/" + FileName: "error: 0.9\n"})

	c, err := tr.For("a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	if c.Rules[0].Error != 0.9 {
		t.Errorf("error = %v, want 0.9", c.Rules[0].Error)
	}
}

func TestEverythingDisabled(t *testing.T) {
	var off string
	for _, id := range builtinIDs {
		off += "  - {id: " + id + ", disabled: true}\n"
	}
	tr, _ := tree(t, map[string]string{"docs/" + FileName: "rules:\n" + off})

	c, err := tr.For("docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rules) != 0 {
		t.Errorf("rules = %v, want none", ids(c.Rules))
	}
}

func TestNestedProviderIsAnError(t *testing.T) {
	tr, root := tree(t, map[string]string{"sub/" + FileName: "model: clef\n"})

	_, err := tr.For("sub")

	if want := filepath.Join(root, "sub", FileName) + ": provider, model, and allow_request_storage can only be set in the root .sven.yaml"; err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant  %s", err, want)
	}
}

func TestInvalid(t *testing.T) {
	for _, tc := range []struct{ name, yaml, err string }{
		{"provider", "provider: openai", `provider "openai": want sven, typesafe, or cloudflare`},
		{"threshold", "error: 1.5", "line 1: threshold 1.5: want more than 0 and at most 1, or off"},
		{"threshold word", "warn: never", "line 1: threshold never: want more than 0 and at most 1, or off"},
		{"unknown field", "threshold: 0.5", "yaml: unmarshal errors:\n  line 1: field threshold not found in type config.layer"},
		{"bad id", "rules: [{id: has space, question: q}]", `rule id "has space": use 1 to 100 letters, digits, '_', '.', or '-'`},
		{"duplicate id", "rules: [{id: a, question: q}, {id: a, question: q}]", `rule id "a" is used twice`},
		{"rule threshold", "rules: [{id: a, question: q, warn: 0}]", "line 1: threshold 0: want more than 0 and at most 1, or off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(filepath.Dir(path), path)
			if want := path + ": " + tc.err; err == nil || err.Error() != want {
				t.Errorf("err = %v\nwant  %s", err, want)
			}
		})
	}
}

func TestTooManyRules(t *testing.T) {
	var rules string
	for i := range 65 - len(without(builtinIDs, "sus")) {
		rules += "  - {id: r" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ", question: q}\n"
	}
	tr, _ := tree(t, map[string]string{FileName: "rules:\n" + rules})

	_, err := tr.For(".")

	if want := ".: 65 rules apply, want at most 64"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestInheritRulesFalse(t *testing.T) {
	tr, _ := tree(t, map[string]string{
		FileName:             "rules:\n  - {id: house, question: q}\n",
		"docs/" + FileName:   "inherit_rules: false\nrules:\n  - {id: prose, question: q}\n",
		"docs/a/" + FileName: "rules:\n  - {id: more, question: q}\n",
	})

	docs, err := tr.For("docs/a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(docs.Rules), []string{"prose", "more"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
	if docs.Rules[0].Error != 0.7 {
		t.Errorf("error = %v, want the inherited 0.7", docs.Rules[0].Error)
	}
}

func TestWorkerAllowListIsCurrent(t *testing.T) {
	want, err := BuiltinQuestions()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../worker/src/builtin.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("worker/src/builtin.json is stale: run go generate ./internal/config")
	}
}

func TestRuleWithoutQuestionAdjustsTheOneAbove(t *testing.T) {
	tr, root := tree(t, map[string]string{
		FileName:        "rules:\n  - {id: sus, disabled: false}\n  - {id: unfinished, error: 0.9}\n",
		"x/" + FileName: "rules:\n  - {id: made-up}\n",
	})

	c, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	if got := rule(t, c.Rules, "sus"); got.Error != 0.95 || got.Warn != 0.7 || got.Question == "" {
		t.Errorf("sus = %+v, want the built-in rule turned on", got)
	}
	if got := rule(t, c.Rules, "unfinished"); got.Error != 0.9 || got.Question == "" {
		t.Errorf("unfinished = %+v, want the built-in rule with error 0.9", got)
	}

	_, err = tr.For("x")
	if want := filepath.Join("x", FileName) + ": rule made-up has no question, and there's no rule made-up above it to adjust"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s (root %s)", err, want, root)
	}
}

func TestOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Overrides
		want []string
	}{
		{"with turns on a rule that's off by default", Overrides{With: []string{"sus"}}, builtinIDs},
		{"no turns rules off", Overrides{No: []string{"emoji", "debug-leftovers"}}, without(builtinIDs, "sus", "emoji", "debug-leftovers")},
		{"only keeps just these, even if off by default", Overrides{Only: []string{"sus", "emoji"}}, []string{"emoji", "sus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, _ := tree(t, nil)
			tr.Overrides = tc.o
			c, err := tr.For(".")
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(c.Rules); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rules = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestErrorsOnly(t *testing.T) {
	tr, _ := tree(t, nil)
	tr.Overrides = Overrides{With: []string{"sus"}, ErrorsOnly: true}

	c, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Rules {
		if r.Error == bouncer.Off || r.Warn != bouncer.Off {
			t.Errorf("rule %s: error %v, warn %v; want only rules that can fail, never warning", r.ID, r.Error, r.Warn)
		}
	}
	if got := ids(c.Rules); slices.Contains(got, "weakened-tests") || !slices.Contains(got, "sus") {
		t.Errorf("rules = %v, want warn-only weakened-tests dropped and sus kept", got)
	}
}

func TestUnknownOverrides(t *testing.T) {
	tr, _ := tree(t, nil)
	tr.Overrides = Overrides{With: []string{"sus", "suss"}, No: []string{"emoji", "emojis"}}

	if _, err := tr.For("."); err != nil {
		t.Fatal(err)
	}
	if got, want := tr.Unknown(), []string{"suss", "emojis"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Unknown = %v, want %v", got, want)
	}
}

func TestAdviceAccumulatesFromTheRootDown(t *testing.T) {
	tr, _ := tree(t, map[string]string{
		FileName:              "advice: |\n  This is a CLI.\n",
		"web/" + FileName:     "inherit_rules: false\nadvice: Emoji in output is our house style.\n",
		"web/src/" + FileName: "error: 0.9\n",
		"lib/" + FileName:     "advice: \"  \"\n",
	})
	tr.Overrides.Advice = []string{"Be kind."}
	for dir, want := range map[string]string{
		".":       "This is a CLI.\n\nBe kind.",
		"web/src": "This is a CLI.\n\nEmoji in output is our house style.\n\nBe kind.",
		"lib":     "This is a CLI.\n\nBe kind.",
	} {
		c, err := tr.For(dir)
		if err != nil {
			t.Fatal(err)
		}
		if c.Advice != want {
			t.Errorf("For(%q).Advice = %q, want %q", dir, c.Advice, want)
		}
	}
}

func TestNoAdviceByDefault(t *testing.T) {
	tr, _ := tree(t, nil)
	c, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	if c.Advice != "" {
		t.Errorf("Advice = %q, want none", c.Advice)
	}
}

func TestStarterUsesTheBuiltInRules(t *testing.T) {
	for allow, provider := range map[bool]string{true: Sven, false: TypeSafe} {
		tr, _ := tree(t, map[string]string{FileName: Starter(allow)})
		if tr.Provider != provider || tr.AllowRequestStorage != allow {
			t.Errorf("Starter(%v): provider %q, allow_request_storage %v; want %q, %v", allow, tr.Provider, tr.AllowRequestStorage, provider, allow)
		}
		c, err := tr.For(".")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := ids(c.Rules), without(builtinIDs, "sus"); !slices.Equal(got, want) {
			t.Errorf("Starter(%v) rules = %v, want the built-in ones, %v", allow, got, want)
		}
		if c.Advice != "" {
			t.Errorf("Starter(%v) advice = %q, want none", allow, c.Advice)
		}
	}
}

// setting matches a commented-out setting or its indented continuation.
var setting = regexp.MustCompile(`^# ([a-z_]+:\s|  )`)

func TestStarterExamplesWork(t *testing.T) {
	var lines []string
	for line := range strings.Lines(Starter(true)) {
		if setting.MatchString(line) {
			line = strings.TrimPrefix(line, "# ")
		}
		lines = append(lines, line)
	}
	tr, _ := tree(t, map[string]string{FileName: strings.Join(lines, "")})
	c, err := tr.For(".")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(c.Rules), append(slices.Clone(builtinIDs), "no-console"); !slices.Equal(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
	if got, want := rule(t, c.Rules, "unfinished").Error, 0.9; got != want {
		t.Errorf("unfinished error = %v, want %v", got, want)
	}
	if want := "This is a CLI: what it prints is its output, not debugging."; c.Advice != want {
		t.Errorf("advice = %q, want %q", c.Advice, want)
	}
	if !c.Excluded("testdata/golden.txt") {
		t.Error("testdata/golden.txt is not excluded")
	}
}

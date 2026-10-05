// Package config loads the .sven.yaml files of a work tree.
//
// Settings layer from the built-in config, through the .sven.yaml at the
// work tree root, down through each directory to the one holding a changed
// file. Rules merge by id, the closest definition replacing the whole rule,
// unless a file sets inherit_rules: false to start over;
// the closest error and warn thresholds win; exclude patterns and advice
// accumulate, each exclude relative to the directory of the file listing it. Provider and model belong to the
// root alone, since one run uses one model.
package config

import (
	"bytes"
	"cmp"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/semistrict/sven/internal/bouncer"
)

// FileName is the name of sven's config file in any directory.
const FileName = ".sven.yaml"

// Default is the built-in config.
//
//go:embed default.yaml
var Default []byte

//go:embed starter.yaml
var starter string

// freeAPI is starter's provider setting.
const freeAPI = "provider: sven\nallow_request_storage: true\n"

// ownKey replaces freeAPI for projects that don't let the free sven API
// store their requests.
const ownKey = `# Uses your own key: set TYPESAFE_API_KEY. Or use provider: cloudflare,
# with CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN.
provider: typesafe
`

// Starter is the config file sven init writes: the provider, and examples
// of what else can be set, commented out. allowRequestStorage picks the
// free sven API over the user's own key.
func Starter(allowRequestStorage bool) string {
	if allowRequestStorage {
		return starter
	}
	return strings.Replace(starter, freeAPI, ownKey, 1)
}

const (
	Sven       = "sven"
	TypeSafe   = "typesafe"
	Cloudflare = "cloudflare"

	// maxRules is the most questions Cloudflare accepts in one request.
	maxRules = 64
)

// Threshold is the probability at or above which a rule fires, or "off".
// Zero means unset, so the closest setting above applies.
type Threshold float64

func (t *Threshold) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "off" {
		*t = Threshold(bouncer.Off)
		return nil
	}
	var p float64
	if err := n.Decode(&p); err != nil || p <= 0 || p > 1 {
		return fmt.Errorf("line %d: threshold %s: want more than 0 and at most 1, or off", n.Line, n.Value)
	}
	*t = Threshold(p)
	return nil
}

// Rule is a rule as written in a config file.
type Rule struct {
	ID        string    `yaml:"id"`
	Question  string    `yaml:"question"`
	Violation string    `yaml:"violation"`
	OK        string    `yaml:"ok"`
	Error     Threshold `yaml:"error"`
	Warn      Threshold `yaml:"warn"`
	// Disabled turns the rule off, or with false, back on.
	Disabled *bool `yaml:"disabled"`
}

// adjust applies a rule without a question to the rule it inherits: the
// thresholds and disabled it sets, and nothing else.
func (base Rule) adjust(r Rule) Rule {
	base.Error = cmp.Or(r.Error, base.Error)
	base.Warn = cmp.Or(r.Warn, base.Warn)
	if r.Disabled != nil {
		base.Disabled = r.Disabled
	}
	return base
}

func (r Rule) disabled() bool { return r.Disabled != nil && *r.Disabled }

// Overrides change what applies everywhere, on top of every config file:
// what sven's command-line flags set.
type Overrides struct {
	// With turns rules on, including built-in ones that are off by default.
	With []string
	// No turns rules off.
	No []string
	// Only, if not empty, turns on these rules and turns off all others.
	Only []string
	// ErrorsOnly drops warnings: rules that can't fail a check aren't asked,
	// and the rest never warn.
	ErrorsOnly bool
	// Advice follows the advice of every config file.
	Advice []string
}

// layer is one config file.
type layer struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// AllowRequestStorage is the project's consent to the free sven API
	// storing its requests and responses.
	AllowRequestStorage bool      `yaml:"allow_request_storage"`
	Error               Threshold `yaml:"error"`
	Warn                Threshold `yaml:"warn"`
	Exclude             []string  `yaml:"exclude"`
	// Advice tells the model about the code under this file's directory.
	Advice string `yaml:"advice"`
	// InheritRules false drops the rules of the layers above.
	InheritRules *bool  `yaml:"inherit_rules"`
	Rules        []Rule `yaml:"rules"`
}

// placed is a layer and the directory it applies to, relative to the root.
type placed struct {
	dir   string
	layer *layer
}

// Config is what applies to the files in one directory.
type Config struct {
	// Rules are the enabled rules, each with its threshold resolved.
	Rules []bouncer.Rule
	// Exclude holds globs relative to the work tree root.
	Exclude []string
	// Advice is what the config files above, from the root down, and the
	// overrides tell the model about the code, or empty.
	Advice string
}

// Tree reads config files from a work tree as directories are asked for.
type Tree struct {
	Provider string
	// AllowRequestStorage is whether the project agreed to the free sven API
	// storing its requests and responses.
	AllowRequestStorage bool
	// Model is empty for the provider's default.
	Model string
	// Overrides apply on top of every config file. Set them before calling
	// For.
	Overrides Overrides

	// known holds the id of every rule For has seen, on or off.
	known   map[string]bool
	root    string
	top     []placed
	layers  map[string]*layer
	configs map[string]Config
}

// Load reads the built-in config and the root config file, if file is not
// empty and exists. Configs in other directories under root load on demand.
func Load(root, file string) (*Tree, error) {
	builtin, err := decode(Default)
	if err != nil {
		return nil, fmt.Errorf("built-in config: %w", err)
	}
	t := &Tree{root: root, layers: map[string]*layer{}, configs: map[string]Config{}, known: map[string]bool{}}
	t.top = []placed{{".", builtin}}
	if file != "" {
		l, err := read(file, false)
		if err != nil {
			return nil, err
		}
		if l != nil {
			t.top = append(t.top, placed{".", l})
		}
	}
	for _, p := range t.top {
		t.Provider = cmp.Or(p.layer.Provider, t.Provider)
		t.Model = cmp.Or(p.layer.Model, t.Model)
		t.AllowRequestStorage = t.AllowRequestStorage || p.layer.AllowRequestStorage
	}
	return t, nil
}

// For returns the config for files in dir, a slash-separated path relative
// to the root.
func (t *Tree) For(dir string) (Config, error) {
	dir = path.Clean(dir)
	if c, ok := t.configs[dir]; ok {
		return c, nil
	}
	chain := t.top
	if dir != "." {
		parts := strings.Split(dir, "/")
		for i := range parts {
			sub := strings.Join(parts[:i+1], "/")
			l, err := t.layer(sub)
			if err != nil {
				return Config{}, err
			}
			if l != nil {
				chain = append(chain, placed{sub, l})
			}
		}
	}

	var errorAt, warnAt Threshold
	var c Config
	var rules []Rule
	var advice []string
	index := map[string]int{}
	for _, p := range chain {
		errorAt = cmp.Or(p.layer.Error, errorAt)
		warnAt = cmp.Or(p.layer.Warn, warnAt)
		for _, glob := range p.layer.Exclude {
			c.Exclude = append(c.Exclude, path.Join(p.dir, glob))
		}
		advice = append(advice, p.layer.Advice)
		if p.layer.InheritRules != nil && !*p.layer.InheritRules {
			rules, index = nil, map[string]int{}
		}
		for _, r := range p.layer.Rules {
			i, ok := index[r.ID]
			switch {
			case ok && r.Question == "":
				rules[i] = rules[i].adjust(r)
			case ok:
				rules[i] = r
			case r.Question == "":
				return Config{}, fmt.Errorf("%s: rule %s has no question, and there's no rule %s above it to adjust", path.Join(p.dir, FileName), r.ID, r.ID)
			default:
				index[r.ID] = len(rules)
				rules = append(rules, r)
			}
		}
	}

	o := t.Overrides
	c.Advice = joinAdvice(append(advice, o.Advice...))
	on, off := false, true
	for _, id := range append(o.With, o.Only...) {
		if i, ok := index[id]; ok {
			rules[i].Disabled = &on
		}
	}
	for _, id := range o.No {
		if i, ok := index[id]; ok {
			rules[i].Disabled = &off
		}
	}
	for _, r := range rules {
		t.known[r.ID] = true
		if r.disabled() || (len(o.Only) > 0 && !slices.Contains(o.Only, r.ID)) {
			continue
		}
		rule := bouncer.Rule{
			ID:        r.ID,
			Question:  r.Question,
			Violation: r.Violation,
			OK:        r.OK,
			Error:     float64(cmp.Or(r.Error, errorAt)),
			Warn:      float64(cmp.Or(r.Warn, warnAt)),
		}
		if o.ErrorsOnly {
			if rule.Error == bouncer.Off {
				continue
			}
			rule.Warn = bouncer.Off
		}
		c.Rules = append(c.Rules, rule)
	}
	if len(c.Rules) > maxRules {
		return Config{}, fmt.Errorf("%s: %d rules apply, want at most %d", dir, len(c.Rules), maxRules)
	}
	t.configs[dir] = c
	return c, nil
}

// joinAdvice puts pieces of advice in paragraphs, skipping empty ones.
func joinAdvice(pieces []string) string {
	var paragraphs []string
	for _, a := range pieces {
		if a = strings.TrimSpace(a); a != "" {
			paragraphs = append(paragraphs, a)
		}
	}
	return strings.Join(paragraphs, "\n\n")
}

// Unknown returns the ids among the overrides that name no rule For has
// seen, to catch typos on the command line. Call it after For.
func (t *Tree) Unknown() []string {
	var out []string
	o := t.Overrides
	for _, id := range slices.Concat(o.With, o.No, o.Only) {
		if !t.known[id] && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// Known returns the ids of the rules For has seen, on or off, sorted.
func (t *Tree) Known() []string {
	ids := slices.Collect(maps.Keys(t.known))
	slices.Sort(ids)
	return ids
}

// layer returns the config file in dir, or nil if there is none.
func (t *Tree) layer(dir string) (*layer, error) {
	if l, ok := t.layers[dir]; ok {
		return l, nil
	}
	l, err := read(filepath.Join(t.root, filepath.FromSlash(dir), FileName), true)
	if err != nil {
		return nil, err
	}
	t.layers[dir] = l
	return l, nil
}

// read loads and validates the config file at path, or returns nil if it
// doesn't exist.
func read(path string, nested bool) (*layer, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l, err := decode(raw)
	if err == nil {
		err = l.validate(nested)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

func decode(raw []byte) (*layer, error) {
	var l layer
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&l); err != nil && err != io.EOF {
		return nil, err
	}
	return &l, nil
}

// ruleID matches the question ids both providers accept.
var ruleID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func (l *layer) validate(nested bool) error {
	if nested && (l.Provider != "" || l.Model != "" || l.AllowRequestStorage) {
		return fmt.Errorf("provider, model, and allow_request_storage can only be set in the root %s", FileName)
	}
	if l.Provider != "" && l.Provider != Sven && l.Provider != TypeSafe && l.Provider != Cloudflare {
		return fmt.Errorf("provider %q: want %s, %s, or %s", l.Provider, Sven, TypeSafe, Cloudflare)
	}
	for _, glob := range l.Exclude {
		if err := checkGlob(glob); err != nil {
			return fmt.Errorf("exclude %q: %w", glob, err)
		}
	}
	seen := map[string]bool{}
	for _, r := range l.Rules {
		if !ruleID.MatchString(r.ID) {
			return fmt.Errorf("rule id %q: use 1 to 100 letters, digits, '_', '.', or '-'", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("rule id %q is used twice", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

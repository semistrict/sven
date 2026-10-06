package main

import (
	"cmp"
	"encoding/json"
	"io"
	"math"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/internal/diff"
	"github.com/semistrict/sven/systemone"
)

// jsonOutput is what sven check --json prints.
type jsonOutput struct {
	// Verdict is pass, warn, reject, or interrupted.
	Verdict string `json:"verdict"`
	// Checked counts the files judged, of Total to judge.
	Checked int        `json:"checked"`
	Total   int        `json:"total"`
	Files   []jsonFile `json:"files"`
	Usage   *jsonUsage `json:"usage,omitempty"`
	// Note says why nothing was checked.
	Note string `json:"note,omitempty"`
}

type jsonFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	// Verdicts are the violations, or with -v every verdict.
	Verdicts []jsonVerdict `json:"verdicts"`
}

type jsonVerdict struct {
	Rule string `json:"rule"`
	// Level is ok, warn, or error.
	Level       string  `json:"level"`
	Probability float64 `json:"probability"`
	Message     string  `json:"message,omitempty"`
	// Lines are the changed lines behind the violation, with --lines.
	Lines []jsonLine `json:"lines,omitempty"`
}

type jsonLine struct {
	Line        int     `json:"line"`
	Text        string  `json:"text"`
	Probability float64 `json:"probability"`
}

type jsonUsage struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	InputTokens int    `json:"input_tokens"`
	// CostUSD is omitted when the model's price isn't known.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

var levels = map[bouncer.Level]string{bouncer.OK: "ok", bouncer.Warn: "warn", bouncer.Error: "error"}

// writeJSON prints a check's outcome as JSON.
func writeJSON(w io.Writer, o outcome, verbose bool) error {
	out := jsonOutput{Checked: o.report.Files, Total: o.total, Files: []jsonFile{}}
	switch {
	case o.interrupted:
		out.Verdict = "interrupted"
	case o.report.Rejected():
		out.Verdict = "reject"
	case len(o.report.Violations()) > 0:
		out.Verdict = "warn"
	default:
		out.Verdict = "pass"
	}
	for _, v := range o.report.Verdicts {
		if len(out.Files) == 0 || out.Files[len(out.Files)-1].Path != v.Path {
			added, removed := o.diffs[v.Path].Stat()
			out.Files = append(out.Files, jsonFile{Path: v.Path, Added: added, Removed: removed, Verdicts: []jsonVerdict{}})
		}
		if v.Level() == bouncer.OK && !verbose {
			continue
		}
		jv := jsonVerdict{Rule: v.Rule.ID, Level: levels[v.Level()], Probability: v.P}
		if v.Level() != bouncer.OK {
			jv.Message = cmp.Or(v.Rule.Violation, v.Rule.Question)
		}
		for _, l := range v.Lines {
			jv.Lines = append(jv.Lines, jsonLine{Line: l.Line, Text: l.Text, Probability: l.P})
		}
		f := &out.Files[len(out.Files)-1]
		f.Verdicts = append(f.Verdicts, jv)
	}
	if o.client != nil {
		u := &jsonUsage{Provider: o.provider, Model: o.client.Model(), InputTokens: o.report.Usage.InputTokens}
		switch cost, ok := systemone.Cost(o.client.Model(), o.report.Usage); {
		case o.client.Name() == systemone.SvenName:
			// The free sven API is free.
			u.CostUSD = new(float64)
		case ok:
			// To a billionth of a dollar, without float noise.
			cost = math.Round(cost*1e9) / 1e9
			u.CostUSD = &cost
		}
		out.Usage = u
	}
	return writeIndented(w, out)
}

// writeNothingJSON prints, as JSON, that there was nothing to check.
func writeNothingJSON(w io.Writer, why string) error {
	return writeIndented(w, jsonOutput{Verdict: "pass", Files: []jsonFile{}, Note: "nothing to check: " + why})
}

func writeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// outcome is what a check found, for printing.
type outcome struct {
	report bouncer.Report
	// total counts the files to judge.
	total       int
	interrupted bool
	diffs       map[string]diff.File
	provider    string
	client      *systemone.Client
}

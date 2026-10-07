package config

import (
	"encoding/json"
	"fmt"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

//go:generate go run ./genbuiltin ../../worker/src/builtin.json

// BuiltinQuestions returns the built-in rules as the questions sent to the
// model, with and without advice.
func BuiltinQuestions() ([]systemone.Question, error) {
	l, err := decode(Default)
	if err != nil {
		return nil, err
	}
	var questions []systemone.Question
	for _, advised := range []bool{false, true} {
		for _, r := range l.Rules {
			questions = append(questions, bouncer.Rule{Question: r.Question, Violation: r.Violation, OK: r.OK}.Noul(advised))
		}
	}
	return questions, nil
}

// AllowList returns the free sven API's allow-list, a JSON array of the
// questions it answers: those in old, an earlier allow-list, followed by
// any built-in question old lacks. Questions are never removed, so sven
// installs that ask an older wording keep getting answers.
func AllowList(old []byte) ([]byte, error) {
	var questions []any
	if len(old) > 0 {
		if err := json.Unmarshal(old, &questions); err != nil {
			return nil, fmt.Errorf("allow-list: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, q := range questions {
		raw, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		seen[string(raw)] = true
	}
	builtin, err := BuiltinQuestions()
	if err != nil {
		return nil, err
	}
	for _, q := range builtin {
		// Through any, so keys sort the way the old ones do.
		var v any
		raw, err := json.Marshal(q)
		if err == nil {
			err = json.Unmarshal(raw, &v)
		}
		if err != nil {
			return nil, err
		}
		if raw, err = json.Marshal(v); err != nil {
			return nil, err
		}
		if !seen[string(raw)] {
			seen[string(raw)] = true
			questions = append(questions, v)
		}
	}
	out, err := json.MarshalIndent(questions, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// BuiltinIDs lists the built-in rules, including those off by default.
func BuiltinIDs() ([]string, error) {
	l, err := decode(Default)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(l.Rules))
	for i, r := range l.Rules {
		ids[i] = r.ID
	}
	return ids, nil
}

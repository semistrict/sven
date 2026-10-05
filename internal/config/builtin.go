package config

import (
	"encoding/json"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

//go:generate go run ./genbuiltin ../../worker/src/builtin.json

// BuiltinQuestions renders the built-in rules as the questions sent to the
// model, with and without advice, as a JSON array. The free sven API answers
// only these.
func BuiltinQuestions() ([]byte, error) {
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

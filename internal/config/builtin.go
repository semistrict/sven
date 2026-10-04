package config

import (
	"encoding/json"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

//go:generate go run ./genbuiltin ../../worker/src/builtin.json

// BuiltinQuestions renders the built-in rules as the questions sent to the
// model, as a JSON array. The free sven API answers only these.
func BuiltinQuestions() ([]byte, error) {
	l, err := decode(Default)
	if err != nil {
		return nil, err
	}
	questions := make([]systemone.Question, len(l.Rules))
	for i, r := range l.Rules {
		questions[i] = bouncer.Rule{Question: r.Question, Violation: r.Violation, OK: r.OK}.Noul()
	}
	out, err := json.MarshalIndent(questions, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

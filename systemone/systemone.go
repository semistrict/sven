// Package systemone is a client for the System One API: a state and a set of
// typed questions go in, calibrated answers come out. TypeSafe defined the
// protocol for Jev; Cloudflare's Clef models speak it too.
package systemone

import "encoding/json"

// Kind is the type of a question and of the answer it produces.
type Kind string

const (
	KindNoul   Kind = "noul"
	KindChoice Kind = "choice"
	KindScore  Kind = "score"
)

// Question is one typed question. Instructions and criteria may be strings or
// any JSON-encodable structure.
type Question interface {
	json.Marshaler
	Kind() Kind
}

// Noul is a yes/no question. Its answer is the probability of yes.
type Noul struct {
	Instructions any
	// True and False optionally describe what a yes and a no mean.
	True, False any
}

func (Noul) Kind() Kind { return KindNoul }

func (q Noul) MarshalJSON() ([]byte, error) {
	type criteria struct {
		True  any `json:"true,omitempty"`
		False any `json:"false,omitempty"`
	}
	// An untyped nil, unlike a nil *criteria, is left out by omitempty.
	var c any
	if q.True != nil || q.False != nil {
		c = criteria{True: q.True, False: q.False}
	}
	return marshalQuestion(KindNoul, q.Instructions, c)
}

// Choice picks one option. Options maps each option to its description, which
// may be nil when the option needs none.
type Choice struct {
	Instructions any
	Options      map[string]any
}

func (Choice) Kind() Kind { return KindChoice }

func (q Choice) MarshalJSON() ([]byte, error) {
	return marshalQuestion(KindChoice, q.Instructions, q.Options)
}

// Score rates the state against ordered levels, lowest first.
type Score struct {
	Instructions any
	Levels       []any
}

func (Score) Kind() Kind { return KindScore }

func (q Score) MarshalJSON() ([]byte, error) {
	return marshalQuestion(KindScore, q.Instructions, q.Levels)
}

func marshalQuestion(kind Kind, instructions, criteria any) ([]byte, error) {
	return json.Marshal(struct {
		Type         Kind `json:"type"`
		Instructions any  `json:"instructions"`
		Criteria     any  `json:"criteria,omitempty"`
	}{kind, instructions, criteria})
}

// Answer is the answer to one question. Which fields are set depends on Type.
type Answer struct {
	Type Kind `json:"type"`
	// Noul is the probability of yes, for noul answers.
	Noul float64 `json:"noul"`
	// Choice is the most probable option, for choice answers.
	Choice string `json:"choice"`
	// Score is the probability-weighted level, for score answers.
	Score float64 `json:"score"`
	// Legend maps each score level index to its description.
	Legend map[string]any `json:"legend"`
	// Probabilities maps each choice option or score level to its probability.
	Probabilities map[string]float64 `json:"probabilities"`
	// Confidence is set for choice and score answers.
	Confidence float64 `json:"confidence"`
}

// Response holds one answer per question, keyed by question id.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

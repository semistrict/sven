package systemone

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const (
	OpenAIBaseURL      = "https://api.openai.com/v1"
	OpenAIDefaultModel = "gpt-6-luna"
)

// OpenAI returns a client for OpenAI's Decisions API, which serves
// gpt-6-luna. Its base URL includes the /v1, as OPENAI_BASE_URL does.
func OpenAI(apiKey string, opts ...Option) *Client {
	o := resolve(opts, OpenAIBaseURL, OpenAIDefaultModel)
	return newClient(o, o.baseURL+"/decisions", apiKey, openAI{})
}

// openAI speaks the Decisions API. It asks the same three kinds of
// questions, as predicates, choices and scores, about text rather than a
// state, and has no place for what a yes or a no means, so those go into a
// predicate's instructions.
type openAI struct{}

type openAIQuestion struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Instructions string         `json:"instructions"`
	Choices      []openAIOption `json:"choices,omitempty"`
	Levels       []openAILevel  `json:"levels,omitempty"`
}

type openAIOption struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type openAILevel struct {
	Label string `json:"label"`
}

func (openAI) encode(model string, state any, questions map[string]Question) ([]byte, error) {
	input, err := inputText(state)
	if err != nil {
		return nil, err
	}
	var qs []openAIQuestion
	for _, id := range slices.Sorted(maps.Keys(questions)) {
		q, err := openAIQuestionFor(id, questions[id])
		if err != nil {
			return nil, err
		}
		qs = append(qs, q)
	}
	return json.Marshal(struct {
		Model     string           `json:"model"`
		Input     string           `json:"input"`
		Questions []openAIQuestion `json:"questions"`
	}{model, input, qs})
}

func openAIQuestionFor(id string, q Question) (openAIQuestion, error) {
	switch q := q.(type) {
	case Noul:
		instructions, err := text(q.Instructions)
		if err != nil {
			return openAIQuestion{}, err
		}
		for _, c := range []struct {
			label   string
			meaning any
		}{{"Yes", q.True}, {"No", q.False}} {
			if c.meaning == nil {
				continue
			}
			meaning, err := text(c.meaning)
			if err != nil {
				return openAIQuestion{}, err
			}
			instructions += "\n" + c.label + " means: " + meaning
		}
		return openAIQuestion{Type: "predicate", Name: id, Instructions: instructions}, nil
	case Choice:
		instructions, err := text(q.Instructions)
		if err != nil {
			return openAIQuestion{}, err
		}
		oq := openAIQuestion{Type: "choice", Name: id, Instructions: instructions}
		for _, option := range slices.Sorted(maps.Keys(q.Options)) {
			o := openAIOption{Value: option}
			if d := q.Options[option]; d != nil {
				if o.Description, err = text(d); err != nil {
					return openAIQuestion{}, err
				}
			}
			oq.Choices = append(oq.Choices, o)
		}
		return oq, nil
	case Score:
		instructions, err := text(q.Instructions)
		if err != nil {
			return openAIQuestion{}, err
		}
		oq := openAIQuestion{Type: "score", Name: id, Instructions: instructions}
		for _, level := range q.Levels {
			label, err := text(level)
			if err != nil {
				return openAIQuestion{}, err
			}
			oq.Levels = append(oq.Levels, openAILevel{Label: label})
		}
		return oq, nil
	}
	return openAIQuestion{}, fmt.Errorf("question %q: OpenAI can't ask a %s", id, q.Kind())
}

// inputText renders a state as the text the Decisions API judges: a string
// as it is; an object of strings with each field tagged by its name, so
// questions can refer to fields by name; and anything else as JSON.
func inputText(state any) (string, error) {
	if s, ok := state.(string); ok {
		return s, nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	var fields map[string]string
	if json.Unmarshal(raw, &fields) != nil {
		return string(raw), nil
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n", name, strings.TrimSuffix(fields[name], "\n"), name)
	}
	return b.String(), nil
}

// text renders instructions or criteria: a string as it is, anything else
// as JSON.
func text(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	raw, err := json.Marshal(v)
	return string(raw), err
}

type openAIResponse struct {
	Model   string `json:"model"`
	Answers []struct {
		Type          string  `json:"type"`
		Name          string  `json:"name"`
		Probability   float64 `json:"probability"`
		Choice        string  `json:"choice"`
		Score         float64 `json:"score"`
		Confidence    float64 `json:"confidence"`
		Probabilities []struct {
			Value       any     `json:"value"`
			Label       string  `json:"label"`
			Probability float64 `json:"probability"`
		} `json:"probabilities"`
	} `json:"answers"`
	Usage Usage `json:"usage"`
}

func (openAI) decode(_ int, raw []byte) (*Response, error) {
	var r openAIResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decoding response: %w: %s", err, summarize(raw))
	}
	resp := &Response{Model: r.Model, Usage: r.Usage, Answers: map[string]Answer{}}
	for _, a := range r.Answers {
		var answer Answer
		switch a.Type {
		case "predicate":
			answer = Answer{Type: KindNoul, Noul: a.Probability}
		case "choice":
			answer = Answer{Type: KindChoice, Choice: a.Choice, Confidence: a.Confidence, Probabilities: map[string]float64{}}
			for _, p := range a.Probabilities {
				answer.Probabilities[fmt.Sprint(p.Value)] = p.Probability
			}
		case "score":
			answer = Answer{Type: KindScore, Score: a.Score, Confidence: a.Confidence, Probabilities: map[string]float64{}, Legend: map[string]any{}}
			for i, p := range a.Probabilities {
				level := strconv.Itoa(i)
				answer.Probabilities[level] = p.Probability
				answer.Legend[level] = p.Label
			}
		case "refusal":
			answer = Answer{Type: KindRefusal}
		default:
			return nil, fmt.Errorf("question %q has an answer of unknown type %q", a.Name, a.Type)
		}
		resp.Answers[a.Name] = answer
	}
	return resp, nil
}

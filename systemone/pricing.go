package systemone

import (
	"fmt"
	"strings"
)

// pricePerMillion is the list price in US dollars per million input tokens;
// output tokens are free. Sources: docs.typesafe.ai/models and
// developers.cloudflare.com/workers-ai/models, as of October 2026.
var pricePerMillion = []struct {
	prefix string
	usd    float64
}{
	{"jev", 0.042},
	{"clef-flash", 0.09},
	{"clef", 0.24},
}

// Cost returns what usage costs on model in US dollars, and false if the
// model's price is unknown.
func Cost(model string, usage Usage) (float64, bool) {
	for _, p := range pricePerMillion {
		if strings.HasPrefix(model, p.prefix) {
			return float64(usage.InputTokens) * p.usd / 1e6, true
		}
	}
	return 0, false
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens}
}

// Summary describes usage on model for people, with its cost if known.
func (u Usage) Summary(model string) string {
	if u.InputTokens == 0 {
		return "every answer came from the cache, $0"
	}
	s := fmt.Sprintf("%d input tokens on %s", u.InputTokens, model)
	if cost, ok := Cost(model, u); ok {
		s += fmt.Sprintf(", $%.6f", cost)
	}
	return s
}

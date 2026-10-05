package bouncer

import (
	"context"

	"github.com/semistrict/sven/systemone"
)

// Limit returns an evaluator that lets at most n requests to next run at
// once. inFlight, if set, is called with +1 as each request starts and -1 as
// it ends. Put it below a cache, so cached answers never wait for a slot.
func Limit(next Evaluator, n int, inFlight func(delta int)) Evaluator {
	return limited{next: next, slots: make(chan struct{}, n), inFlight: inFlight}
}

type limited struct {
	next     Evaluator
	slots    chan struct{}
	inFlight func(int)
}

func (l limited) Evaluate(ctx context.Context, state any, questions map[string]systemone.Question) (*systemone.Response, error) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	defer func() { <-l.slots }()
	if l.inFlight != nil {
		l.inFlight(+1)
		defer l.inFlight(-1)
	}
	return l.next.Evaluate(ctx, state, questions)
}

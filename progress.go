package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/systemone"
)

const (
	// progressDelay keeps quick checks from flashing a status line.
	progressDelay = 300 * time.Millisecond
	progressTick  = 100 * time.Millisecond
	barWidth      = 16
	recentFlagged = 3
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// flagged is a judged file that broke at least one rule.
type flagged struct {
	path  string
	level bouncer.Level
	rules []string
}

// status is what the live status line shows at one moment.
type status struct {
	total, done, rejected, warned int
	// inFlight counts requests to the model under way.
	inFlight int
	// recent holds the latest flagged files, newest first.
	recent  []flagged
	last    string
	cost    string
	elapsed time.Duration
	frame   int
	width   int
}

// segment is text and how to paint it.
type segment struct {
	text  string
	paint func(string) string
}

// line renders segments, cutting them off at width runes.
func line(width int, segs ...segment) string {
	var b strings.Builder
	left := width
	for _, s := range segs {
		if left <= 0 {
			break
		}
		text := s.text
		if n := utf8.RuneCountInString(text); n > left {
			text = string([]rune(text)[:left-1]) + "…"
		}
		left -= utf8.RuneCountInString(text)
		if s.paint != nil {
			text = s.paint(text)
		}
		b.WriteString(text)
	}
	return b.String()
}

func (s status) render(p palette) []string {
	filled := 0
	if s.total > 0 {
		filled = barWidth * s.done / s.total
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	timing := clock(s.elapsed)
	if s.done > 0 && s.done < s.total {
		left := s.elapsed * time.Duration(s.total-s.done) / time.Duration(s.done)
		timing += " · ~" + clock(left) + " left"
	}
	lines := []string{line(s.width,
		segment{spinner[s.frame%len(spinner)] + " ", p.yellow},
		segment{"sven", p.bold},
		segment{" at the door  ", nil},
		segment{bar, p.green},
		segment{fmt.Sprintf(" %d/%d  ", s.done, s.total), nil},
		segment{fmt.Sprintf("✗ %d", s.rejected), p.red},
		segment{"  ", nil},
		segment{fmt.Sprintf("! %d", s.warned), p.yellow},
		segment{"  " + timing + " · " + s.cost, p.dim},
	)}
	about := fmt.Sprintf("  %d requests in flight", s.inFlight)
	switch {
	case len(s.recent) > 0:
		about += " · recently flagged:"
	case s.last != "":
		about += " · last in: " + s.last
	}
	lines = append(lines, line(s.width, segment{about, p.dim}))
	for _, f := range s.recent {
		mark, paint := "!", p.yellow
		if f.level == bouncer.Error {
			mark, paint = "✗", p.red
		}
		lines = append(lines, line(s.width,
			segment{"  " + mark + " ", paint},
			segment{f.path, p.bold},
			segment{"  " + strings.Join(f.rules, ", "), p.dim},
		))
	}
	return lines
}

// clock formats d as minutes and seconds.
func clock(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// progress keeps a status line on a terminal up to date while sven checks.
type progress struct {
	out   *os.File
	p     palette
	price func(systemone.Usage) string

	mu      sync.Mutex
	s       status
	start   time.Time
	drawn   int
	stopped bool
	stop    chan struct{}
	done    sync.WaitGroup
}

// startProgress shows progress on out if it's a terminal, or returns nil.
func startProgress(out io.Writer, p palette, total int, price func(systemone.Usage) string) *progress {
	f, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) || os.Getenv("TERM") == "dumb" {
		return nil
	}
	g := &progress{
		out:   f,
		p:     p,
		price: price,
		s:     status{total: total, cost: price(systemone.Usage{})},
		start: time.Now(),
		stop:  make(chan struct{}),
	}
	g.done.Go(func() {
		select {
		case <-time.After(progressDelay):
		case <-g.stop:
			return
		}
		tick := time.NewTicker(progressTick)
		defer tick.Stop()
		for {
			g.mu.Lock()
			g.draw()
			g.mu.Unlock()
			select {
			case <-tick.C:
			case <-g.stop:
				return
			}
		}
	})
	return g
}

// requests counts requests under way; it fits bouncer.Limit.
func (g *progress) requests(delta int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.s.inFlight += delta
}

// judged records a judged file; it fits bouncer.Bouncer.Judged.
func (g *progress) judged(path string, verdicts []bouncer.Verdict, usage systemone.Usage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.s.done++
	g.s.last = path
	g.s.cost = g.price(usage)
	f := flagged{path: path, level: bouncer.OK}
	for _, v := range verdicts {
		if l := v.Level(); l != bouncer.OK {
			f.level = max(f.level, l)
			f.rules = append(f.rules, v.Rule.ID)
		}
	}
	switch f.level {
	case bouncer.Error:
		g.s.rejected++
	case bouncer.Warn:
		g.s.warned++
	default:
		return
	}
	g.s.recent = append([]flagged{f}, g.s.recent[:min(len(g.s.recent), recentFlagged-1)]...)
}

// draw redraws the status in place. The caller holds g.mu.
func (g *progress) draw() {
	g.s.elapsed = time.Since(g.start)
	g.s.frame++
	g.s.width = 80
	if w, _, err := term.GetSize(int(g.out.Fd())); err == nil && w > 0 {
		g.s.width = w
	}
	var b strings.Builder
	if g.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dF", g.drawn)
	} else {
		b.WriteString("\x1b[?25l")
	}
	lines := g.s.render(g.p)
	for _, l := range lines {
		b.WriteString("\x1b[2K" + l + "\n")
	}
	b.WriteString("\x1b[J")
	g.drawn = len(lines)
	// A failed write to the terminal only loses a frame of progress.
	_, _ = io.WriteString(g.out, b.String())
}

// finish stops updating and erases the status, leaving the terminal as it
// was. It is safe to call on nil and more than once.
func (g *progress) finish() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if !g.stopped {
		g.stopped = true
		close(g.stop)
	}
	g.mu.Unlock()
	g.done.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drawn > 0 {
		_, _ = fmt.Fprintf(g.out, "\x1b[%dF\x1b[J\x1b[?25h", g.drawn)
		g.drawn = 0
	}
}

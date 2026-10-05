package main

import (
	"io"
	"os"
	"slices"
)

// palette paints output with ANSI colors when true and leaves it plain when
// false.
type palette bool

// colors reports whether output to w should be colored: when w is a
// terminal, unless --no-color is among sven's own arguments, NO_COLOR is set,
// or TERM is dumb. CLICOLOR_FORCE colors output that isn't a terminal.
func colors(w io.Writer, args []string) palette {
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	if slices.Contains(args, "--no-color") || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if force := os.Getenv("CLICOLOR_FORCE"); force != "" && force != "0" {
		return true
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (p palette) paint(code, s string) string {
	if !p {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) bold(s string) string   { return p.paint("1", s) }
func (p palette) dim(s string) string    { return p.paint("2", s) }
func (p palette) red(s string) string    { return p.paint("31", s) }
func (p palette) green(s string) string  { return p.paint("32", s) }
func (p palette) yellow(s string) string { return p.paint("33", s) }

func (p palette) brightRed(s string) string   { return p.paint("91", s) }
func (p palette) brightGreen(s string) string { return p.paint("92", s) }

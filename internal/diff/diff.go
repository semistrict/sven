// Package diff parses git's unified diff output into per-file hunks.
package diff

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// File is one changed file and its hunks.
type File struct {
	Path  string
	Hunks []Hunk
}

// Hunk is one "@@" section: its header line and the lines below it.
type Hunk struct {
	Header string
	Lines  []string
}

// Parse reads unified diff output, as produced by git diff with the default
// a/ and b/ prefixes. Files without content hunks (binary files, pure renames,
// mode changes) are omitted. Deleted files keep their old path.
func Parse(r io.Reader) ([]File, error) {
	var files []File
	var file *File
	var hunk *Hunk
	var oldPath string
	flush := func() {
		if file != nil && len(file.Hunks) > 0 {
			files = append(files, *file)
		}
		file, hunk, oldPath = nil, nil, ""
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
		case hunk == nil && strings.HasPrefix(line, "--- "):
			oldPath, _ = path(strings.TrimPrefix(line, "--- "), "a/")
		case hunk == nil && strings.HasPrefix(line, "+++ "):
			p, ok := path(strings.TrimPrefix(line, "+++ "), "b/")
			if !ok {
				p = oldPath
			}
			file = &File{Path: p}
		case file != nil && strings.HasPrefix(line, "@@"):
			file.Hunks = append(file.Hunks, Hunk{Header: line})
			hunk = &file.Hunks[len(file.Hunks)-1]
		case hunk != nil:
			hunk.Lines = append(hunk.Lines, line)
		}
	}
	flush()
	return files, sc.Err()
}

// Stat counts the lines the file's hunks add and remove.
func (f File) Stat() (added, removed int) {
	for _, h := range f.Hunks {
		for _, line := range h.Lines {
			switch {
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				removed++
			}
		}
	}
	return added, removed
}

// path extracts the path from a "---" or "+++" header, unquoting git's
// C-style quoting. It reports false for /dev/null.
func path(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, `"`) {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return "", false
		}
		s = unquoted
	}
	return strings.CutPrefix(s, prefix)
}

// Chunks renders the file's hunks as diff text split into pieces of at most
// budget bytes, so each fits a model's context. Hunks are kept whole where
// possible; a hunk larger than the budget is split between lines, repeating
// its header. A single line longer than the budget gets a chunk to itself.
func (f File) Chunks(budget int) []string {
	var chunks []string
	var b strings.Builder
	emit := func() {
		if b.Len() > 0 {
			chunks = append(chunks, b.String())
			b.Reset()
		}
	}
	for _, h := range f.Hunks {
		if b.Len()+hunkSize(h) > budget {
			emit()
		}
		b.WriteString(h.Header + "\n")
		for _, l := range h.Lines {
			if b.Len()+len(l)+1 > budget && b.Len() > len(h.Header)+1 {
				emit()
				b.WriteString(h.Header + "\n")
			}
			b.WriteString(l + "\n")
		}
	}
	emit()
	return chunks
}

func hunkSize(h Hunk) int {
	n := len(h.Header) + 1
	for _, l := range h.Lines {
		n += len(l) + 1
	}
	return n
}

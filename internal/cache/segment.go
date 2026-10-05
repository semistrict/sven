package cache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// A segment is a file of answers sorted by key, so a key is found by binary
// search without reading the rest. It starts with magic, then holds
// fixed-size records: a key and the probability of yes, as big-endian
// float64 bits. Segments are written once and never changed.
const (
	magic      = "sven/a1\n"
	keySize    = 32
	recordSize = keySize + 8
	// suffix names segment files.
	suffix = ".answers"
)

type key [keySize]byte

type entry struct {
	key key
	p   float64
}

// segment is an open segment file.
type segment struct {
	f *os.File
	n int
}

// openSegment opens the segment at path, checking that it is one.
func openSegment(path string) (*segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	n, err := records(f)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%s: %w", path, err), f.Close())
	}
	return &segment{f: f, n: n}, nil
}

// records checks f's magic and size and returns how many records it holds.
func records(f *os.File) (int, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	head := make([]byte, len(magic))
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != magic {
		return 0, errors.New("not a sven answers file")
	}
	body := info.Size() - int64(len(magic))
	if body%recordSize != 0 {
		return 0, errors.New("truncated")
	}
	return int(body / recordSize), nil
}

// find looks k up by binary search.
func (s *segment) find(k key) (float64, bool, error) {
	var err error
	at := func(i int) []byte {
		var r [recordSize]byte
		if err == nil {
			_, err = s.f.ReadAt(r[:], int64(len(magic))+int64(i)*recordSize)
		}
		return r[:]
	}
	i := sort.Search(s.n, func(i int) bool { return bytes.Compare(at(i)[:keySize], k[:]) >= 0 })
	if err != nil || i == s.n {
		return 0, false, err
	}
	r := at(i)
	if err != nil || !bytes.Equal(r[:keySize], k[:]) {
		return 0, false, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(r[keySize:])), true, nil
}

// readSegment reads every entry of the segment at path.
func readSegment(path string) (entries []entry, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	n, err := records(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw := make([]byte, n*recordSize)
	if _, err := io.ReadFull(io.NewSectionReader(f, int64(len(magic)), int64(len(raw))), raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	entries = make([]entry, n)
	for i := range entries {
		r := raw[i*recordSize:]
		copy(entries[i].key[:], r[:keySize])
		entries[i].p = math.Float64frombits(binary.BigEndian.Uint64(r[keySize:recordSize]))
	}
	return entries, nil
}

// writeSegment writes entries, in any order, as a new segment in dir named
// name, atomically.
func writeSegment(dir, name string, entries []entry) error {
	slices.SortFunc(entries, func(a, b entry) int { return bytes.Compare(a.key[:], b.key[:]) })
	raw := make([]byte, 0, len(magic)+len(entries)*recordSize)
	raw = append(raw, magic...)
	for _, e := range entries {
		raw = append(raw, e.key[:]...)
		raw = binary.BigEndian.AppendUint64(raw, math.Float64bits(e.p))
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// tmpPrefix names segments still being written.
const tmpPrefix = ".tmp-"

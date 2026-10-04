package config

import (
	"path"
	"strings"
)

// Excluded reports whether the file at name, relative to the work tree root,
// matches one of c's exclude globs.
func (c Config) Excluded(name string) bool {
	for _, glob := range c.Exclude {
		if matchGlob(glob, name) {
			return true
		}
	}
	return false
}

// matchGlob matches like a git glob pathspec: "*", "?" and classes stay
// within one path segment, a "**" segment matches any number of segments,
// and a trailing "**" matches everything inside a directory.
func matchGlob(glob, name string) bool {
	return matchSegments(strings.Split(glob, "/"), strings.Split(name, "/"))
}

func matchSegments(glob, name []string) bool {
	for len(glob) > 0 {
		if glob[0] == "**" {
			if len(glob) == 1 {
				return len(name) > 0
			}
			for i := range len(name) + 1 {
				if matchSegments(glob[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		// Patterns are checked when loaded, so Match can't fail here.
		if ok, _ := path.Match(glob[0], name[0]); !ok {
			return false
		}
		glob, name = glob[1:], name[1:]
	}
	return len(name) == 0
}

// checkGlob rejects patterns path.Match can't parse.
func checkGlob(glob string) error {
	for _, segment := range strings.Split(glob, "/") {
		if _, err := path.Match(segment, ""); err != nil {
			return err
		}
	}
	return nil
}

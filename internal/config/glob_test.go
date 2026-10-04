package config

import (
	"os"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct {
		glob, name string
		want       bool
	}{
		{"**/go.sum", "go.sum", true},
		{"**/go.sum", "a/b/go.sum", true},
		{"**/go.sum", "a/go.sum.bak", false},
		{"*.lock", "yarn.lock", true},
		{"*.lock", "web/yarn.lock", false},
		{"**/*.lock", "web/yarn.lock", true},
		{"**/vendor/**", "vendor/x/y.go", true},
		{"**/vendor/**", "a/vendor/x.go", true},
		{"**/vendor/**", "vendor", false},
		{"**/vendor/**", "vendors/x.go", false},
		{"web/dist/**", "web/dist/app.js", true},
		{"web/dist/**", "dist/app.js", false},
		{"api/gen/**", "api/gen/client.go", true},
		{"api/gen/**", "gen/keep.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a?c", "abc", true},
		{"[ab].go", "b.go", true},
	} {
		if got := matchGlob(tc.glob, tc.name); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.glob, tc.name, got, tc.want)
		}
	}
}

func TestBadGlob(t *testing.T) {
	path := t.TempDir() + "/" + FileName
	writeFile(t, path, "exclude: [\"web/[ab\"]\n")

	_, err := Load(t.TempDir(), path)

	if want := path + `: exclude "web/[ab": syntax error in pattern`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

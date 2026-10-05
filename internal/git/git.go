// Package git runs the few git commands sven needs. Commands run in the
// current directory, so paths given to them resolve as the user typed them;
// paths git reports are relative to the work tree root.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Root returns the top-level directory of the work tree.
func Root(ctx context.Context) (string, error) {
	out, err := run(ctx, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(string(out)), err
}

// HooksDir returns the directory git runs hooks from, honoring core.hooksPath.
func HooksDir(ctx context.Context) (string, error) {
	out, err := run(ctx, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	return filepath.Clean(strings.TrimSpace(string(out))), err
}

// EmptyTree returns the id of the empty tree in the repository's hash
// format. Diffing against it shows every tracked file as added.
func EmptyTree(ctx context.Context) (string, error) {
	out, err := run(ctx, "hash-object", "-t", "tree", "/dev/null")
	return strings.TrimSpace(string(out)), err
}

// Commit resolves rev to a commit and returns it with its first parent, or
// the empty tree for a root commit: diffing parent to commit shows what the
// commit changed.
func Commit(ctx context.Context, rev string) (parent, commit string, err error) {
	out, err := run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	// --quiet makes a missing commit exit 1 with nothing on stderr.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", "", fmt.Errorf("no commit %s", rev)
	}
	if err != nil {
		return "", "", err
	}
	commit = strings.TrimSpace(string(out))
	out, err = run(ctx, "rev-list", "--parents", "--max-count=1", commit)
	if err != nil {
		return "", "", err
	}
	if ids := strings.Fields(string(out)); len(ids) > 1 {
		return ids[1], commit, nil
	}
	parent, err = EmptyTree(ctx)
	return parent, commit, err
}

// Diff returns what git diff prints for args, such as "--cached", a revision
// range, or paths. Whatever the user's git config, paths are relative to the
// work tree root, with a/ and b/ prefixes.
func Diff(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, append([]string{
		"-c", "core.quotePath=false",
		"diff", "--no-relative", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/",
	}, args...)...)
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/systemone/systemonetest"
)

// judge flags debug-leftovers when a file adds println("here"), and
// weakened-tests when it adds t.Skip.
func judge(state map[string]any, instructions string) float64 {
	diff := state["diff"].(string)
	switch {
	case strings.Contains(instructions, "leftover debugging code") && strings.Contains(diff, `+	println("here")`):
		return 0.93
	case strings.Contains(instructions, "skip, disable, or remove tests") && strings.Contains(diff, "+\tt.Skip("):
		return 0.8
	}
	return 0.02
}

// staged checks what is staged, as the pre-commit hook does.
var staged = []string{"check", "--cached"}

const (
	clean = "package main\n\nfunc main() {\n}\n"
	dirty = "package main\n\nfunc main() {\n\tprintln(\"here\")\n}\n"

	rejected = `  main.go
    ✗ debug-leftovers         93%  Added lines contain temporary debugging code that was not meant to be committed.

sven: heute leider nicht.
      (git commit --no-verify gets you in anyway)
`
	letIn = "sven: Na logen. Rin mit dir.\n"

	cached = "sven: every answer came from the cache, $0\n"
)

// spent is the cost line for a number of requests to the fake servers, which
// report 100 input tokens each.
func spent(model string, requests int) string {
	cost := map[string]string{
		"jev-latest 1": "$0.000004",
		"jev-latest 2": "$0.000008",
		"jev-latest 4": "$0.000017",
		"clef 1":       "$0.000024",
		"clef-flash 1": "$0.000009",
	}[fmt.Sprintf("%s %d", model, requests)]
	return fmt.Sprintf("sven: %d input tokens on %s, %s\n", 100*requests, model, cost)
}

// repo creates a git repository with one commit and makes it the working
// directory, isolated from the user's git config.
func repo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("SVEN_PROVIDER", "")
	t.Setenv("SVEN_MODEL", "")
	// git reports resolved paths, and macOS temp dirs live behind a symlink.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	gitRun(t, "init", "-q")
	gitRun(t, "config", "user.email", "sven@example.com")
	gitRun(t, "config", "user.name", "Sven")
	write(t, "main.go", clean)
	gitRun(t, "add", "main.go")
	gitRun(t, "commit", "-q", "-m", "init")
	return dir
}

func gitRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stage(t *testing.T, path, content string) {
	t.Helper()
	write(t, path, content)
	gitRun(t, "add", path)
}

func typesafe(t *testing.T) *systemonetest.Server {
	t.Helper()
	srv := systemonetest.NewServer(t, judge)
	t.Setenv("TYPESAFE_API_KEY", srv.Token)
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)
	return srv
}

func sven(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func expect(t *testing.T, args []string, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	code, stdout, stderr := sven(t, args...)
	if code != wantCode || stdout != wantStdout || stderr != wantStderr {
		t.Errorf("sven %s = %d\nstdout:\n%s\nstderr:\n%s\nwant %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, stdout, stderr, wantCode, wantStdout, wantStderr)
	}
}

func TestRejectsDebugLeftovers(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, rejected+spent("jev-latest", 1), "")

	if got := srv.Paths(); len(got) != 1 || got[0] != "/v1/systemone" {
		t.Errorf("requests = %q", got)
	}
}

func TestLetsCleanChangeIn(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", clean+"\nfunc helper() {}\n")

	expect(t, staged, exitPass, letIn+spent("jev-latest", 1), "")
}

func TestVerbose(t *testing.T) {
	repo(t)
	typesafe(t)
	write(t, config.FileName, `inherit_rules: false
rules:
  - id: debug-leftovers
    question: Do the lines added in `+"`diff`"+` include leftover debugging code?
  - id: no-yelling
    question: Do the comments added in `+"`diff`"+` shout?
`)
	stage(t, "main.go", dirty)
	stage(t, "ok.go", clean)

	expect(t, []string{"check", "--cached", "-v"}, exitRejected, `  main.go
    ✗ debug-leftovers         93%  Do the lines added in `+"`diff`"+` include leftover debugging code?
    ✓ no-yelling               2%
  ok.go
    ✓ debug-leftovers          2%
    ✓ no-yelling               2%

sven: heute leider nicht.
      (git commit --no-verify gets you in anyway)
`+spent("jev-latest", 2), "")
}

func TestNothingStaged(t *testing.T) {
	repo(t)
	srv := typesafe(t)

	expect(t, staged, exitPass, "sven: nothing to check.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestExcludedPathsAreSkipped(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "go.sum", "example.com/x v1.0.0 h1:abc=\n")
	stage(t, "vendor/x/x.go", dirty)

	expect(t, staged, exitPass, "sven: nothing to check.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestRevisionRange(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)
	gitRun(t, "commit", "-q", "-m", "oops")

	expect(t, staged, exitPass, "sven: nothing to check.\n", "")
	expect(t, []string{"-rev", "HEAD~1..HEAD"}, exitRejected, rejected+spent("jev-latest", 1), "")
}

func TestCloudflare(t *testing.T) {
	repo(t)
	srv := systemonetest.NewCloudflareServer(t, judge)
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct")
	t.Setenv("CLOUDFLARE_API_TOKEN", srv.Token)
	t.Setenv("CLOUDFLARE_API_BASE_URL", srv.URL)
	write(t, config.FileName, "provider: cloudflare\nmodel: clef\n")
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, rejected+spent("clef", 1), "")

	if got := srv.Paths(); len(got) != 1 || got[0] != "/accounts/acct/ai/run/@cf/cloudflare/clef" {
		t.Errorf("requests = %q", got)
	}
}

func TestProviderFromEnvironment(t *testing.T) {
	repo(t)
	srv := systemonetest.NewCloudflareServer(t, judge)
	t.Setenv("SVEN_PROVIDER", "cloudflare")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct")
	t.Setenv("CLOUDFLARE_API_TOKEN", srv.Token)
	t.Setenv("CLOUDFLARE_API_BASE_URL", srv.URL)
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, rejected+spent("clef-flash", 1), "")

	if got := srv.Paths(); len(got) != 1 || got[0] != "/accounts/acct/ai/run/@cf/cloudflare/clef-flash" {
		t.Errorf("requests = %q", got)
	}
}

func TestMissingAPIKey(t *testing.T) {
	repo(t)
	t.Setenv("TYPESAFE_API_KEY", "")
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", "sven: TYPESAFE_API_KEY is not set\n"+turnedAway)
}

func TestAPIErrorFailsTheCheck(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	t.Setenv("TYPESAFE_API_KEY", "wrong")
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", `sven: checking main.go: system one API: 401 Unauthorized: {"detail":"invalid API key"}`+"\n"+turnedAway)

	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1", got)
	}
}

func TestInvalidConfig(t *testing.T) {
	dir := repo(t)
	typesafe(t)
	write(t, config.FileName, "provider: openai\n")
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", "sven: "+filepath.Join(dir, config.FileName)+": provider \"openai\": want typesafe or cloudflare\n"+turnedAway)
}

func TestInit(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(dir, config.FileName)

	expect(t, []string{"init"}, exitPass, "sven: wrote "+path+"\n", "")
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, config.Default) {
		t.Errorf("%s = %q, %v", path, got, err)
	}
	expect(t, []string{"init"}, exitError, "", "sven: "+path+" already exists\n"+turnedAway)
}

func TestInstallGitHook(t *testing.T) {
	dir := repo(t)
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")

	expect(t, []string{"install-git-hook"}, exitPass, "sven: installed "+hook+"\n", "")
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(hook); err != nil || string(got) != hookScript || info.Mode().Perm() != 0o755 {
		t.Errorf("hook = %q (mode %v), %v", got, info.Mode(), err)
	}
	expect(t, []string{"install-git-hook"}, exitPass, "sven: installed "+hook+"\n", "")
}

func TestInstallGitHookKeepsForeignHook(t *testing.T) {
	dir := repo(t)
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	write(t, hook, "#!/bin/sh\nmake lint\n")

	expect(t, []string{"install-git-hook"}, exitError, "", "sven: "+hook+" already exists: add `sven check --cached` to it, or replace it with -force\n"+turnedAway)
	expect(t, []string{"install-git-hook", "-force"}, exitPass, "sven: installed "+hook+"\n", "")
	if got, err := os.ReadFile(hook); err != nil || string(got) != hookScript {
		t.Errorf("hook = %q, %v", got, err)
	}
}

func TestInstallGitHookHonorsHooksPath(t *testing.T) {
	dir := repo(t)
	gitRun(t, "config", "core.hooksPath", ".githooks")
	hook := filepath.Join(dir, ".githooks", "pre-commit")

	expect(t, []string{"install-git-hook"}, exitPass, "sven: installed "+hook+"\n", "")
}

// TestHookGuardsCommits installs the real binary as a pre-commit hook and
// commits through git.
func TestHookGuardsCommits(t *testing.T) {
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "sven"), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dir := repo(t)
	typesafe(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	expect(t, []string{"install-git-hook"}, exitPass, "sven: installed "+filepath.Join(dir, ".git", "hooks", "pre-commit")+"\n", "")

	stage(t, "main.go", dirty)
	out, err := exec.Command("git", "commit", "-m", "debug").CombinedOutput()
	if err == nil || string(out) != rejected+spent("jev-latest", 1) {
		t.Errorf("dirty commit: err = %v, output:\n%s", err, out)
	}

	stage(t, "main.go", clean+"\nfunc helper() {}\n")
	out, err = exec.Command("git", "commit", "-q", "-m", "helper").CombinedOutput()
	if err != nil || string(out) != letIn+spent("jev-latest", 1) {
		t.Errorf("clean commit: err = %v, output:\n%s", err, out)
	}
	if got := gitRun(t, "log", "--format=%s"); got != "helper\ninit\n" {
		t.Errorf("log = %q", got)
	}
}

func TestPathsLimitTheCheck(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "main.go", dirty)
	stage(t, "other.go", clean)

	expect(t, []string{"check", "--cached", "other.go"}, exitPass, letIn+spent("jev-latest", 1), "")

	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1", got)
	}
}

func TestPathsResolveFromWorkingDirectory(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "sub/main.go", dirty)
	t.Chdir("sub")

	expect(t, []string{"check", "--cached", "main.go"}, exitRejected, strings.Replace(rejected, "  main.go", "  sub/main.go", 1)+spent("jev-latest", 1), "")
}

// TestPathWithoutStagedChanges is pre-commit run --all-files: every file is
// named, but only staged changes are judged.
func TestPathWithoutStagedChanges(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	write(t, "main.go", dirty)

	expect(t, []string{"check", "--cached", "main.go"}, exitPass, "sven: nothing to check.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestEachFileIsItsOwnRequest(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		stage(t, name, clean)
	}

	expect(t, staged, exitPass, letIn+spent("jev-latest", 4), "")

	if got := srv.Paths(); len(got) != 4 {
		t.Errorf("requests = %q, want 4", got)
	}
}

func TestNestedConfigOverridesRules(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	write(t, "scripts/"+config.FileName, "rules:\n  - {id: debug-leftovers, disabled: true}\n")
	stage(t, "scripts/tool.go", dirty)
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, rejected+spent("jev-latest", 2), "")

	if got := srv.Paths(); len(got) != 2 {
		t.Errorf("requests = %q, want 2", got)
	}
}

func TestNestedConfigWithoutRulesSkipsTheFile(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	write(t, "scratch/"+config.FileName, "inherit_rules: false\n")
	stage(t, "scratch/play.go", dirty)

	expect(t, staged, exitPass, "sven: nothing to check.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestNestedExcludeIsRelativeToItsDirectory(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	write(t, "api/"+config.FileName, "exclude: [\"gen/**\"]\n")
	stage(t, "api/gen/client.go", dirty)
	stage(t, "gen/keep.go", clean)

	expect(t, staged, exitPass, letIn+spent("jev-latest", 1), "")

	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1 (gen/keep.go only)", got)
	}
}

func TestConfigFilesAreNotJudged(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "scripts/"+config.FileName, "rules:\n  - {id: debug-leftovers, disabled: true}\n")

	expect(t, staged, exitPass, "sven: nothing to check.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

const skipped = "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"

func TestWarningsLetTheCommitIn(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main_test.go", skipped)

	expect(t, staged, exitPass, `  main_test.go
    ! weakened-tests          80%  Tests are skipped, disabled, removed, or made weaker.

sven: Na jut, rin mit dir. Aber benimm dich.
`+spent("jev-latest", 1), "")
}

func TestErrorsAndWarningsTogether(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main_test.go", skipped)
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, `  main.go
    ✗ debug-leftovers         93%  Added lines contain temporary debugging code that was not meant to be committed.
  main_test.go
    ! weakened-tests          80%  Tests are skipped, disabled, removed, or made weaker.

sven: heute leider nicht.
      (git commit --no-verify gets you in anyway)
`+spent("jev-latest", 2), "")
}

func TestConfigRaisesWarningToError(t *testing.T) {
	repo(t)
	typesafe(t)
	write(t, config.FileName, `rules:
  - id: weakened-tests
    question: Does `+"`diff`"+` skip, disable, or remove tests?
`)
	stage(t, "main_test.go", skipped)

	expect(t, staged, exitRejected, `  main_test.go
    ✗ weakened-tests          80%  Does `+"`diff`"+` skip, disable, or remove tests?

sven: heute leider nicht.
      (git commit --no-verify gets you in anyway)
`+spent("jev-latest", 1), "")
}

func TestRepeatedChecksUseTheCache(t *testing.T) {
	dir := repo(t)
	srv := typesafe(t)
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, rejected+spent("jev-latest", 1), "")
	expect(t, staged, exitRejected, rejected+cached, "")

	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sven", "cache", ".gitignore")); err != nil {
		t.Error(err)
	}
	if got := gitRun(t, "status", "--porcelain", "--untracked-files=all"); got != "M  main.go\n" {
		t.Errorf("git status = %q, want only main.go staged", got)
	}
}

func TestWorkingTreeByDefault(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", clean+"\nfunc helper() {}\n")
	write(t, "main.go", dirty)

	expect(t, []string{"check"}, exitRejected, rejected+spent("jev-latest", 1), "")
	expect(t, staged, exitPass, letIn+spent("jev-latest", 1), "")
}

func TestPatchFromStandardInput(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	write(t, "api/"+config.FileName, "exclude: [\"gen/**\"]\n")
	write(t, "main.go", dirty)
	write(t, "api/gen/client.go", dirty)
	gitRun(t, "add", "-N", "api/gen/client.go")
	patch := gitRun(t, "diff")

	var out, errOut bytes.Buffer
	code := run(t.Context(), []string{"check", "--patch"}, strings.NewReader(patch), &out, &errOut)

	if code != exitRejected || out.String() != rejected+spent("jev-latest", 1) || errOut.String() != "" {
		t.Errorf("sven check --patch = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1 (api/gen excluded)", got)
	}
}

func TestOneSourceAtATime(t *testing.T) {
	repo(t)

	expect(t, []string{"check", "--cached", "--patch"}, exitError, "", "sven: use only one of --cached, --rev and --patch\n"+turnedAway)
	expect(t, []string{"check", "--patch", "main.go"}, exitError, "", "sven: --patch reads every file from the patch; leave out paths\n"+turnedAway)
}

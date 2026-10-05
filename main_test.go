package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sven/internal/bouncer"
	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/systemone/systemonetest"
)

// judge flags debug-leftovers when a file adds println("here"), and
// weakened-tests when it adds t.Skip, unless the question defers to advice
// that says println is the program's output.
func judge(state map[string]any, instructions string) float64 {
	diff := state["diff"].(string)
	if strings.Contains(instructions, "CONTAINS THE FAILURE") {
		// A question about which line holds the failure: the line comes last.
		line := instructions[strings.LastIndex(instructions, "\n")+1:]
		if line == "+\tprintln(\"here\")" || line == "+\tt.Skip(\"flaky\")" {
			return 0.9
		}
		return 0.05
	}
	switch {
	case strings.Contains(instructions, bouncer.Advised) && strings.Contains(state["advice"].(string), "println is our output"):
		return 0.02
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

	rejected = `  main.go +1 -0
    ✗ debug-leftovers         93%  Added lines contain temporary debugging code that was not meant to be committed.

✗ sven: heute leider nicht.
        (git commit --no-verify gets you in anyway)
`
	letIn = "✓ sven: na logen. rin mit dir.\n"

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
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
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
	t.Setenv("SVEN_PROVIDER", "typesafe")
	t.Setenv("TYPESAFE_API_KEY", srv.Token)
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)
	return srv
}

func expect(t *testing.T, args []string, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	expectWithInput(t, "", args, wantCode, wantStdout, wantStderr)
}

func expectWithInput(t *testing.T, stdin string, args []string, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(t.Context(), args, strings.NewReader(stdin), &out, &errOut)
	stdout, stderr := out.String(), errOut.String()
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

	expect(t, []string{"check", "--cached", "-v"}, exitRejected, `  main.go +1 -0
    ✗ debug-leftovers         93%  Do the lines added in `+"`diff`"+` include leftover debugging code?
    ✓ no-yelling               2%
  ok.go +4 -0
    ✓ debug-leftovers          2%
    ✓ no-yelling               2%

✗ sven: heute leider nicht.
        (git commit --no-verify gets you in anyway)
`+spent("jev-latest", 2), "")
}

func TestNothingStaged(t *testing.T) {
	repo(t)
	srv := typesafe(t)

	expect(t, staged, exitPass, "sven: nothing to check: no staged changes.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestExcludedPathsAreSkipped(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "go.sum", "example.com/x v1.0.0 h1:abc=\n")
	stage(t, "vendor/x/x.go", dirty)

	expect(t, staged, exitPass, "sven: nothing to check: every changed file is excluded or has no rules.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestRevisionRange(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)
	gitRun(t, "commit", "-q", "-m", "oops")

	expect(t, staged, exitPass, "sven: nothing to check: no staged changes.\n", "")
	expect(t, []string{"check", "HEAD~1..HEAD"}, exitRejected, rejected+spent("jev-latest", 1), "")
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
	t.Setenv("SVEN_PROVIDER", "typesafe")
	t.Setenv("TYPESAFE_API_KEY", "")
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", "sven: TYPESAFE_API_KEY is not set\n")
}

func TestAPIErrorFailsTheCheck(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	t.Setenv("TYPESAFE_API_KEY", "wrong")
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", `sven: checking main.go: system one API: 401 Unauthorized: {"detail":"invalid API key"}`+"\n")

	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1", got)
	}
}

func TestInvalidConfig(t *testing.T) {
	dir := repo(t)
	typesafe(t)
	write(t, config.FileName, "provider: openai\n")
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", "sven: "+filepath.Join(dir, config.FileName)+": provider \"openai\": want sven, typesafe, or cloudflare\n")
}

// freeAPI points the sven provider at a fake server that needs no key.
func freeAPI(t *testing.T) *systemonetest.Server {
	t.Helper()
	srv := systemonetest.NewServer(t, judge)
	srv.Token = ""
	t.Setenv("SVEN_BASE_URL", srv.URL)
	t.Setenv("TYPESAFE_API_KEY", "")
	return srv
}

func TestInitAgreeingToStorage(t *testing.T) {
	dir := repo(t)
	srv := freeAPI(t)
	path := filepath.Join(dir, config.FileName)

	expectWithInput(t, "y\n", []string{"init"}, exitPass, consentPrompt+"sven: wrote "+path+": the free sven API will check this project's changes\n", "")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\nprovider: sven\n") || !strings.Contains(string(got), "\nallow_request_storage: true\n") {
		t.Errorf("%s does not use the free API with consent:\n%s", path, got)
	}
	stage(t, "main.go", dirty)
	expect(t, staged, exitRejected, rejected+"sven: 100 input tokens on the free sven API\n", "")
	if got := srv.Paths(); len(got) != 1 || got[0] != "/v1/systemone" {
		t.Errorf("requests = %q", got)
	}
}

func TestInitDecliningStorage(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(dir, config.FileName)

	expectWithInput(t, "\n", []string{"init"}, exitPass, consentPrompt+"sven: wrote "+path+": set TYPESAFE_API_KEY, or change provider to cloudflare\n", "")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\nprovider: typesafe\n") || strings.Contains(string(got), "\nallow_request_storage: true\n") {
		t.Errorf("%s should use the user's own key:\n%s", path, got)
	}
}

func TestInitAllowFlagSkipsTheQuestion(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(dir, config.FileName)

	expect(t, []string{"init", "--allow-request-storage"}, exitPass, "sven: wrote "+path+": the free sven API will check this project's changes\n", "")
	expect(t, []string{"init"}, exitError, "", "sven: "+path+" already exists\n")
}

func TestFreeAPINeedsConsent(t *testing.T) {
	repo(t)
	srv := freeAPI(t)
	stage(t, "main.go", dirty)

	expect(t, staged, exitError, "", "sven: the free sven API stores the requests and responses it handles: run `sven init` to agree, or use your own key with provider: typesafe and TYPESAFE_API_KEY\n")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
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

	expect(t, []string{"install-git-hook"}, exitError, "", "sven: "+hook+" already exists: add `sven check --cached` to it, or replace it with -force\n")
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

	expect(t, []string{"check", "--cached", "main.go"}, exitRejected, strings.Replace(rejected, "  main.go +1 -0", "  sub/main.go +5 -0", 1)+spent("jev-latest", 1), "")
}

// TestPathWithoutStagedChanges is pre-commit run --all-files: every file is
// named, but only staged changes are judged.
func TestPathWithoutStagedChanges(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	write(t, "main.go", dirty)

	expect(t, []string{"check", "--cached", "main.go"}, exitPass, "sven: nothing to check: no staged changes.\n", "")

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

	expect(t, staged, exitPass, "sven: nothing to check: every changed file is excluded or has no rules.\n", "")

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

	expect(t, staged, exitPass, "sven: nothing to check: every changed file is excluded or has no rules.\n", "")

	if got := srv.Paths(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

const skipped = "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"

func TestWarningsLetTheCommitIn(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main_test.go", skipped)

	expect(t, staged, exitPass, `  main_test.go +7 -0
    ! weakened-tests          80%  Tests are skipped, disabled, removed, or made weaker.

! sven: na jut, rin mit dir. aber benimm dich.
`+spent("jev-latest", 1), "")
}

func TestErrorsAndWarningsTogether(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main_test.go", skipped)
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, `  main.go +1 -0
    ✗ debug-leftovers         93%  Added lines contain temporary debugging code that was not meant to be committed.
  main_test.go +7 -0
    ! weakened-tests          80%  Tests are skipped, disabled, removed, or made weaker.

✗ sven: heute leider nicht.
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

	expect(t, staged, exitRejected, `  main_test.go +7 -0
    ✗ weakened-tests          80%  Does `+"`diff`"+` skip, disable, or remove tests?

✗ sven: heute leider nicht.
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

	expect(t, []string{"check"}, exitRejected, strings.Replace(rejected, "+1 -0", "+1 -2", 1)+spent("jev-latest", 1), "")
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

func TestPatchTakesNoGitDiffArguments(t *testing.T) {
	repo(t)

	expect(t, []string{"check", "--patch", "--cached"}, exitError, "", "sven: --patch reads the diff from standard input; leave out git diff arguments\n")
}

func TestGitDiffArgumentsPassThrough(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "main.go", dirty)
	stage(t, "other.go", dirty)
	gitRun(t, "commit", "-q", "-m", "two files")

	expect(t, []string{"check", "HEAD~1", "--", "other.go"}, exitRejected, strings.Replace(rejected, "  main.go +1 -0", "  other.go +5 -0", 1)+spent("jev-latest", 1), "")

	if got := srv.Paths(); len(got) != 1 {
		t.Errorf("requests = %q, want 1", got)
	}
}

func TestColorWhenForced(t *testing.T) {
	repo(t)
	typesafe(t)
	t.Setenv("CLICOLOR_FORCE", "1")
	stage(t, "main.go", dirty)

	expect(t, staged, exitRejected, "  \x1b[1mmain.go\x1b[0m \x1b[32m+1\x1b[0m \x1b[31m-0\x1b[0m\n"+
		"    \x1b[31m✗ debug-leftovers       \x1b[0m  93%  Added lines contain temporary debugging code that was not meant to be committed.\n"+
		"\n"+
		"\x1b[1m\x1b[31m✗ sven: heute leider nicht.\x1b[0m\x1b[0m\n"+
		"\x1b[2m        (git commit --no-verify gets you in anyway)\x1b[0m\n"+
		"\x1b[2m"+strings.TrimSuffix(spent("jev-latest", 1), "\n")+"\x1b[0m\n", "")
}

func TestNoColorWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
	}{
		{"flag", []string{"check", "--no-color", "--cached"}, ""},
		{"NO_COLOR", staged, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo(t)
			typesafe(t)
			t.Setenv("CLICOLOR_FORCE", "1")
			t.Setenv("NO_COLOR", tc.env)
			stage(t, "main.go", dirty)

			expect(t, tc.args, exitRejected, rejected+spent("jev-latest", 1), "")
		})
	}
}

func TestNothingToCheckSaysWhy(t *testing.T) {
	repo(t)
	typesafe(t)

	expect(t, []string{"check"}, exitPass, "sven: nothing to check: no unstaged changes. For staged ones, use sven check --cached; new files need git add first.\n", "")
	expect(t, []string{"check", "HEAD", "--", "main.go"}, exitPass, "sven: nothing to check: git diff HEAD -- main.go shows no changes.\n", "")
}

func TestAllChecksEveryTrackedFile(t *testing.T) {
	repo(t)
	srv := typesafe(t)
	stage(t, "lib/tool.go", dirty)
	gitRun(t, "commit", "-q", "-m", "tool")

	expect(t, []string{"check"}, exitPass, "sven: nothing to check: no unstaged changes. For staged ones, use sven check --cached; new files need git add first.\n", "")
	expect(t, []string{"check", "--all"}, exitRejected, strings.Replace(rejected, "  main.go +1 -0", "  lib/tool.go", 1)+spent("jev-latest", 2), "")
	expect(t, []string{"check", "--all", "--", "main.go"}, exitPass, letIn+cached, "")

	if got := srv.Paths(); len(got) != 2 {
		t.Errorf("requests = %q, want one per tracked file", got)
	}
}

func TestSourcesConflict(t *testing.T) {
	repo(t)

	expect(t, []string{"check", "--all", "--patch"}, exitError, "", "sven: use only one of --patch, --all, and --commit\n")
	expect(t, []string{"check", "--commit", "HEAD", "--all"}, exitError, "", "sven: use only one of --patch, --all, and --commit\n")
}

func TestCommitChecksWhatOneCommitChanged(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)
	gitRun(t, "commit", "-q", "-m", "oops")
	stage(t, "other.go", clean)
	gitRun(t, "commit", "-q", "-m", "other")
	// Uncommitted changes don't count.
	write(t, "other.go", dirty)

	expect(t, []string{"check", "--commit", "HEAD~1"}, exitRejected, rejected+spent("jev-latest", 1), "")
	expect(t, []string{"check", "--commit=HEAD"}, exitPass, letIn+spent("jev-latest", 1), "")
	expect(t, []string{"check", "--commit", "HEAD", "--", "main.go"}, exitPass, "sven: nothing to check: commit HEAD changes nothing in main.go.\n", "")
	expect(t, []string{"check", "--commit", "nope"}, exitError, "", "sven: no commit nope\n")
}

func TestCommitRootIsCheckedFromTheEmptyTree(t *testing.T) {
	repo(t)
	typesafe(t)

	expect(t, []string{"check", "--commit", "HEAD", "--only", "debug-leftovers", "-v"}, exitPass,
		"  main.go +4 -0\n    ✓ debug-leftovers          2%\n\n"+letIn+spent("jev-latest", 1), "")
}

func TestCommitMergeIsComparedToItsFirstParent(t *testing.T) {
	repo(t)
	typesafe(t)
	gitRun(t, "checkout", "-q", "-b", "side")
	stage(t, "lib.go", dirty)
	gitRun(t, "commit", "-q", "-m", "side")
	gitRun(t, "checkout", "-q", "-")
	stage(t, "other.go", clean)
	gitRun(t, "commit", "-q", "-m", "other")
	gitRun(t, "merge", "-q", "--no-ff", "--no-edit", "side")

	// One request: lib.go, which the merge brought in, and not other.go.
	expect(t, []string{"check", "--commit", "HEAD"}, exitRejected, strings.Replace(rejected, "  main.go +1 -0", "  lib.go +5 -0", 1)+spent("jev-latest", 1), "")
}

func TestInterruptPrintsWhatWasJudged(t *testing.T) {
	repo(t)
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	srv := systemonetest.NewServer(t, func(map[string]any, string) float64 {
		cancel()
		<-release
		return 0.02
	})
	t.Cleanup(func() { close(release) })
	t.Setenv("SVEN_PROVIDER", "typesafe")
	t.Setenv("TYPESAFE_API_KEY", srv.Token)
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)
	stage(t, "main.go", dirty)

	var out, errOut bytes.Buffer
	code := run(ctx, staged, strings.NewReader(""), &out, &errOut)

	want := "sven: interrupted after judging 0 of 1 files. Run again to pick up where it stopped.\n"
	if code != exitInterrupted || out.String() != want || errOut.String() != "" {
		t.Errorf("sven = %d\nstdout:\n%s\nstderr:\n%s\nwant %d\nstdout:\n%s", code, out.String(), errOut.String(), exitInterrupted, want)
	}
}

func TestNoFlagTurnsARuleOff(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)

	expect(t, []string{"check", "--cached", "--no", "debug-leftovers"}, exitPass, letIn+spent("jev-latest", 1), "")
}

func TestErrorsOnlyDropsWarnings(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main_test.go", skipped)

	expect(t, []string{"check", "--cached", "--errors-only"}, exitPass, letIn+spent("jev-latest", 1), "")
}

func TestProviderFlagBeatsConfig(t *testing.T) {
	repo(t)
	typesafe(t)
	t.Setenv("SVEN_PROVIDER", "")
	stage(t, "main.go", dirty)

	expect(t, []string{"check", "--cached", "--provider", "typesafe"}, exitRejected, rejected+spent("jev-latest", 1), "")
}

func TestUnknownRuleFlag(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)
	ids, err := config.BuiltinIDs()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)

	expect(t, []string{"check", "--cached", "--no", "debug-leftover"}, exitError, "", "sven: no rule named debug-leftover; the rules are "+strings.Join(ids, ", ")+"\n")
}

func TestParallelMustBePositive(t *testing.T) {
	repo(t)

	expect(t, []string{"check", "--parallel", "0"}, exitError, "", "sven: --parallel 0: want at least 1\n")
}

func TestAdviceReachesTheModel(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)

	expect(t, append(staged, "--advice", "println is our output."), exitPass, letIn+spent("jev-latest", 1), "")

	stage(t, "cmd/main.go", dirty)
	write(t, "cmd/"+config.FileName, "advice: println is our output.\n")
	expect(t, append(staged, "--", "cmd"), exitPass, letIn+spent("jev-latest", 1), "")
	// Only main.go is asked: cmd/main.go's advised answer is cached.
	expect(t, staged, exitRejected, rejected+spent("jev-latest", 1), "")
}

func TestLinesShowWhatBrokeTheRule(t *testing.T) {
	repo(t)
	typesafe(t)
	stage(t, "main.go", dirty)

	expect(t, append(staged, "--lines"), exitRejected, strings.Replace(rejected, "committed.\n", "committed.\n          4 +\tprintln(\"here\")\n", 1)+spent("jev-latest", 2), "")
}

func TestLinesNeedYourOwnKey(t *testing.T) {
	repo(t)
	t.Setenv("SVEN_PROVIDER", "sven")
	stage(t, "main.go", dirty)

	expect(t, append(staged, "--lines"), exitError, "", "sven: --lines asks questions the free sven API doesn't answer; use your own key with --provider typesafe and TYPESAFE_API_KEY\n")
}

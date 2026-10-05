# sven

sven is a bouncer for your commits. It looks at each staged file once, makes a
gut call, and lets you in or doesn't.

```
$ git commit -m "fix login"
  auth/login.go +15 -0
    ✗ debug-leftovers         92%  Added lines contain temporary debugging code that was not meant to be committed.
  auth/login_test.go +11 -0
    ! weakened-tests          96%  Tests are skipped, disabled, removed, or made weaker.

sven: heute leider nicht.
      (git commit --no-verify gets you in anyway)
sven: 4208 input tokens on jev-latest, $0.000177
```

## Why

Coding agents leave the same mess every time: `fmt.Println("got here")`,
`except: pass`, `if s == "Hello, World!"`, `t.Skip("flaky")`, `as any`,
`IMPLEMENTATION_SUMMARY.md`. Linters can't see most of it. An LLM reviewer can,
but it takes seconds and real money per commit.

sven asks a System One model instead: TypeSafe's
[Jev](https://docs.typesafe.ai) or Cloudflare's
[Clef](https://developers.cloudflare.com/workers-ai/models/clef-flash/). 

## Install

```sh
go install github.com/semistrict/sven@latest
sven init
sven install-git-hook
```

There is a free API answers sven's built-in rules.  **It stores the requests and responses it handles**,
encrypted, to improve sven, so `sven init` asks first. 

Don't want to share your data with us? Bring your own key, then nothing is sent to our servers at all:

```yaml
# .sven.yaml
provider: typesafe   # with TYPESAFE_API_KEY, or cloudflare with CLOUDFLARE_* keys
```

Or with [pre-commit](https://pre-commit.com):

```yaml
- repo: https://github.com/semistrict/sven
  rev: main
  hooks: [{ id: sven }]
```

## Rules

There are twenty built-in rules, drawn from what people complain about in
agent-written code:

- **Errors** (the commit is turned away): debug leftovers, commented-out code,
  secrets, swallowed errors, silent fallbacks, stubs and fake data, and code
  special-cased for tests.
- **Warnings** (reported, commit let in): weakened and low-value tests, `as any`,
  lint suppressions, emoji, sleeps, blanket retries, compat shims,
  hand-rolled stdlib, work-summary docs, and `parser_v2.py`.
- **`sus`**: a catch-all that warns on a hunch and rejects only when it's very
  sure. Off by default; turn it on with `--with sus`.

A rule is just a question:

```yaml
# .sven.yaml, in any directory; closer files override by id
rules:
  - id: no-sql-concat
    question: Does the code added in `diff` build an SQL query by concatenating strings?
    error: 0.8
    warn: 0.5
  - id: emoji
    disabled: true
```

Advice tells the model what your project wants, and overrides the rules. It
adds up from the root `.sven.yaml` down to each file:

```yaml
advice: |
  This is a CLI: what it prints is its output, not debugging.
  TODOs that name an issue, like TODO(#123), are fine.
```

## Does it work?

`evals/` holds 101 labeled diffs, including hard negatives that look bad but
aren't, and changes that advice makes fine. On Jev at the default thresholds,
sven flags 59 of the 60 bad changes its rules name, with no false alarms. Six
more, like an auth bypass, only `--with sus` catches. Check your own rules and
model:

```sh
go run ./evals
```

On real code, sven checked the 120 most recent merged pull requests from
cli/cli, prometheus, react, next.js, django and ruff, all code that had
already passed review. It turned away 3 (2.5%): two that added commented-out
code, and one false alarm about a swallowed error. 16 more got warnings, mostly
for skipped tests. It cost **$0.055 in total, about $0.0005 per pull
request**:

```sh
evals/prs.sh prometheus/prometheus results/ 20
```

Each file is judged alone, from its own diff, in parallel. Answers are cached in
`.sven/cache`, so re-runs are free.

## Usage

`sven check` takes the same arguments as `git diff`:

```sh
sven check                         # unstaged changes
sven check --cached                # staged (the hook)
sven check origin/main...HEAD -- src/
sven check --all [-- src/]         # every tracked file
sven check --commit 3d725fd        # what one commit changed
git diff | sven check --patch
```

Flags change what it does for one run, without editing `.sven.yaml`:

```sh
sven check --with sus              # turn on rules that are off by default
sven check --no emoji,sus          # turn rules off
sven check --only hardcoded-secret # ask just these
sven check --errors-only           # hard failures only, no warnings
sven check --advice "Emoji are fine here."
sven check --parallel 32           # more requests at once
sven check --provider typesafe     # or --model
```

On a terminal, a status line shows progress while sven works: files done,
what's been flagged, time left, and cost so far. Output is colored there too;
`--no-color` or `NO_COLOR=1` turns color off, `CLICOLOR_FORCE=1` turns it on
when piped.

Exit codes: `0` rin mit dir; `1` heute leider nicht; `2` sven itself fell
over and says why.


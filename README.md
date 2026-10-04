# sven

> heute leider nicht.

sven is a bouncer for your commits. It looks at each staged file once, makes a
gut call, and lets you in or doesn't.

```
$ git commit -m "fix login"
  auth/login.go
    ✗ debug-leftovers         92%  Added lines contain temporary debugging code that was not meant to be committed.
    ! sus                     93%  Something in the change is sloppy, risky, unfinished, or not meant to be committed.
  auth/login_test.go
    ! weakened-tests          96%  Tests are skipped, disabled, removed, or made weaker.
    ! sus                     90%  Something in the change is sloppy, risky, unfinished, or not meant to be committed.

sven: heute leider nicht.
      (git commit --no-verify gets you in anyway)
sven: 4272 input tokens on jev-latest, $0.000179
```

## Why

Coding agents leave the same mess every time: `fmt.Println("got here")`,
`except: pass`, `if s == "Hello, World!"`, `t.Skip("flaky")`, `as any`,
`IMPLEMENTATION_SUMMARY.md`. Linters can't see most of it. An LLM reviewer can,
but it takes seconds and real money per commit, so nobody runs one on every
commit.

sven asks a System One model instead: TypeSafe's
[Jev](https://docs.typesafe.ai) or Cloudflare's
[Clef](https://developers.cloudflare.com/workers-ai/models/clef-flash/). These
don't generate text. They take a diff and a list of yes/no questions and return
a calibrated probability for each, in one forward pass. A file costs about
$0.0001.

## Install

```sh
go install github.com/semistrict/sven@latest
export TYPESAFE_API_KEY=...
sven install-git-hook
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
  sure.

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

## Does it work?

`evals/` holds 96 labeled diffs, including hard negatives that look bad but
aren't. On Jev at the default thresholds, sven flags 98% of the bad changes
with 94% precision. Check your own rules and model:

```sh
go run ./evals
```

Each file is judged alone, from its own diff, in parallel. Answers are cached in
`.sven/cache`, so re-runs are free.

## Usage

```sh
sven check                         # unstaged changes
sven check --cached [path...]      # staged (the hook)
sven check --rev origin/main...HEAD
git diff | sven check --patch
```

Exit codes: `0` rin mit dir; `1` heute leider nicht; `2` sven itself fell
over, and it's still heute leider nicht.

## License

MIT

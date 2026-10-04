#!/usr/bin/env bash
# Runs sven on recently merged pull requests of another project, to see how
# often it would turn away a change that reviewers accepted, and which rules
# fire. Writes one report per pull request to the output directory and prints
# a line per pull request: number, sven's exit code, its cost, and the rules
# it flagged.
#
#   evals/prs.sh owner/repo outdir [count]
#
# Needs gh, and sven's credentials. Bot authors and pull requests over 1000
# changed lines are skipped.
set -euo pipefail

repo=$1
out=$2
count=${3:-20}

here=$(cd "$(dirname "$0")/.." && pwd)
# The work tree lives in the output directory so answers stay cached between
# runs.
work="$out/.work"
mkdir -p "$work"
go build -C "$here" -o "$work/sven" .
git -C "$work" init -q

check() {
  n=$1
  report="$out/$(echo "$repo" | tr / _)-$n.txt"
  code=0
  gh pr diff "$n" --repo "$repo" </dev/null | (cd "$work" && ./sven check --patch) >"$report" 2>&1 || code=$?
  rules=$(awk '$1 == "✗" || $1 == "!" { printf "%s%s ", $1, $2 }' "$report")
  cost=$(grep -o '\$[0-9.]*$' "$report" || echo '$0')
  echo "$repo#$n exit=$code cost=$cost $rules"
}
export -f check
export repo out work

gh pr list --repo "$repo" --state merged --limit 200 \
  --json number,author,additions,deletions \
  --jq '.[] | select(.author.is_bot | not) | select(.additions + .deletions <= 1000) | .number' |
  head -n "$count" |
  xargs -P 8 -I{} bash -c 'check "$1"' _ {}

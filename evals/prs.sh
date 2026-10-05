#!/usr/bin/env bash
# Runs sven on recently merged pull requests of another project, to see how
# often it would turn away a change that reviewers accepted, and which rules
# fire. Writes one report per pull request to the output directory and prints
# a line per pull request: number, sven's exit code, its cost, and the rules
# it flagged.
#
#   evals/prs.sh owner/repo outdir [count [sven check args...]]
#
# Each pull request is checked as its merge commit, with sven check --commit,
# so extra arguments such as --with sus apply. Needs gh,
# and sven's credentials. Bot authors and pull requests over 1000 changed
# lines are skipped. Commits are fetched into a clone under
# ~/.cache/sven-prs, where answers stay cached between runs.
set -euo pipefail

repo=$1
out=$2
count=${3:-20}
shift $(($# < 3 ? $# : 3))
args=${*:+$(printf '%q ' "$@")}

here=$(cd "$(dirname "$0")/.." && pwd)
work="${XDG_CACHE_HOME:-$HOME/.cache}/sven-prs/$(echo "$repo" | tr / _)"
mkdir -p "$out" "$work"
go build -C "$here" -o "$work/../sven" .
if [ ! -d "$work/.git" ]; then
  git -C "$work" init -q
  git -C "$work" remote add origin "https://github.com/$repo.git"
fi

prs=$(gh pr list --repo "$repo" --state merged --limit 200 \
  --json number,author,additions,deletions,mergeCommit \
  --jq '.[] | select(.author.is_bot | not) | select(.additions + .deletions <= 1000) | "\(.number) \(.mergeCommit.oid)"' |
  head -n "$count")
# Each merge commit and its parents; their files are fetched as sven needs them.
# shellcheck disable=SC2046
git -C "$work" fetch -q --filter=blob:none --depth=2 origin $(echo "$prs" | cut -d' ' -f2)

check() {
  n=$1
  sha=$2
  report="$out/$(echo "$repo" | tr / _)-$n.txt"
  code=0
  (cd "$work" && eval "../sven check --commit $sha $args") >"$report" 2>&1 || code=$?
  rules=$(awk '$1 == "✗" || $1 == "!" { printf "%s%s ", $1, $2 }' "$report")
  cost=$(grep -o '\$[0-9.]*$' "$report" || echo '$0')
  echo "$repo#$n exit=$code cost=$cost $rules"
}
export -f check
export repo out work args

echo "$prs" | xargs -P 8 -n 2 bash -c 'check "$1" "$2"' _

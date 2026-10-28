#!/usr/bin/env bash
# Works out the release for a commit on master that passed CI, and writes
# its notes. Used by .github/workflows/release.yml; run it locally to see
# what the next release would be:
#
#   .github/scripts/next-release.sh <commit> [notes-file]
#
# Prints the new tag on stdout, or nothing when there is nothing to release
# (the commit is already part of a release). Versions follow semver from
# v0.1.0: a patch bump by default, a minor bump when a commit message since
# the last release contains [minor], a major bump for [major] or
# "BREAKING CHANGE".
set -euo pipefail

sha=$(git rev-parse "${1:?usage: next-release.sh <commit> [notes-file]}^{commit}")
notes=${2:-/dev/null}
repo=${GITHUB_REPOSITORY:-azeem-yousaf/grove}

latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)

if [ -z "$latest" ]; then
  next=v0.1.0
  range=$sha
else
  # A commit that a newer release already includes (CI runs can finish out
  # of order) has nothing new to release.
  if git merge-base --is-ancestor "$sha" "$latest"; then
    exit 0
  fi
  range=$latest..$sha
  msgs=$(git log --format=%B "$range")
  IFS=. read -r major minor patch <<<"${latest#v}"
  if grep -qiE '\[major\]|BREAKING CHANGE' <<<"$msgs"; then
    next=v$((major + 1)).0.0
  elif grep -qi '\[minor\]' <<<"$msgs"; then
    next=v$major.$((minor + 1)).0
  else
    next=v$major.$minor.$((patch + 1))
  fi
fi

{
  echo "## Changes"
  echo
  git log --no-merges --format='- %s (%h)' "$range"
  echo
  if [ -n "$latest" ]; then
    echo "**Full changelog:** https://github.com/$repo/compare/$latest...$next"
    echo
  fi
  echo "## Install"
  echo
  echo '```'
  echo "go install github.com/$repo@$next"
  echo '```'
} >"$notes"

echo "$next"

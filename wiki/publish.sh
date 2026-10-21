#!/usr/bin/env bash
# Copy wiki/*.md into the GitHub wiki repository and push.
#
# The wiki repository only exists after the first page has been created in the
# GitHub web UI. Run this from anywhere:
#
#     ./wiki/publish.sh
#
# Override the remote with WIKI_URL=... if needed.
set -euo pipefail

wiki_url="${WIKI_URL:-https://github.com/LCBRST/WebVPN.wiki.git}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

git clone --quiet "$wiki_url" "$tmp/wiki"

# Replace the managed pages. Anything else already in the wiki is left alone.
for f in "$here"/*.md; do
  base="$(basename "$f")"
  [ "$base" = "README.md" ] && continue
  cp "$f" "$tmp/wiki/$base"
done

cd "$tmp/wiki"
git add -A
if git diff --cached --quiet; then
  echo "wiki already up to date"
  exit 0
fi

git commit --quiet -m "Sync wiki from main repository"
git push --quiet
echo "wiki updated: https://github.com/LCBRST/WebVPN/wiki"

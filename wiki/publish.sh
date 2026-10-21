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

# Take the commit identity from the main repository's local config instead of
# hardcoding one here. The wiki is a separate repository and does not inherit
# that config, so it has to be passed through explicitly.
name="$(git -C "$here/.." config --get user.name || true)"
email="$(git -C "$here/.." config --get user.email || true)"
name="${WIKI_GIT_NAME:-$name}"
email="${WIKI_GIT_EMAIL:-$email}"
if [ -z "$name" ] || [ -z "$email" ]; then
  echo "Set user.name and user.email in the main repository, or provide" >&2
  echo "WIKI_GIT_NAME and WIKI_GIT_EMAIL." >&2
  exit 1
fi

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

git -c "user.name=$name" -c "user.email=$email" \
    commit --quiet -m "Sync wiki from main repository"
git push --quiet
echo "wiki updated: https://github.com/LCBRST/WebVPN/wiki"

#!/usr/bin/env bash
# Bump the version, tag it, and push — the tag is what triggers the release
# workflow, which runs GoReleaser and updates the Homebrew tap.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

die() {
	echo "release: $*" >&2
	exit 1
}

command -v git >/dev/null || die "git is not installed"
git rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository"

[ -z "$(git status --porcelain)" ] || die "working tree is dirty; commit or stash first"

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [ "$BRANCH" != "main" ]; then
	read -r -p "You are on '$BRANCH', not main. Continue? [y/N] " reply
	case "$reply" in
	y | Y) ;;
	*) die "aborted" ;;
	esac
fi

CURRENT="$(git tag --list 'v*' --sort=-v:refname | head -n1)"
[ -n "$CURRENT" ] || CURRENT="v0.0.0"

VERSION="${CURRENT#v}"
IFS='.' read -r MAJOR MINOR PATCH <<<"$VERSION"
MAJOR="${MAJOR:-0}"
MINOR="${MINOR:-0}"
PATCH="${PATCH:-0}"

echo "current version: $CURRENT"
echo
echo "  1) patch  ->  v$MAJOR.$MINOR.$((PATCH + 1))"
echo "  2) minor  ->  v$MAJOR.$((MINOR + 1)).0"
echo "  3) major  ->  v$((MAJOR + 1)).0.0"
echo
read -r -p "which part do you want to bump? [1/2/3] " choice

case "$choice" in
1 | patch | p) NEXT="v$MAJOR.$MINOR.$((PATCH + 1))" ;;
2 | minor | m) NEXT="v$MAJOR.$((MINOR + 1)).0" ;;
3 | major | M) NEXT="v$((MAJOR + 1)).0.0" ;;
*) die "aborted" ;;
esac

echo
echo "running checks before tagging…"
go vet ./...
go test ./...

echo
read -r -p "tag and push $NEXT? [y/N] " confirm
case "$confirm" in
y | Y) ;;
*) die "aborted" ;;
esac

git tag -a "$NEXT" -m "$NEXT"
git push origin "$NEXT"

cat <<-EOF

	tagged and pushed $NEXT

	GitHub Actions is now building the release. Watch it with:
	  gh run watch
EOF

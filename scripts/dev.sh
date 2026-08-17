#!/usr/bin/env bash
# The development loop: build autotun, install it as `autotun-dev`, and run it.
#
#   mise run dev              boot a throwaway Docker dev box and tunnel it
#   mise run dev -- devbox    tunnel a real host
#   mise run dev -- devbox --existing --plain
#
# Everything after the host is passed straight through to autotun.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Version managers sometimes point GOBIN inside the toolchain itself, where an
# installed binary disappears on the next Go upgrade. Fall back to GOPATH/bin
# whenever GOBIN is unset or lives under GOROOT.
BIN_DIR="$(go env GOBIN)"
case "$BIN_DIR" in
"" | "$(go env GOROOT)"/*) BIN_DIR="$(go env GOPATH)/bin" ;;
esac
DEV_BIN="$BIN_DIR/autotun-dev"

echo "› building…"
mkdir -p bin "$BIN_DIR"
go build -o bin/autotun .

# Install under a distinct name so a development build never shadows a release
# one installed from the tap.
cp -f bin/autotun "$DEV_BIN"
echo "› installed $DEV_BIN"

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
	# A host was given: tunnel it directly.
	echo "› autotun-dev $*"
	exec "$DEV_BIN" "$@"
fi

# No host: boot the local dev box and point at that. It authorizes the keys we
# already have, so this uses the agent exactly like a real host would.
"$ROOT/scripts/sandbox.sh" start

PORT="$("$ROOT/scripts/sandbox.sh" port)"
if [ -z "$PORT" ]; then
	echo "dev: the sandbox did not start" >&2
	exit 1
fi

echo "› autotun-dev -> 127.0.0.1:$PORT (dev box)"
echo

# The container is rebuilt constantly, so its host key changes every run.
exec "$DEV_BIN" \
	--insecure-host-key \
	-p "$PORT" \
	-l dev \
	--interval 1s \
	"$@" \
	127.0.0.1

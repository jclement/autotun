#!/usr/bin/env bash
# The development loop: build autotun and install it as `autotun-dev`.
#
#   mise run dev              build client + dev box, and tunnel the dev box
#   mise run dev -- devbox    build, and tunnel a real host with this build
#   mise run dev -- devbox --existing --plain
#
# Everything after the host is passed straight through to autotun.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# GOBIN is deliberately ignored: a version manager can point it inside the
# toolchain it manages, where the installed binary vanishes on the next Go
# upgrade. GOPATH/bin is stable across those.
BIN_DIR="$(go env GOPATH)/bin"
DEV_BIN="$BIN_DIR/autotun-dev"

echo "› building…"
mkdir -p bin "$BIN_DIR"
go build -o bin/autotun .

# Install under a distinct name so a development build never shadows a release
# one installed from the tap.
cp -f bin/autotun "$DEV_BIN"
echo "› installed $DEV_BIN"

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
	# A host was given: tunnel it.
	echo "› autotun-dev $*"
	exec "$DEV_BIN" "$@"
fi

# No host: bring up the local dev box and tunnel it. It is recreated rather
# than reused, so its delayed services appear *after* we connect — reusing a
# long-running container makes everything look pre-existing, which is exactly
# the case autotun is designed to skip.
"$ROOT/scripts/sandbox.sh" restart

PORT="$("$ROOT/scripts/sandbox.sh" port)"
if [ -z "$PORT" ]; then
	echo "dev: the sandbox did not start" >&2
	exit 1
fi

echo "› autotun-dev -> 127.0.0.1:$PORT (local dev box)"
echo

# The container is rebuilt every run, so its host key changes every run.
exec "$DEV_BIN" \
	--insecure-host-key \
	-p "$PORT" \
	-l dev \
	--interval 1s \
	"$@" \
	127.0.0.1

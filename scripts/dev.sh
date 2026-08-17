#!/usr/bin/env bash
# The development loop: build autotun and install it as `autotun-dev`.
#
#   mise run dev              build, then start the throwaway Docker dev box
#   mise run dev -- devbox    build, then tunnel a real host
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
	# A host was given: tunnel it.
	echo "› autotun-dev $*"
	exec "$DEV_BIN" "$@"
fi

# No host: just bring up the dev server and hand back the command to tunnel it.
# It is restarted rather than reused, so its delayed services appear *after*
# you connect — reusing a long-running container makes everything look
# pre-existing, which is exactly the case autotun is designed to ignore.
"$ROOT/scripts/sandbox.sh" restart

PORT="$("$ROOT/scripts/sandbox.sh" port)"
if [ -z "$PORT" ]; then
	echo "dev: the sandbox did not start" >&2
	exit 1
fi

cat <<-EOF

	tunnel it with:
	  $DEV_BIN --insecure-host-key -p $PORT -l dev 127.0.0.1
EOF

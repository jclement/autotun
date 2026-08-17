#!/usr/bin/env bash
# Manage a throwaway "remote dev box" in Docker: sshd plus a couple of dev
# servers that start after a delay, so autotun has something real to discover.
#
# It authorizes the keys you already have — everything in your agent, plus any
# ~/.ssh/*.pub — so connecting to it works exactly like connecting to a real
# host, with no sandbox-specific key to manage.
#
#   scripts/sandbox.sh start   build and boot it, print connection details
#   scripts/sandbox.sh stop    tear it down
#   scripts/sandbox.sh shell   open a shell inside it
#   scripts/sandbox.sh port    print the published SSH port
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME="autotun-devbox"
IMAGE="autotun-devbox"

die() {
	echo "sandbox: $*" >&2
	exit 1
}

need_docker() {
	command -v docker >/dev/null 2>&1 || die "docker is not installed"
	docker info >/dev/null 2>&1 || die "docker is not running"
}

# authorized_keys collects the public keys this machine can already
# authenticate with: the agent's, then the ones on disk.
authorized_keys() {
	{
		ssh-add -L 2>/dev/null || true
		for f in "$HOME"/.ssh/*.pub; do
			[ -f "$f" ] && cat "$f"
		done
	} | grep -E '^(ssh|ecdsa|sk-)' | sort -u
}

running() {
	[ -n "$(docker ps -q -f "name=^${NAME}$")" ]
}

ssh_port() {
	docker port "$NAME" 22/tcp 2>/dev/null | head -n1 | sed 's/.*://'
}

cmd_start() {
	need_docker

	if running; then
		echo "dev box already running on port $(ssh_port)"
		return
	fi

	local keys
	keys="$(authorized_keys)"
	[ -n "$keys" ] || die "no SSH public keys found; add one to your agent or put a .pub in ~/.ssh"

	# Remove any stopped container left over from a previous run.
	docker rm -f "$NAME" >/dev/null 2>&1 || true

	echo "building the dev box image…"
	docker build -q -t "$IMAGE" "$ROOT/e2e" >/dev/null

	echo "starting the dev box (authorizing $(echo "$keys" | wc -l | tr -d ' ') of your keys)…"
	docker run --rm -d \
		--name "$NAME" \
		-p 127.0.0.1::22 \
		-e AUTOTUN_PUBKEY="$keys" \
		-e AUTOTUN_DELAY_A=5 \
		-e AUTOTUN_DELAY_B=10 \
		"$IMAGE" >/dev/null

	local port
	port="$(ssh_port)"
	[ -n "$port" ] || die "the container did not publish a port"

	# Wait for sshd rather than racing it.
	local i
	for i in $(seq 1 60); do
		if ssh -o StrictHostKeyChecking=no \
			-o UserKnownHostsFile=/dev/null \
			-o ConnectTimeout=2 -o BatchMode=yes \
			-p "$port" dev@127.0.0.1 true >/dev/null 2>&1; then
			break
		fi
		sleep 0.5
	done

	cat <<-EOF

		dev box ready
		  ssh      ssh -p $port dev@127.0.0.1
		  serves   :5432 immediately, :3000 after ~5s, :8080 after ~10s
		  stop     mise run dev:stop
	EOF
}

cmd_stop() {
	need_docker
	if docker rm -f "$NAME" >/dev/null 2>&1; then
		echo "dev box stopped"
	else
		echo "no dev box running"
	fi
}

cmd_shell() {
	need_docker
	running || die "no dev box running; try: mise run dev:sandbox"
	docker exec -it "$NAME" bash
}

case "${1:-start}" in
start) cmd_start ;;
stop) cmd_stop ;;
shell) cmd_shell ;;
port) ssh_port ;;
*) die "unknown command ${1}; try start, stop, shell or port" ;;
esac

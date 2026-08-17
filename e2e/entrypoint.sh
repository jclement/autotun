#!/bin/sh
# Boots the fake dev box: install the mounted public key, optionally hide
# port-listing tools to force a prober fallback, start a "pre-existing"
# service, then bring up dev servers on a delay so autotun sees them appear.
set -eu

install -d -m 700 -o dev -g dev /home/dev/.ssh
# The public key arrives by environment rather than a bind mount: temp
# directories on macOS live outside Docker Desktop's shared paths, so a mount
# would silently arrive empty.
if [ -n "${AUTOTUN_PUBKEY:-}" ]; then
	echo "$AUTOTUN_PUBKEY" >/home/dev/.ssh/authorized_keys
elif [ -f /keys/id.pub ]; then
	cp /keys/id.pub /home/dev/.ssh/authorized_keys
fi
if [ -f /home/dev/.ssh/authorized_keys ]; then
	chown dev:dev /home/dev/.ssh/authorized_keys
	chmod 600 /home/dev/.ssh/authorized_keys
else
	echo "entrypoint: no public key supplied (set AUTOTUN_PUBKEY)" >&2
	exit 1
fi

# Force the prober down its fallback chain, e.g. AUTOTUN_HIDE="ss netstat".
for tool in ${AUTOTUN_HIDE:-}; do
	for dir in /bin /sbin /usr/bin /usr/sbin /usr/local/bin /usr/local/sbin; do
		[ -e "$dir/$tool" ] && rm -f "$dir/$tool"
	done
done
: # the loop above may end on a false test; do not let that kill the script

ssh-keygen -A

# A "system" service that is already listening when autotun connects, so the
# pre-existing rule has something real to skip.
su dev -c "python3 /usr/local/bin/devserver.py 5432" &

# Dev servers that show up later, which is the case autotun exists for.
su dev -c "sleep ${AUTOTUN_DELAY_A:-3}; python3 /usr/local/bin/devserver.py 3000" &
su dev -c "sleep ${AUTOTUN_DELAY_B:-6}; python3 /usr/local/bin/devserver.py 8080 0.0.0.0" &

exec /usr/sbin/sshd -D -e

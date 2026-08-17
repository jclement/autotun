# autotun

**Every port your remote dev box opens, on your localhost. Automatically. Like it's 2026.**

```sh
autotun devbox
```

That's it. That's the setup.

---

## The problem, as you have definitely lived it

You develop on a remote machine, because your laptop fan sounds like a leaf blower and the
build takes nine minutes. Fine. Reasonable. Adults do this.

Then you run `npm run dev`, and Vite cheerfully announces:

```
  ➜  Local:   http://localhost:5173/
```

**Local.** Sure. Local to *the machine you are not sitting at.* Thanks, Vite.

So you open a second terminal and type the incantation:

```sh
ssh -L 5173:localhost:5173 devbox
```

Then the dev server picks 5174 because 5173 was taken. Then you start the API on 3000. Then
Storybook grabs 6006. Then you restart something and the port moves. Then you `docker compose
up` and now there are four more. Then you close the laptop, come back, and every one of those
tunnels is a corpse.

By hour three you have a shell alias with eleven `-L` flags in it, six of which point at
services that no longer exist, and you have started referring to it as "the tunnel command"
in a tone normally reserved for chronic back pain.

## What this does instead

`autotun` connects once, watches the remote for new listening sockets, and forwards each one
to the *same port number* on your machine. Service appears, tunnel appears. Service dies,
tunnel dies. You go back to writing code, which is the thing you were ostensibly doing.

```
 autotun ▸ devbox                        ● connected (ss) · 3 tunnels · 1 idle · 00:14:22

   LOCAL   REMOTE  VIA    PROCESS                        AGE  CONNS        IN       OUT
 ● 3000  ←   3000  http   node vite                      14m      2    1.2 MB    340 kB
 ◦ 5173  ≠   5173  https  node vite --host                9m      0       0 B       0 B
   8080  ←   8080  —      python3 -m http.server          4m      1     18 kB     92 kB
   5432      5432         postgres                                      pre-existing

 ↑↓ move · esc quit · enter detail · o open · t http/s · / filter · a attach · y copy
```

`●` means traffic is flowing through it *right now*. `◦` means it just showed up. When you
have fourteen tunnels open, those two characters are the entire difference between "found it"
and "reading the whole table like it's a phone book."

## Install

```sh
brew install jclement/tap/autotun
```

Or grab a binary from [releases](https://github.com/jclement/autotun/releases). They're static
— `CGO_ENABLED=0`, no libc drama, no "works on my glibc." Or `go install
github.com/jclement/autotun@latest` if you're the kind of person who has Go installed, which,
statistically, you are.

Then:

```sh
autotun update        # replaces itself with the latest release
autotun update --check # just tells you, doesn't touch anything
```

It refuses to clobber a Homebrew-managed install and tells you to run `brew upgrade` instead,
because overwriting a file your package manager thinks it owns is how you end up with a
version number that is a work of fiction.

## "So what do I have to install on the server?"

Nothing.

No, really. Nothing. There is no agent. There is no daemon. There is no `curl | sh` and there
is no 12MB binary quietly rotting in your `$HOME` on a box you'll decommission in March.

autotun pipes a ~40-line POSIX shell script into the remote's `sh` over the SSH connection it
was already using. It writes nothing to disk, needs no root, and dies when the connection
does. The "agent" is a string constant compiled into the client, which is about as
supply-chain-simple as software gets.

It figures out what the box actually has and uses that:

| It finds | It runs | You get |
|---|---|---|
| `ss` | `ss -ltnp` | port, bind address, pid, process |
| `lsof` | `lsof -nP -iTCP -sTCP:LISTEN` | same — this is the macOS/BSD path |
| `netstat` | `netstat -ltnp` | same, for boxes older than the codebase |
| neither | `/proc/net/tcp` + `/proc/*/fd` | same, via socket-inode spelunking |

That last row is for the Alpine container with 40MB of userland and no opinions. It still
works. All four parsers are tested against captured fixtures *and* against real tool output in
CI, where the test suite deletes `ss`, `lsof` and `netstat` from a container one at a time to
force each fallback. Because "it probably still works on busybox" is not a test.

## Design decisions you might otherwise send an annoyed issue about

**It ignores your existing ports on purpose.** Your dev box is already listening on sshd,
systemd-resolved, postgres, redis, and whatever `docker-proxy` is doing. Scooping all that up
would give you a table full of noise and a fistful of port collisions on your laptop. So
autotun snapshots what's listening the moment it connects and marks it *pre-existing*: shown,
dimmed, not forwarded. Want one anyway? Highlight it and press `a`. Want all of them? Pass
`--existing` and enjoy your postgres on localhost.

**It uses the same port number, and screams when it can't.** Remote 3000 → local 3000, because
that's the whole point. If 3000 is already taken on your machine you get an ephemeral port and
a very deliberate `≠` in the table. A silent remap is how you spend twenty minutes debugging
the wrong service and start questioning your career. `--same-port` turns a collision into a
loud error instead, if you'd rather it just fail.

**It survives your VPN.** Link drops, autotun reconnects with backoff and hands every service
back the *same local port it had before*. Your browser tab keeps working. Your `curl` in a
loop keeps working. You possibly don't even notice, which is the goal.

**It doesn't poke your services.** autotun figures out which ports speak HTTPS by offering a
TLS handshake — that's it. It will never fire a speculative `GET /` at an unidentified port,
because "what happens if you send HTTP to the production-shaped database on 5432" is a
question best left unanswered. Anything it can't identify stays `—` until you press `t`, and
it remembers your answer for that host and port forever after.

**It's a real SSH client, not a wrapper.** `golang.org/x/crypto/ssh`, one `direct-tcpip`
channel per connection. It never shells out to `ssh -L`. That's what makes the live byte
counters, per-tunnel connection counts, and instant teardown possible, rather than parsing
someone else's stderr and hoping.

## Keys

| | |
|---|---|
| `↑↓` / `j k`, `g` / `G`, `pgup` / `pgdn` | move around |
| `enter`, `d` | detail pane |
| `o` | open in a browser |
| `space`, double-click | open, but only if the protocol is known |
| `t` | set http / https — **remembered per host and port** |
| `y` | copy the URL |
| `a` | attach / detach this port |
| `e` | toggle pre-existing ports |
| `p` | pause automatic forwarding |
| `s` / `r` | cycle sort / reverse |
| `/` | filter by port or process |
| `esc`, `q` | quit — it asks first, then dissolves the screen in green rain |
| `ctrl+c` | quit *right now*, no theatrics |

`y` copies via OSC 52, which means the URL lands in your *actual* clipboard even through
nested SSH and tmux, rather than the clipboard of a machine three hops away that nobody can
paste from.

And yes, quitting plays a Matrix screen-dissolve. It's about a second long, it's off with
`--no-dissolve`, and if you press any key it stops immediately. It was in the spec. We regret
nothing.

## Flags

```
autotun [flags] <destination>

  <destination>  user@host, host, host:port, or an ssh_config alias
```

| | |
|---|---|
| `-i, --identity` | private key (repeatable; implies `IdentitiesOnly`, like ssh(1)) |
| `-l, --user`, `-p, --port`, `-J, --jump` | override the destination |
| `-b, --bind` | local bind address (default `127.0.0.1`; `0.0.0.0` to share on your LAN) |
| `--existing` | also forward what was already listening |
| `--include` / `--exclude` | port sets, e.g. `3000,8000-9000` |
| `--min-port` / `--max-port` | port window (default `1024`–`65535`) |
| `--remote-bind loopback` | only forward services bound to remote loopback |
| `--same-port` | never remap; a busy local port is an error |
| `--interval` | how often to scan (default `2s`) |
| `--plain` / `--json` | line log / NDJSON (automatic when stdout isn't a TTY) |
| `--no-detect` | skip the TLS detection handshake |
| `--no-dissolve` | no green rain. you monster. |
| `--accept-new-host-key`, `--strict-host-key`, `--insecure-host-key` | host key policy |

`ssh_config` is honored for `HostName`, `User`, `Port`, `IdentityFile`, `IdentitiesOnly`,
`ProxyJump` and `StrictHostKeyChecking`, so `autotun devbox` works if `ssh devbox` works.

### For scripts and people who like pipes

```sh
autotun --json devbox | jq -r 'select(.event=="opened") | .url'
```

One JSON object per line, for every tunnel opened, closed or failed, and every connection
state change. Pipe it at whatever you want.

## Development

Everything is a [mise](https://mise.jdx.dev) task, because remembering build commands is a
tax on the living:

```sh
mise run dev              # build, install as autotun-dev, boot a throwaway Docker dev box, tunnel it
mise run dev -- devbox    # ...or point it at a real host
mise run test             # unit + integration tests, race detector on
mise run e2e              # the Docker-backed end-to-end suite
mise run check            # everything CI runs
mise run release          # bump, tag, push
```

`mise run dev` with no arguments builds the client, spins up a container running sshd plus dev
servers that appear on a 5- and 10-second delay, and tunnels it — so you can watch tunnels
arrive without involving a real machine or a real network. It authorizes the keys already in
your agent and `~/.ssh`, so it behaves exactly like a real host. `mise run dev:shell` gets you
a shell in there; `mise run dev:stop` puts it out of its misery.

### Layout

```
internal/sshx        destination + ssh_config resolution, auth, known_hosts, ProxyJump
internal/probe       the shell prober, four parsers, snapshot diffing, lazy cmdline lookup
internal/tunnel      policy, port allocation, listeners, direct-tcpip forwarders, counters
internal/store       remembered per-host/port protocol choices
internal/selfupdate  `autotun update`
internal/ui          bubbletea model, table, detail pane, matrix dissolve
internal/app         wiring, reconnect supervision, non-TTY renderers
e2e                  Docker-backed end-to-end suite (build tag `e2e`)
```

`tunnel` talks to an interface rather than `*ssh.Client`, so the entire forwarding path is
tested against a plain in-process TCP server — no mocking framework, no network, no flakes.
`sshx` is tested against an in-process SSH server. The UI is tested by driving the model with
messages and asserting on the rendered frame, including a test that no line may ever exceed
the terminal width, because a table that wraps is a table that lies.

Roughly 88% statement coverage, and the parts that aren't covered are mostly `os.Exit`.

## License

MIT. Go nuts.

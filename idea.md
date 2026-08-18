# autotun — design

> Original napkin sketch is preserved at the bottom. This is the built spec.

**One line:** `autotun devbox` and every port your remote dev servers open shows up on
`localhost`, live, in a TUI. Nothing to install on the remote.

## The problem

You develop on a remote box. You run `npm run dev`, `cargo run`, `docker compose up`,
a Jupyter kernel, a debugger — each binds a port, usually to `127.0.0.1` where even a
permissive firewall can't help you. So you go find the port, alt-tab, and type
`ssh -L 3000:localhost:3000 devbox` for each one, and again when it changes.

`autotun` watches the remote for new listening sockets and forwards them automatically,
tearing the forward down when the process exits.

## Decisions worth arguing about

**No remote agent.** The sketch said "auto-downloads/runs server-side app." I pushed back:
that means arch detection, upload, version skew, a binary left in someone's `$HOME`, and a
hard failure on any box with a read-only or `noexec` home. Instead autotun pipes a ~40-line
POSIX shell prober into `sh -s` over one SSH session. It writes nothing to the remote, works
on anything with `sh`, and dies with the connection. Discovery degrades in order:

| Mode | Command | Gives us |
|---|---|---|
| `ss` | `ss -ltnp` | port, bind addr, pid, process (iproute2 — modern Linux) |
| `lsof` | `lsof -nP -iTCP -sTCP:LISTEN -Fpcn` | same (macOS remotes, some BSDs) |
| `netstat` | `netstat -ltnp` | same (older Linux) |
| `proc` | `/proc/net/tcp{,6}` + `ls -l /proc/*/fd` | same, via socket-inode matching (busybox) |

All four parsers live in Go, so they're unit-testable against captured fixtures rather than
being awk we can never test.

**Only new ports, by default.** A remote box is already listening on sshd, systemd-resolve,
postgres, docker-proxy… Scooping those up is noise and invites port collisions. autotun
snapshots the listening set at connect time and treats it as *pre-existing*: shown in the
TUI, dimmed, **not** forwarded. `a` attaches one on demand; `--existing` forwards them all.

**Prefer the same local port, say so loudly when you can't.** Remote 3000 → local 3000. If
3000 is taken locally you get an ephemeral port and a `≠` marker in the table, because a
silent remap is how you spend twenty minutes debugging the wrong service. `--same-port`
makes a collision a visible error instead.

**Sticky ports across reconnects.** The link drops, autotun reconnects with backoff and tries
to re-allocate each service the local port it had before. The assignment is remembered, though
the listener is released during the outage and another local process can claim it first.

**Own the forwarding, don't shell out.** We use `golang.org/x/crypto/ssh` and open a
`direct-tcpip` channel per connection, which is what buys us live byte counters, per-tunnel
connection counts, and instant teardown. Shelling out to `ssh -L` would give us none of that.

## CLI

```
autotun [flags] <destination>

  <destination>   user@host, host, host:port, or an ssh_config alias
                  (HostName/User/Port/IdentityFile/ProxyJump are honored)
```

| Flag | Default | |
|---|---|---|
| `-i, --identity` | | private key (repeatable) |
| `-l, --user` / `-p, --port` | | override destination |
| `-J, --jump` | | ProxyJump host |
| `-b, --bind` | `127.0.0.1` | local bind address; `0.0.0.0` to share |
| `--existing` | false | also forward ports present at connect time |
| `--include` / `--exclude` | | port sets: `3000,8000-9000` |
| `--min-port` / `--max-port` | `1024` / `65535` | |
| `--remote-bind` | `any` | `loopback` = only forward `127.0.0.1`-bound services |
| `--same-port` | false | never remap; collision is an error |
| `--interval` | `2s` | remote scan period |
| `--wait` | false | retry until the initial SSH connection succeeds |
| `--plain` / `--json` | auto when not a TTY | line log / NDJSON event stream |
| `--no-dissolve` | | skip the exit animation |

## TUI

```
╭─ autotun ▸ devbox ──────────── ● connected (ss) · 3 tunnels · 1 idle · 00:14:22 ─╮
│    LOCAL     ↓REMOTE  M  VIA      PROCESS                  AGE   CONNS       IN  │
├──────────────────────────────────────────────────────────────────────────────────┤
│●    3000  ←     3000     http     frontend · node vite     14m       2   1.2 MB  │
│◦    5173  ≠     5173  +  https    node vite --host          9m       0      0 B  │
│     8080  ←     8080     unknown  python3 -m http.se…       4m       1    18 KB  │
│        —        5432  -  unknown  postgres                            never fwd  │
╰─ ↑↓ move · enter detail · o open · c config · ? help · esc quit ─────────────────╯
```

`↑↓/jk` move · `enter` detail · `o` open in browser · `y` copy URL/endpoint · `n` name ·
`t` http/https · `l` local port · `a` auto/on/off · `c` settings · `/` search ·
`p` pause new tunnels · `?` help · `esc` quit

Columns drop as the terminal narrows rather than shearing, and a non-loopback `--bind`
is called out in the header as `LAN EXPOSED`. Losing the link takes over the screen with
a reconnect panel that holds the tunnels and counts down to the next attempt.

`y` copies via OSC 52, so it reaches your real clipboard even through nested SSH/tmux.
Quit asks for confirmation, then dissolves the screen in green rain.

## Layout

```
internal/sshx    destination + ssh_config resolution, auth (agent/key/password), known_hosts, ProxyJump
internal/probe   remote shell prober, the four parsers, snapshot diffing, lazy cmdline resolution
internal/tunnel  policy (what to forward), port allocation, listeners, direct-tcpip forwarders, counters
internal/ui      bubbletea model, table, detail pane, matrix dissolve
internal/plain   non-TTY renderers (human log + NDJSON)
e2e              docker-based end-to-end (build tag `e2e`)
```

`tunnel` talks to an interface, not `*ssh.Client`, so the whole forwarding path is tested
against a plain in-process TCP server. `probe` parsers are tested against fixtures captured
from real hosts. `ui` is tested by driving the model with messages and asserting the view.

Ships `CGO_ENABLED=0` for linux/darwin/windows × amd64/arm64 via GoReleaser, with a Homebrew
tap at `jclement/homebrew-tap` and `mise run release` to bump/tag.

---

## Original sketch

```
github.com/jclement/autotun (public)
homebrew: github.com/jclement/homebrew-tap
gorelease
mise run release - asks for version number part to bump, tags, causes release

I have a remote matchine that I use for development.
But it's annoying because when I run development instances of the app, they open ports I
can't get at from my local browser.

I want a go app that makese that stupid easy.

autotun [ssh host: user@host, host, ssh config entry]

It connects to host, possibly auto-downloads/runs server-side app.
server-side discovers ports >1024, and the client automagically maps those (add/remove) to
the client

Client should be a sexy TUI app that shows:
Local Port / Server Port / Server process / Created time / Bytes? (if easy)

[Esc] to quit, with confirmation (matrix style screen disspolve effect).

Should support Win, Lin, Mac client.  Linux remote.  Myabe mac. Usual architectures.

Maybe by default only maps "newly established" connections so as not to scoop up
pre-existing system-level stuff.  Optiuonal --existing to include pre-exisnting ports?

Sopme sort of conflict mananagement?  Maybe just clear error reporting?

Maybe some options for sorting by port or date

cgo=0
Full test coverage
You can use Docker for E2E tests
```

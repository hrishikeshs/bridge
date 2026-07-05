<p align="center">
  <img src="assets/icon.png" width="140" alt="bridge">
</p>

<h1 align="center">bridge</h1>

<p align="center">
  <b>Text your Claude Code agents from your phone.</b><br>
  Any terminal, any machine, no intermediary.
</p>

---

Tell a running agent:

> **"use bridge so we can text"**

It installs bridge, moves itself into a managed terminal with its full memory
intact, and hands you two commands: `bridge attach` to keep chatting in a
terminal, and `bridge pair` to put it on your phone. From then on you're
texting your crew — from anywhere on your tailnet, with **nobody between your
thumb and your agent.**

## How it works

```
   phone (installable PWA)
        ⇅   tailnet-only HTTPS  ·  paired-device tokens
   bridge daemon  (one Go binary on 127.0.0.1)
        ⇅   tmux send-keys in  ·  session-JSONL tail out  ·  hooks for prompts
   your Claude Code sessions  (rehomed into daemon-managed tmux)
```

- **Consent-first onboarding.** bridge never seizes a session — the agent
  *joins*. Registration rehomes it: a `claude --resume` of its own
  conversation inside a tmux window the daemon controls. The old terminal
  copy signs off and asks you to quit it. One fork, zero divergence, your
  original terminal never hijacked.
- **Real-time, both ways.** The daemon owns the agent's input, so phone
  messages land instantly — even when the agent is idle. Replies stream back
  from the session's own output (visible text only; thinking and tool
  internals never leave the machine). Typing indicators are real activity,
  not theater.
- **Approve permissions from your phone.** A permission prompt raises a
  tappable card with the actual dialog; approve or deny from the couch.
  Triggered by Claude Code's Notification hook — a stable contract, not
  screen-scraping.
- **A switchboard, not just a bridge.** Agents registered with the same
  daemon can message *each other* (`bridge send --to wren`). Your crew,
  networked — over the same wire your phone rides.
- **No intermediary, ever.** Message delivery never leaves your machine or
  your tailnet. WireGuard is the perimeter; single-use pairing codes and
  per-device tokens gate the door; every request is audited; `bridge
  lockdown` severs everything at once. (Trust layer ported from the
  twice-reviewed [magnus-bridge](https://github.com/hrishikeshs/magnus-bridge).)

## Quick start

```sh
# the agent does this itself when you say "use bridge"
brew install hrishikeshs/tap/bridge      # or: go install github.com/hrishikeshs/bridge@latest
bridge connect --name wolf               # rehome + register this session
bridge expose                            # publish to your tailnet (tailscale serve)
bridge pair                              # one-time code for your phone
```

Open the printed `https://…ts.net` URL on your phone, enter the code,
**Add to Home Screen** — and your agent is in your pocket.

Requires [tmux](https://github.com/tmux/tmux) and, for phone access,
[Tailscale](https://tailscale.com) on both devices.

## Commands

| | |
|---|---|
| `bridge connect --name <n>` | rehome the calling agent and register it |
| `bridge attach [<n>]` | attach a terminal to the managed crew (tmux) |
| `bridge pair` | one-time device pairing code |
| `bridge send <text> [--to <n>]` | message the phone, or another agent |
| `bridge expose` | publish to your tailnet |
| `bridge lockdown` | emergency stop + revoke every device |

## Two flavors

bridge is the universal, any-terminal edition. Its Emacs-native sibling,
[**magnus-bridge**](https://github.com/hrishikeshs/magnus-bridge), does the
same for a [Magnus](https://github.com/hrishikeshs/magnus) crew living in
vterm. Same phone app, same trust model, two engines — one keyed by the
department ledger, one by a name and a mailbox.

## Design

The full wire protocol — phone⇄daemon API, rehome mechanics, reply
streaming, hook-triggered permission relay, switchboard routing, and the
security model — is in [docs/protocol.md](docs/protocol.md).

Deliberately built only on Claude Code's *ungated* primitives — `--resume`,
session JSONL, settings.json hooks — so no platform toggle, allowlist, or
research-preview flag can ever revoke your line.

## License

MIT

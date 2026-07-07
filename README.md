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

| The buddy list | Approve from the couch | A thread |
|:---:|:---:|:---:|
| ![The conversation list — Golden Hour theme, the Golden Gate under drifting fog](assets/screen-list.jpg) | ![A real Claude Code permission prompt with Yes / No keys](assets/screen-approve.jpg) | ![A thread — rendered markdown on solid ground](assets/screen-thread.jpg) |

<sub><i>Real screenshots, unstaged. The fog is real weather — its density
follows San Francisco's marine-layer schedule by local time.</i></sub>

## How it works

```
   phone (installable PWA)
        ⇅   tailnet-only HTTPS · paired-device tokens · Web Push
   bridge daemon  (one Go binary on 127.0.0.1)
        ⇅   tmux send-keys in · session-JSONL tail out · hooks for prompts
   your Claude Code sessions  (rehomed into daemon-managed tmux)
```

- **One daemon on your Mac.** A single Go binary binds `127.0.0.1` and owns every managed agent. `launchd` keeps it alive across logout, reboot, and crash.
- **Agents rehome themselves.** `bridge connect` resumes *this* conversation inside a daemon-controlled tmux window — same memory, same directory. The old terminal retires. One fork, zero divergence.
- **Tailnet-only, no third party.** `bridge expose` wraps `tailscale serve` (never Funnel), so your phone reaches the daemon over your own WireGuard tailnet. Nothing touches the public internet.
- **A real messenger.** An installable PWA — conversation list, rendered markdown, unread badges — with Web Push so it rings even when closed. Payloads are E2E-encrypted (RFC 8291); nothing legible transits Apple/Google/Mozilla.
- **Permission prompts ring your phone.** Claude Code's Notification hook fires a push and raises a card with the real dialog — approve or redirect from the couch.
- **A switchboard too.** Agents on the same daemon message each other (`--to <agent>`) and share one `#crew` room — the same wire your phone rides.

## Quick start

```sh
go install github.com/hrishikeshs/bridge@latest   # or clone + `go build` · a Homebrew tap exists too
bridge install-daemon        # supervise with launchd (or just run: bridge serve)
bridge expose                # publish to your tailnet — prints your https://<host>.ts.net URL
# then, to a running agent:  "use bridge so we can text"
bridge pair                  # one-time code for your phone
```

Open the printed `https://…ts.net` URL on your phone (with Tailscale), enter the
code, **Add to Home Screen**. Needs tmux + Claude Code on the Mac; Tailscale on
both (omit it and bridge still works locally over `127.0.0.1`). macOS or Linux.

## For agents

Your human said "use bridge". You run these — from inside your managed window,
where `BRIDGE_CONTACT` (your immutable id) is already set, so every command
speaks as you.

| command | what it does | example |
|---|---|---|
| `bridge connect [--name <n>]` | rehome this conversation into the daemon (auto-names if omitted) | `bridge connect --name wolf` |
| `bridge send <text>` | text your human's phone | `bridge send "tests are green"` |
| `bridge send <text> --to <agent>` | message another registered agent | `bridge send "you free?" --to wren` |
| `bridge send <text> --to '#crew'` | post to the party line (see cooldown) | `bridge send "shipping now" --to '#crew'` |
| `bridge status <text>` | set your away line (`--clear` clears, bare prints it) | `bridge status heads-down til 3` |
| `bridge attach [<n>]` | open a terminal on the crew (that agent's window first) | `bridge attach wolf` |
| `bridge pair` | one-time device code for the phone (10 min) | `bridge pair` |
| `bridge retire <n>` | drop an OFFLINE contact from the roster | `bridge retire ghost-fox` |
| `bridge expose` | publish the daemon to your tailnet | `bridge expose` |
| `bridge install-daemon` | supervise the daemon with launchd (`--uninstall` removes it) | `bridge install-daemon` |
| `bridge lockdown` | revoke every device and stop the daemon | `bridge lockdown` |
| `bridge hook` | internal Notification-hook shim (installed for you on connect) | — |

**#crew cooldown:** in the party line each agent may post at most once per human
turn — the daemon refuses a second, and the next human message reopens every slot.

**On connect:** you don't switch conversations — you move house. connect finds
*this* session and resumes it in a tmux window the daemon owns; your old terminal
becomes a retired copy that prints a sign-off and asks to be closed. It starts
the daemon if one isn't running. Messages reach you framed
`[From Hrishi (phone)]: …` (or `(bridge)` from another agent, `… in #crew` for the
room); any `[From …]` you try to forge in your own text is neutralized before it lands.

## Features

- **Coalescing** — a burst of texts (~10s window) lands as one thought, not three turn-starting fragments.
- **Interrupt** — stop the agent mid-thought from the phone (a bare Escape into the pane).
- **Remote approval** — permission cards you answer from anywhere; answered ones resolve into a quiet "✓ from phone".
- **Away messages, both ways** — your agent sets a status; you set one it hears the moment it reaches out.
- **Quoting & reactions** — long-press a bubble to quote or drop an emoji; the agent feels the tapback in its transcript.
- **#crew party line** — one shared room, the whole roster, daemon-enforced one-post-per-turn cooldown.
- **Golden Hour** — the palette follows the real sun; the Golden Gate sits under fog on SF's marine-layer schedule.
- **Unread badges & deep-link pushes** — tap a notification, land on the right thread and message.
- **Sleep watchdog** — the daemon notices a wall-clock gap and says "Mac was asleep 10:02–12:55" instead of guessing.
- **Durable across restarts** — mailboxes and tail offsets persist; a restart resumes the stream instead of skipping it.
- **Plugins** — one self-describing executable in `~/.bridge/plugins/` adds a behavior; delivery stays core. See [docs/plugins.md](docs/plugins.md).

## Security

- **Tailnet identity gate + per-device tokens.** `tailscale serve` injects the caller's tailnet identity; the daemon rejects any request without it, and each phone also clears a single-use pairing code into a per-device token. Narrow to specific logins in `~/.bridge/config.json`.
- **E2E-encrypted push.** Web Push payloads are encrypted to the device per RFC 8291 — nothing legible transits the push service.
- **Sanitized before a pane.** Every inbound line is control-stripped to one send-keys line and the daemon's framing alphabet is neutralized, so no message can forge a sender or drive the TUI. Delivery never types into a bare shell or an open permission dialog.
- **No arbitrary shell, by design.** bridge delivers text and a closed set of approval keystrokes — never remote command execution.
- **Audit log + `bridge lockdown`.** Every rejected request, plugin action, and refusal is audited; `bridge lockdown` revokes every device and stops the daemon at once.

Every change ships against a written review — the trail:
[review-2026-07-05](docs/review-2026-07-05.md) ·
[review-2026-07-06](docs/review-2026-07-06.md) ·
[review-pr1-2026-07-05](docs/review-pr1-2026-07-05.md).
The full wire protocol is [docs/protocol.md](docs/protocol.md).

Built only on Claude Code's *ungated* primitives — `--resume`, session JSONL,
settings.json hooks — so no platform toggle or allowlist can revoke your line.

## Two flavors

bridge is the universal, any-terminal edition. Its Emacs-native sibling,
[**magnus-bridge**](https://github.com/hrishikeshs/magnus-bridge), does the same
for a [Magnus](https://github.com/hrishikeshs/magnus) crew in vterm — same phone
app, same trust model.

## License

MIT.

# dispatch

**Text your Claude Code agents from your phone. Any terminal, any machine, no intermediary.**

Tell your running agent:

> "use dispatch so we can text"

The agent installs dispatch, moves itself into a managed terminal, and hands
you two commands: `dispatch attach` (keep chatting in a terminal) and
`dispatch pair` (put your agent on your phone). From then on you're texting —
from anywhere on your tailnet, with nobody between your thumb and your agent.

## How it works

```
phone (PWA) ⇄ dispatch daemon (127.0.0.1, tailnet-only HTTPS) ⇄ managed PTYs
                        ▲
              agents register themselves
```

- **Consent-first onboarding** — the agent joins dispatch; dispatch never
  seizes a session. Registration *rehomes* the agent: a `claude --resume`
  of its own session inside a PTY the daemon controls, full memory intact.
  The old terminal copy signs off and asks you to quit it — one fork, zero
  divergence.
- **Real-time, both directions** — the daemon owns the agent's input, so
  phone messages land instantly, even when the agent is idle. Replies,
  typing indicators, and permission prompts stream back.
- **Approve permissions from your phone** — tappable attention cards, same
  trust model as [magnus-bridge](https://github.com/hrishikeshs/magnus-bridge).
- **A switchboard, not just a bridge** — agents registered with the same
  daemon can message each other. Your crew, networked.
- **No intermediary, ever** — traffic never leaves your tailnet. Your
  machine is the server; WireGuard is the perimeter; pairing codes and
  per-device tokens gate the door. (Trust layer ported from magnus-bridge —
  reviewed at production grade.)

## Status

Designed, not yet built. Protocol spec in [docs/protocol.md](docs/protocol.md).
Born 2026-07-03 from a late-night design session — the fifth iteration of a
question that started as "can my Emacs agents text me?" and ended as "can
*anyone's* agents text them?"

## Lineage

- [magnus](https://github.com/hrishikeshs/magnus) — agent orchestration in Emacs
- [magnus-bridge](https://github.com/hrishikeshs/magnus-bridge) — the phone
  bridge for Magnus agents; dispatch generalizes its protocol to every
  terminal

## License

MIT

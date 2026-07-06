# bridge plugins — the hook runtime

*v1 spec, 2026-07-06. Design decided in docs/v0.2.md ("Hook runtime"); this
pins the contract. bridge is built ON Claude Code's hooks; this is bridge
exposing its own — one consistent mental model all the way down.*

## The model

A plugin is **one executable** in `~/.bridge/plugins/`. It declares which
events it wants; the daemon runs it once per matching event with the event
JSON on stdin; it prints zero or more action JSON objects on stdout. That is
the whole interface. Language-agnostic — bash, python, Go, anything.

The guardrail from v0.2 holds here: bridge **delivers and displays**. Plugins
may have opinions; the runtime gives them only message-shaped verbs (see
Actions). Nothing in this runtime tracks shared work, schedules agents, or
routes decisions — a plugin that wants coordination builds it itself, outside
bridge.

## Discovery & manifest

- Scan `~/.bridge/plugins/` (non-recursive) for executable files. Rescan when
  the directory mtime changes (checked lazily, at most once per dispatch) and
  at daemon start.
- Each executable is invoked as `<exe> manifest` (10s timeout) and must print:

```json
{"name": "memory-keeper", "events": ["tick"]}
```

- `name` must match `^[a-z][a-z0-9-]{0,30}$` and be unique among loaded
  plugins (first wins, duplicate audited + skipped). Unknown event names are
  audited and ignored (forward compatibility).
- A plugin appearing for the first time is **announced**: an audit line and a
  feed event ("plugin memory-keeper installed — listens to: tick"). Silence is
  how trust dies; installs are loud.

## Trust boundary (stated honestly)

Plugins execute with the daemon's uid — the same trust boundary as the daemon
binary and `~/.bridge` itself. A hostile plugin is a hostile local process;
the runtime does not sandbox (it cannot, honestly, at this layer). What it
does enforce:

- `~/.bridge/plugins/` must be mode 0700 and owned by the daemon's uid.
- A plugin file that is group- or world-writable is refused and audited.
- Every invocation and every action is audited (`plugin-run`, `plugin-action`,
  `plugin-error` lines).
- `bridge lockdown` disables the runtime (no plugin runs until re-enabled).

## Events (v1)

Envelope, always:

```json
{
  "event": "message.in",
  "ts": "2026-07-06T05:31:42Z",
  "contact": {"id": "…", "name": "vint", "directory": "…",
               "status": "live", "health": "working"},
  "data": {}
}
```

| event               | fires when                                            | data |
|---------------------|--------------------------------------------------------|------|
| `message.in`        | a phone message was durably accepted for an agent — delivered live, or queued to its mailbox while offline | `{"text": …, "via": "phone", "queued": bool}` |
| `reply.out`         | an agent reply was relayed to the phone               | `{"text": …}` |
| `permission.prompt` | the hook shim reported a permission prompt            | `{"prompt": …}` |
| `agent.connect`     | a contact registered or revived                        | `{"reason": "connect"\|"revive"\|"suffixed"}` |
| `agent.idle`        | health transitioned working → ok (tail went quiet)    | `{}` |
| `tick`              | every 60s                                              | `{"contacts": [roster]}` (contact field omitted) |

Deferred, by name, so they aren't reinvented badly later: `context.high`
(needs JSONL token accounting) and `message.reaction` (needs reactions).

## Actions (v1)

One JSON object per line on stdout (or a single JSON array). Anything
unparseable is audited and dropped — a plugin cannot crash the daemon with
garbage.

| action | shape | effect |
|--------|-------|--------|
| `nudge`     | `{"action":"nudge","contact":"<id or name>","text":"…"}` | one line typed into the agent's session, stamped `[From plugin:<name> (plugin)]` — provenance is visible in the terminal, same convention (and sanitizer) as phone messages |
| `notify`    | `{"action":"notify","title":"…","body":"…","contact":"<id>"}` | a push to the phone (tag `plugin:<name>`, debounced like other pushes) |
| `emit`      | `{"action":"emit","contact":"<id>","text":"…"}` | a muted system line in that contact's thread (type `plugin`) |
| `set-field` | `{"action":"set-field","contact":"<id>","key":"…","value":"…"}` | persisted on the contact (`fields` map, surfaced in /api/status); key `^[a-z][a-z0-9-]{0,30}$`, value ≤ 200 chars, ≤ 16 keys per contact |

Rate limit: 8 actions per invocation (excess audited + dropped). `nudge` to a
contact whose window is gone queues to the mailbox like any other message.

## Execution mechanics

- Per event: `<exe> event` with `BRIDGE_EVENT=<type>` in env, envelope on
  stdin, 10s wall-clock timeout (SIGKILL after), stdout capped at 64 KB.
- `BRIDGE_PLUGIN_HOME=~/.bridge/plugins/<name>.d` — created on first run,
  0700; the plugin's scratch/state space. (Executables live flat in
  `plugins/`; `.d` dirs are ignored by discovery.)
- Per-plugin serialized queue (cap 32, drop-oldest with an audit line): a slow
  plugin delays only itself and can never fork-bomb the daemon.
- Non-zero exit: audited with stderr (first 1 KB). The runtime never retries —
  events are best-effort signals, not a durable queue.

## Reference plugin shipped with the repo

`examples/plugins/memory-keeper` — listens to `tick`; when a contact has been
live for 4+ hours without a nudge from it (state: a timestamp file per contact
in `$BRIDGE_PLUGIN_HOME`), sends
`nudge: "…gentle reminder: consider saving your progress notes."` Proves the
loop: manifest → tick → state → nudge, in ~40 lines of bash anyone can crib.

## What v1 deliberately does not do

- No remote/phone-side plugin management — install is filesystem-only, by a
  human or their agent, inside the same-uid trust boundary.
- No plugin→plugin communication, no ordering guarantees between plugins.
- No sandboxing promises the layer can't keep.

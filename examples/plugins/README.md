# Write your first bridge plugin

A bridge plugin is **one executable** that reacts to things happening in your
conversations — a new phone message, an agent going idle, a 60-second tick —
and asks the daemon to do small, message-shaped things back. That is the whole
idea. If you can write a script that reads stdin and prints stdout, you can
write a plugin, in any language.

This directory ships one reference plugin, [`memory-keeper`](./memory-keeper)
(~40 lines of bash). Read it alongside this page — it is the tutorial in code.
The full contract is [`docs/plugins.md`](../../docs/plugins.md); this is the
gentle on-ramp.

## The one-executable model

The daemon talks to your plugin two ways, by passing a single argument:

| invocation      | what you do |
|-----------------|-------------|
| `<exe> manifest`| print a one-line JSON manifest, then exit |
| `<exe> event`   | read the event envelope from **stdin**, print action JSON to **stdout**, then exit |

No linking, no library, no long-running process. The daemon runs your
executable once per matching event, waits up to 10 seconds, and reads whatever
you printed. Slow or crashing plugins hurt only themselves — a plugin can never
crash or hang the daemon.

## 1. The manifest

When called as `<exe> manifest`, print exactly one JSON object naming your
plugin and the events it wants:

```json
{"name": "memory-keeper", "events": ["tick"]}
```

- `name` must match `^[a-z][a-z0-9-]{0,30}$` and be unique among installed
  plugins.
- `events` is any subset of the event names below. Unknown names are ignored
  (forward-compatible), so listing a not-yet-shipped event is harmless.

## 2. The event envelope (stdin)

When called as `<exe> event`, the daemon puts one JSON envelope on stdin and
sets `BRIDGE_EVENT=<type>` in the environment. The envelope always looks like:

```json
{
  "event": "message.in",
  "ts": "2026-07-06T05:31:42Z",
  "contact": {"id": "…", "name": "vint", "directory": "…",
              "status": "live", "health": "working"},
  "data": {"text": "hey", "via": "phone"}
}
```

Events you can listen to (v1):

| event               | fires when                                   | `data` |
|---------------------|----------------------------------------------|--------|
| `message.in`        | a phone message was delivered to an agent    | `{"text": …, "via": "phone"}` |
| `reply.out`         | an agent reply was relayed to the phone      | `{"text": …}` |
| `permission.prompt` | a permission prompt was reported             | `{"prompt": …}` |
| `agent.connect`     | a contact registered or revived              | `{"reason": "connect"｜"revive"｜"suffixed"}` |
| `agent.idle`        | health went working → ok (tail went quiet)   | `{}` |
| `tick`              | every 60s                                    | `{"contacts": [roster]}` — note: **no** top-level `contact` |

Parse the envelope with a real JSON parser (`python3`, `jq`, your language's
stdlib) — never grep. `memory-keeper` shells out to `python3`; the daemon
guarantees `python3` is available on the platforms bridge supports.

## 3. Actions (stdout)

Print zero or more action objects, **one JSON object per line** (or a single
JSON array). Anything unparseable is dropped and audited — so if you have
nothing to do, print nothing and exit 0. Max 8 actions per invocation.

| action      | shape | effect |
|-------------|-------|--------|
| `nudge`     | `{"action":"nudge","contact":"<id or name>","text":"…"}` | types one line into the agent's session, stamped `[From plugin:<name>]` |
| `notify`    | `{"action":"notify","title":"…","body":"…","contact":"<id>"}` | a push to the phone |
| `emit`      | `{"action":"emit","contact":"<id>","text":"…"}` | a muted system line in that contact's thread |
| `set-field` | `{"action":"set-field","contact":"<id>","key":"…","value":"…"}` | persists on the contact (`fields` map, shown in `/api/status`); key `^[a-z][a-z0-9-]{0,30}$`, value ≤ 200 chars, ≤ 16 keys |

The vocabulary is deliberately small: bridge **delivers and displays**. A
plugin that wants to track shared work or schedule agents builds that itself,
outside bridge.

## 4. State: `BRIDGE_PLUGIN_HOME`

Each plugin gets a private scratch directory at
`~/.bridge/plugins/<name>.d`, created 0700 on first run and exported as
`$BRIDGE_PLUGIN_HOME`. Keep any state you need there — `memory-keeper` writes
one small timestamp file per contact to remember when it last nudged each one.
Always tolerate the directory being empty or missing on the first run
(`mkdir -p "$BRIDGE_PLUGIN_HOME"`).

## 5. Install

Installation is filesystem-only, inside the same trust boundary as the daemon:

```sh
cp memory-keeper ~/.bridge/plugins/
chmod +x ~/.bridge/plugins/memory-keeper
```

That is the entire install. The daemon rescans `~/.bridge/plugins/` at startup
and whenever the directory changes, announces a newly-seen plugin loudly (an
audit line and a feed event), and starts routing matching events to it.

**Security rules the daemon enforces** (see `docs/plugins.md` → *Trust
boundary*):

- `~/.bridge/plugins/` must be mode `0700` and owned by you.
- A plugin file that is **group- or world-writable is refused** and audited —
  keep yours `0700` / `0755`, never `0777`/`0666`.
- Every invocation and every action is audited (`plugin-run`,
  `plugin-action`, `plugin-error`).
- Plugins run with the daemon's uid — the same trust as the daemon itself.
  There is no sandbox; install only plugins you would run by hand.

## Checklist for your own plugin

1. `<exe> manifest` prints a valid, uniquely-named manifest.
2. `<exe> event` reads stdin, prints only valid action JSON (or nothing).
3. It is safe under `set -euo pipefail` and never errors on an empty/odd
   envelope.
4. It keeps state under `$BRIDGE_PLUGIN_HOME`, tolerating a missing dir.
5. `chmod +x`, drop it in `~/.bridge/plugins/`, done.

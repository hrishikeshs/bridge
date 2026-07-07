#!/usr/bin/env bash
# Smoke test for the bridge daemon: build it, boot an ISOLATED instance on a
# throwaway port under a throwaway HOME (so it never touches ~/.bridge or the
# real `bridge serve` a phone may be talking to), then exercise the API with
# curl. See docs/protocol.md §2 for the contract under test.
#
# Isolation contract (important):
#   * separate port (8399, not the daemon default 8378)
#   * HOME points at a mktemp dir, so config/state/lockfile all land there
#   * config.json sets require_identity:false so curl reaches /api without the
#     Tailscale identity header the phone would carry
# The real daemon on 8378 and the real ~/.bridge are never read or written.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT=8399
BASE="http://127.0.0.1:$PORT"

# --- isolate everything under one temp dir --------------------------------
TESTDIR="$(mktemp -d)"
HOME_DIR="$TESTDIR/home"
BIN="$TESTDIR/bridge"
LOG="$TESTDIR/server.log"
HDRS="$TESTDIR/pair-headers"
mkdir -p "$HOME_DIR/.bridge"
# require_identity:false lets curl hit /api without a Tailscale-User-Login
# header; per-device pairing tokens are still enforced.
printf '%s\n' '{"require_identity": false}' > "$HOME_DIR/.bridge/config.json"

# --- plugin runtime fixtures (installed BEFORE the daemon boots) ----------
# docs/plugins.md: discovery scans ~/.bridge/plugins/ at daemon start and lazily
# on dir-mtime change per dispatch. Installing the fixtures before boot is the
# deterministic path (no race with a first dispatch). Three fixtures:
#   smoke-recorder  — well-behaved: records envelopes, emits set-field + emit
#   smoke-garbage   — prints non-JSON on stdout (must be dropped, not fatal)
#   smoke-badperm   — world-writable (mode 0777): must be REFUSED, never run
PLUGINS_DIR="$HOME_DIR/.bridge/plugins"
mkdir -p "$PLUGINS_DIR"
chmod 700 "$PLUGINS_DIR"   # spec: plugins dir must be 0700, owned by the daemon uid

# Absolute marker paths under $TESTDIR so the "did it run?" assertions don't
# depend on $BRIDGE_PLUGIN_HOME (which the daemon only creates for a plugin it
# actually runs — so a refused plugin has none).
GARBAGE_MARKER="$TESTDIR/garbage-ran"   # appears once the garbage plugin runs
BADPERM_MARKER="$TESTDIR/badperm-ran"   # must NEVER appear (plugin is refused)

# Fixture 1 — smoke-recorder: listens to message.in + tick. On every event it
# appends the raw stdin envelope to $BRIDGE_PLUGIN_HOME/events.log, and when the
# event carries a contact it emits set-field + emit for that contact. Pins the
# stdin-envelope, BRIDGE_PLUGIN_HOME, set-field and emit clauses.
cat > "$PLUGINS_DIR/smoke-recorder" <<'REC'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "manifest" ]; then
  printf '%s\n' '{"name":"smoke-recorder","events":["message.in","tick"]}'
  exit 0
fi
[ "${1:-}" = "event" ] || exit 0
env_json="$(cat)"
mkdir -p "${BRIDGE_PLUGIN_HOME:?BRIDGE_PLUGIN_HOME unset}"
printf '%s\n' "$env_json" >> "$BRIDGE_PLUGIN_HOME/events.log"
# The contact id (empty for tick, which carries no contact).
cid="$(printf '%s' "$env_json" | python3 -c 'import json,sys
try: e=json.load(sys.stdin)
except Exception: sys.exit(0)
print((e.get("contact") or {}).get("id") or "")')"
[ -n "$cid" ] || exit 0
printf '{"action":"set-field","contact":"%s","key":"smoke","value":"seen"}\n' "$cid"
printf '{"action":"emit","contact":"%s","text":"smoke-recorder saw an event"}\n' "$cid"
REC
chmod 700 "$PLUGINS_DIR/smoke-recorder"

# Fixture 2 — smoke-garbage: listens to message.in, drains stdin, records that
# it ran, then prints unparseable garbage. The runtime must drop the garbage
# (audited) and keep serving. Unquoted heredoc bakes in the marker path.
cat > "$PLUGINS_DIR/smoke-garbage" <<GARB
#!/usr/bin/env bash
if [ "\${1:-}" = "manifest" ]; then
  printf '%s\n' '{"name":"smoke-garbage","events":["message.in"]}'
  exit 0
fi
[ "\${1:-}" = "event" ] || exit 0
cat >/dev/null
: > "$GARBAGE_MARKER"
printf '%s\n' 'not json at all }{ <<< garbage'
GARB
chmod 700 "$PLUGINS_DIR/smoke-garbage"

# Fixture 3 — smoke-badperm: would record on any event, but is made 0777 =
# executable (so discovery considers it) AND world+group-writable (so the
# runtime must refuse it per docs/plugins.md "Trust boundary"). A plain 0666
# file would just be skipped as non-executable, exercising a different path.
cat > "$PLUGINS_DIR/smoke-badperm" <<BADP
#!/usr/bin/env bash
if [ "\${1:-}" = "manifest" ]; then
  printf '%s\n' '{"name":"smoke-badperm","events":["message.in"]}'
  exit 0
fi
[ "\${1:-}" = "event" ] || exit 0
: > "$BADPERM_MARKER"
BADP
chmod 0777 "$PLUGINS_DIR/smoke-badperm"

SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && { pkill -P "$SERVER_PID" 2>/dev/null || true; kill "$SERVER_PID" 2>/dev/null || true; }
  rm -rf "$TESTDIR"
}
trap cleanup EXIT

# --- port-in-use guard (never stomp a real server) ------------------------
if lsof -nP -iTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1; then
  echo "FAIL: port $PORT already in use (stale test server?)"; exit 1
fi

# --- build (uses the real HOME so the module cache is warm) ---------------
if ! (cd "$ROOT" && go build -o "$BIN" .); then
  echo "FAIL: go build failed"; exit 1
fi

# --- boot the isolated daemon (its own HOME, its own port) ----------------
HOME="$HOME_DIR" "$BIN" serve --port "$PORT" >"$LOG" 2>&1 &
SERVER_PID=$!

for _ in $(seq 40); do
  [ -f "$HOME_DIR/.bridge/daemon.json" ] && curl -s -o /dev/null "$BASE/" && break
  sleep 0.25
done
if ! curl -s -o /dev/null "$BASE/"; then
  echo "FAIL: daemon did not start"; cat "$LOG"; exit 1
fi

# The daemon wrote its lockfile (port + local-trust token) into the isolated
# HOME. The local token authenticates /local/* CLI-style calls.
LOCAL_TOKEN=$(sed -n 's/.*"token":"\([0-9a-f]*\)".*/\1/p' "$HOME_DIR/.bridge/daemon.json")
if [ -z "$LOCAL_TOKEN" ]; then
  echo "FAIL: could not read local token from daemon.json"; cat "$LOG"; exit 1
fi

# --- harness --------------------------------------------------------------
pass=0; fail=0
check() { # check <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then
    pass=$((pass + 1)); echo "ok   - $1"
  else
    fail=$((fail + 1)); echo "FAIL - $1 (expected $2, got $3)"
  fi
}
body_has() { # body_has <desc> <needle> <haystack>
  case "$3" in
    *"$2"*) pass=$((pass + 1)); echo "ok   - $1" ;;
    *)      fail=$((fail + 1)); echo "FAIL - $1 (missing '$2' in: $3)" ;;
  esac
}
pass_msg() { pass=$((pass + 1)); echo "ok   - $1"; }
fail_msg() { fail=$((fail + 1)); echo "FAIL - $1"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# Precompute JSON bodies into variables: macOS /bin/bash 3.2 mis-parses \"
# escapes inside a double-quoted $(...) and brace-expands JSON into fragments.
J=(-H "Content-Type: application/json")
LOCAL_AUTH=(-H "Authorization: Bearer $LOCAL_TOKEN")

CNAME="smoke-agent"
BAD_CODE_BODY='{"code":"000000"}'
GHOST_BODY='{"agent":"ghost-nobody","text":"anyone home?"}'
EMPTY_BODY='{"agent":"ghost-nobody","text":"   "}'
DUP_BODY="{\"agent\":\"$CNAME\",\"text\":\"dup test\",\"client_id\":\"cid-smoke-1\"}"
# tmux_target is a window id ("@N") — the grammar-immune routing key the daemon now
# requires for new registrations. @999 is a nonexistent window, so the contact is
# retired to offline by the reconcile loop (exercises the offline path below).
CONNECT_BODY="{\"name\":\"$CNAME\",\"directory\":\"/private/tmp/smoke-bridge\",\"session_id\":\"sess-smoke\",\"tmux_target\":\"@999\"}"
# C2 daemon-side hardening fixtures: a numeric name (tmux would misresolve it as a
# window index) and a legacy name-based target (send-keys to a window index) must
# both be neutralized at the registry choke point, not just by the CLI.
NUM_CONNECT_BODY="{\"name\":\"1\",\"directory\":\"/private/tmp/smoke-bridge\",\"session_id\":\"sess-num\",\"tmux_target\":\"@998\"}"
BADTGT_BODY="{\"name\":\"legit-name\",\"directory\":\"/private/tmp/smoke-bridge\",\"session_id\":\"sess-bt\",\"tmux_target\":\"bridge:1\"}"
DUP1_BODY="{\"name\":\"twin\",\"directory\":\"/private/tmp/smoke-a\",\"session_id\":\"sess-twa\",\"tmux_target\":\"@990\"}"
DUP2_BODY="{\"name\":\"twin\",\"directory\":\"/private/tmp/smoke-b\",\"session_id\":\"sess-twb\",\"tmux_target\":\"@991\"}"
APPROVE_BADKEY='{"agent":"ghost-nobody","key":"q"}'
APPROVE_OFFLINE='{"agent":"ghost-nobody","key":"1"}'
# push.example.com does not resolve; subscribe-time validation fails CLOSED on a
# lookup miss (#7), so this endpoint must be refused — deterministic offline too.
SUB_UNRESOLVABLE='{"endpoint":"https://push.example.com/smoke-endpoint-xyz","keys":{"p256dh":"BFakeKeyForSmokeTestingPurposesOnly","auth":"ZmFrZWF1dGg"}}'
# The real production push host — resolves to public IPs, so it exercises the
# accept path. Gated on DNS actually working so the suite stays green offline.
SUB_APPLE='{"endpoint":"https://web.push.apple.com/smoke-endpoint-xyz","keys":{"p256dh":"BFakeKeyForSmokeTestingPurposesOnly","auth":"ZmFrZWF1dGg"}}'
SUB_BAD='{"endpoint":"","keys":{"p256dh":"x","auth":"y"}}'

# --- static + perimeter ---------------------------------------------------
check "app shell served (GET /)"          200 "$(code $BASE/)"
check "api rejects unpaired device"       401 "$(code $BASE/api/status)"
check "path traversal blocked (../)"      404 "$(code --path-as-is $BASE/../serve.go)"

# --- local API auth -------------------------------------------------------
check "/local rejects missing token"      401 "$(code -X POST $BASE/local/pair)"
check "/local rejects wrong token"        401 "$(code -X POST -H 'Authorization: Bearer deadbeef' $BASE/local/pair)"

# Mint a pairing code via the local API (the on-machine `bridge pair` path).
MINT=$(curl -s -w '\n%{http_code}' "${LOCAL_AUTH[@]}" -X POST $BASE/local/pair)
MINT_STATUS=$(printf '%s' "$MINT" | tail -n1)
MINT_BODY=$(printf '%s' "$MINT" | sed '$d')
CODE=$(printf '%s' "$MINT_BODY" | sed -n 's/.*"code":"\([0-9]*\)".*/\1/p')
check "mint pairing code -> 200"          200 "$MINT_STATUS"
case "$CODE" in
  [0-9][0-9][0-9][0-9][0-9][0-9]) pass_msg "minted code is 6 digits ($CODE)" ;;
  *) fail_msg "minted code not 6 digits: '$CODE'" ;;
esac

PAIR_BODY="{\"code\":\"$CODE\",\"device\":\"smoke\"}"

# --- pairing --------------------------------------------------------------
check "bad pairing code -> 403"           403 "$(code "${J[@]}" -d "$BAD_CODE_BODY" $BASE/api/pair)"

# Good pair: capture status + Set-Cookie header (device token).
PAIR_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -D "$HDRS" "${J[@]}" -d "$PAIR_BODY" $BASE/api/pair)
check "pairing succeeds -> 200"           200 "$PAIR_STATUS"
if grep -qi 'set-cookie:.*bridge_token=' "$HDRS"; then
  pass_msg "pairing sets bridge_token cookie"
else
  fail_msg "no bridge_token Set-Cookie header"
fi
DEVICE_TOKEN=$(grep -i 'set-cookie:.*bridge_token=' "$HDRS" | sed -n 's/.*bridge_token=\([0-9a-f]*\).*/\1/p' | head -n1)
DEV_AUTH=(-H "Authorization: Bearer $DEVICE_TOKEN")

check "pairing code is single-use -> 403" 403 "$(code "${J[@]}" -d "$PAIR_BODY" $BASE/api/pair)"

# --- authenticated device API ---------------------------------------------
check "status with token -> 200"          200 "$(code "${DEV_AUTH[@]}" $BASE/api/status)"
STATUS_BODY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "status returns contacts array"  '"contacts"'         "$STATUS_BODY"
body_has "status returns version"         '"version":"0.1.0"'  "$STATUS_BODY"
# round 4: presence clocks for the phone (now + daemon start time)
body_has "status returns server clock"    '"now":'             "$STATUS_BODY"
body_has "status returns daemon start"    '"started":'         "$STATUS_BODY"

# --- register a fake contact via the local API ----------------------------
check "local/connect registers contact"   200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CONNECT_BODY" $BASE/local/connect)"
STATUS_BODY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "status now lists the contact"   "\"name\":\"$CNAME\"" "$STATUS_BODY"
CONTACTS_BODY=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts)
body_has "local/contacts lists the contact" "\"name\":\"$CNAME\"" "$CONTACTS_BODY"
check "local/contacts needs local token"  401 "$(code $BASE/local/contacts)"

# --- C2: daemon-side name/target sanitization (finding #1) ----------------
# A rollback binary or stale client speaks this exact /local/connect protocol, so
# the choke point (registry.Connect), not just the CLI, must refuse a numeric name
# — tmux resolves "1" as a window *index*, misrouting messages to another agent.
NUM_RESP=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$NUM_CONNECT_BODY" $BASE/local/connect)
case "$NUM_RESP" in
  *'"name":"1"'*) fail_msg "numeric name '1' accepted daemon-side (C2 regression)" ;;
  *)              pass_msg "numeric name refused daemon-side (sanitized to a safe address)" ;;
esac
CONTACTS_BODY=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts)
case "$CONTACTS_BODY" in
  *'"name":"1"'*) fail_msg "roster exposes a contact named '1'" ;;
  *)              pass_msg "roster never exposes a numeric-named contact" ;;
esac
# A legacy name-based target ("bridge:1") could send-keys to a window index; only a
# "@N" window id is grammar-immune. The daemon accepts the (valid) name but blanks
# the target, so the contact is present yet unroutable until it reconnects.
check "legacy target accepted, name kept" 200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$BADTGT_BODY" $BASE/local/connect)"
CONTACTS_BODY=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts)
body_has "legacy target neutralized to empty" '"tmux_target":""' "$CONTACTS_BODY"

# --- collision suffix yields a distinct address (finding #2) --------------
# Two agents in different directories choosing the same name must not collide: the
# second is suffixed so each stays addressable (and, when live, gets its own
# window). Registered back-to-back while the first is still live.
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$DUP1_BODY" $BASE/local/connect
DUP2_RESP=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$DUP2_BODY" $BASE/local/connect)
body_has "collision suffix yields distinct name" '"name":"twin-2"' "$DUP2_RESP"

# --- send: offline, idempotency, validation -------------------------------
check "send to unknown contact -> 409"    409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$GHOST_BODY" $BASE/api/send)"
check "empty send rejected -> 400"        400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$EMPTY_BODY" $BASE/api/send)"
# H1: a client_id commits only after a *durable* accept (delivered, or mailboxed
# for a known-offline contact); a failed/unknown send no longer falsely dedups.
# Wait for the reconcile loop to retire the dead-tmux contact to offline, so the
# first send is queued (durable — commits the id) and the retry is a true dup.
for _ in $(seq 40); do
  curl -s "${DEV_AUTH[@]}" $BASE/api/status | grep -q '"offline"' && break
  sleep 0.25
done
curl -s -o /dev/null "${DEV_AUTH[@]}" "${J[@]}" -d "$DUP_BODY" $BASE/api/send
DUP_RESP=$(curl -s "${DEV_AUTH[@]}" "${J[@]}" -d "$DUP_BODY" $BASE/api/send)
body_has "duplicate client_id acked"      '"duplicate":true' "$DUP_RESP"

# --- interrupt: stop mid-thought (Escape) ----------------------------------
# Honored only for LIVE contacts; smoke-agent is offline by now (dead @999),
# and a ghost resolves to nothing — both must refuse, never blind-key.
check "interrupt unknown agent -> 409"    409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d '{"agent":"ghost-nobody"}' $BASE/api/interrupt)"
INTERRUPT_OFFLINE="{\"agent\":\"$CNAME\"}"
check "interrupt offline agent -> 409"    409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$INTERRUPT_OFFLINE" $BASE/api/interrupt)"

# --- approve gating -------------------------------------------------------
check "approve rejects bad key -> 400"    400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$APPROVE_BADKEY" $BASE/api/approve)"
check "approve on offline agent -> 400"   400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$APPROVE_OFFLINE" $BASE/api/approve)"

# --- round 2: switchboard honesty, sender identity, hook parking -----------
# H9: the local/send "contact" field becomes the provenance frame's From in
# the recipient's terminal — an unregistered string there is identity forgery.
check "local/send unknown sender -> 400"  400 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d '{"contact":"nobody-here","text":"hi"}' $BASE/local/send)"
# Switchboard: a missing recipient is an error, but an OFFLINE recipient is a
# durably-queued SUCCESS — the old 409 read as failure, so senders retried
# and the recipient woke up to duplicates.
SB_GHOST_BODY="{\"contact\":\"$CNAME\",\"text\":\"hi\",\"to\":\"ghost-nobody\"}"
check "switchboard unknown recipient -> 404" 404 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$SB_GHOST_BODY" $BASE/local/send)"
# Wait for 'twin' (dead @990 target) to strike out to offline first, so the
# send below exercises the queued path, not live delivery.
for _ in $(seq 60); do
  TWIN_STATUS=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts | python3 -c 'import json,sys
data = json.load(sys.stdin)
for c in (data.get("contacts") if isinstance(data, dict) else data) or []:
    if c.get("name") == "twin": print(c.get("status",""))' 2>/dev/null)
  [ "$TWIN_STATUS" = "offline" ] && break
  sleep 0.25
done
SB_RESP=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "{\"contact\":\"$CNAME\",\"text\":\"queued hello\",\"to\":\"twin\"}" $BASE/local/send)
body_has "switchboard offline recipient queued -> queued:true" '"queued":true' "$SB_RESP"
# H8: a hook event whose session id no contact claims is PARKED for the
# reconcile loop to chain-resolve — a session roll's one-and-only hook
# delivery must never be dropped on the floor.
HOOK_RESP=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d '{"session_id":"00000000-0000-4000-8000-00000000d00d","message":"x","kind":"notification"}' $BASE/local/event)
body_has "unclaimed hook event parked (H8)" '"parked":true' "$HOOK_RESP"

# --- away messages (AIM statuses, both directions) ------------------------
# Direction 1 — an agent sets its away line via /local/status. The "contact"
# field is the SENDER's own identity (like /local/send), so an unregistered
# sender is the same H9 forgery vector and must be refused; a registered one's
# status is stored and surfaced on /api/status as the contact's "away".
STATUS_UNKNOWN_BODY='{"contact":"nobody-here","text":"brb"}'
STATUS_SET_BODY="{\"contact\":\"$CNAME\",\"text\":\"in a meeting\"}"
check "away status unknown sender -> 400"   400 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$STATUS_UNKNOWN_BODY" $BASE/local/status)"
check "away status set -> 200"              200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$STATUS_SET_BODY" $BASE/local/status)"
STATUS_BODY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "away surfaced on /api/status"     '"away":"in a meeting"' "$STATUS_BODY"
# Direction 2 — the human sets "My status" (device-token authed). It surfaces on
# /api/status as a top-level my_status and is the auto-responder delivered to an
# agent the moment it reaches out.
MYSTATUS_BODY='{"text":"on the couch"}'
check "my-status set -> 200"                200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$MYSTATUS_BODY" $BASE/api/mystatus)"
STATUS_BODY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "my_status surfaced on /api/status" '"my_status":"on the couch"' "$STATUS_BODY"

# --- quoting & reactions (features A + B) ---------------------------------
# Mint a reactable event WITHOUT a live tmux: /local/send with no `to` is the
# "agent reaches out to the phone" path, which emits a "reply" in the sender's
# own thread. The (offline) smoke agent still resolves as a sender, so this is
# deterministic. Read the new event's id back from /api/history.
REACT_MINT_BODY="{\"contact\":\"$CNAME\",\"text\":\"a line worth reacting to\"}"
check "mint reply event (agent reach-out) -> 200" 200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$REACT_MINT_BODY" $BASE/local/send)"
EVID=$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0" | python3 -c 'import json,sys
data = json.load(sys.stdin)
ids = [e["id"] for e in data.get("events", [])
       if e.get("type") == "reply" and e.get("text") == "a line worth reacting to"]
print(ids[-1] if ids else "")' 2>/dev/null)
EVID="${EVID:-0}"   # fall back to a non-existent id so the bodies stay valid JSON
if [ "$EVID" != "0" ]; then
  pass_msg "reply event id read from history ($EVID)"
else
  fail_msg "could not read a reply event id from /api/history"
fi

# Bodies pre-defined (the JSON-in-$(...) gotcha): event_id is a number, emoji is
# raw UTF-8. 🦄 is outside the whitelist; 👍 is inside it.
REACT_BADEMOJI_BODY="{\"agent\":\"$CNAME\",\"event_id\":$EVID,\"emoji\":\"🦄\"}"
REACT_NOEVENT_BODY="{\"agent\":\"$CNAME\",\"event_id\":999999999,\"emoji\":\"👍\"}"
REACT_OK_BODY="{\"agent\":\"$CNAME\",\"event_id\":$EVID,\"emoji\":\"👍\"}"
check "react bad emoji -> 400"            400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$REACT_BADEMOJI_BODY" $BASE/api/react)"
check "react unknown event id -> 404"     404 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$REACT_NOEVENT_BODY" $BASE/api/react)"
check "react on a real event -> 200"      200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$REACT_OK_BODY" $BASE/api/react)"
REACT_DUP=$(curl -s "${DEV_AUTH[@]}" "${J[@]}" -d "$REACT_OK_BODY" $BASE/api/react)
body_has "duplicate react -> duplicate:true" '"duplicate":true' "$REACT_DUP"

# A quoted phone send to the OFFLINE smoke agent is durably queued (409): the
# quote rides inline in the mailbox text and the client_id commits, so the
# identical retry is a safe 200 duplicate — proving the quote fields were
# ACCEPTED (not 400-rejected) and the send was durably taken.
QUOTE_SEND_BODY="{\"agent\":\"$CNAME\",\"text\":\"ship it\",\"client_id\":\"cid-quote-1\",\"quote\":{\"name\":\"marvin\",\"excerpt\":\"the build passed\"}}"
check "quote-send to offline queued -> 409" 409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$QUOTE_SEND_BODY" $BASE/api/send)"
QUOTE_DUP=$(curl -s "${DEV_AUTH[@]}" "${J[@]}" -d "$QUOTE_SEND_BODY" $BASE/api/send)
body_has "quote-send accepted -> 200" '"duplicate":true' "$QUOTE_DUP"

# --- party line (#crew room) ----------------------------------------------
# The one built-in room fans a message out to every registered contact and
# records exactly ONE event keyed to the room id ("room:crew"). Membership here
# is the (offline) smoke contacts, so every fan-out is a durable mailbox queue —
# which IS success (the round-2 rule): a phone send to the room acks 200 with no
# live member. Bodies pre-defined (the JSON-in-$(...) gotcha).
ROOM_SEND_BODY='{"agent":"room:crew","text":"party line hello"}'
ROOM_LOCAL_BODY="{\"contact\":\"$CNAME\",\"text\":\"crew, standup in 5\",\"to\":\"#crew\"}"
ROOM_BADSENDER_BODY='{"contact":"nobody-here","text":"hi","to":"#crew"}'
ROOM_APPROVE_BODY='{"agent":"room:crew","key":"1"}'
ROOM_INTERRUPT_BODY='{"agent":"room:crew"}'

# /api/status advertises the room so the phone renders its row without hardcoding.
STATUS_BODY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "status advertises the rooms array" '"rooms":'            "$STATUS_BODY"
body_has "status carries the #crew room"      '"id":"room:crew","name":"#crew"' "$STATUS_BODY"

# Phone -> room: 200 even with no live member, and history records one 'sent'
# keyed to the room id (struct field order pins type before agent).
check "phone send to #crew -> 200"            200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$ROOM_SEND_BODY" $BASE/api/send)"
HIST_BODY=$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0")
body_has "history shows a room 'sent' event"  '"type":"sent","agent":"room:crew"' "$HIST_BODY"

# Agent -> room via /local/send: the smoke contact resolves as sender, fans out
# to the other (offline) members, and records a 'peer' keyed to the room.
check "agent send --to #crew -> 200"          200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$ROOM_LOCAL_BODY" $BASE/local/send)"
HIST_BODY=$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0")
body_has "history shows a room 'peer' event"  '"type":"peer","agent":"room:crew"' "$HIST_BODY"

# H9 still holds on the room path: the sender must resolve to a registered
# contact (it becomes the frame's author), so an unknown sender is a 400.
check "room send unknown sender -> 400"       400 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$ROOM_BADSENDER_BODY" $BASE/local/send)"

# A room is a fan-out thread, not a pane: approve/interrupt have nothing to key.
check "approve on the room rejected -> 400"   400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$ROOM_APPROVE_BODY" $BASE/api/approve)"
check "interrupt on the room rejected -> 409" 409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$ROOM_INTERRUPT_BODY" $BASE/api/interrupt)"

# --- room cooldown (the crew's own rule, 2026-07-06) ------------------------
# Between human messages each agent speaks at most once. The agent send above
# consumed the smoke contact's slot for the current human turn, so a second
# post is refused 429 with a readable detail; a phone message reopens the room
# and the agent may speak again.
check "second agent post same turn -> 429"    429 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$ROOM_LOCAL_BODY" $BASE/local/send)"
COOLDOWN_RESP=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$ROOM_LOCAL_BODY" $BASE/local/send)
body_has "cooldown refusal carries a detail line" '"detail":"party-line cooldown' "$COOLDOWN_RESP"
ROOM_REOPEN_BODY='{"agent":"room:crew","text":"human speaks, room reopens"}'
check "phone message reopens the room -> 200" 200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$ROOM_REOPEN_BODY" $BASE/api/send)"
check "agent may speak again after human -> 200" 200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$ROOM_LOCAL_BODY" $BASE/local/send)"

# --- web push -------------------------------------------------------------
check "push key -> 200"                    200 "$(code "${DEV_AUTH[@]}" $BASE/api/push/key)"
PUSH_KEY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/push/key | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
if [ "${#PUSH_KEY}" -ge 80 ]; then
  pass_msg "VAPID public key looks valid (len ${#PUSH_KEY})"
else
  fail_msg "VAPID key too short (len ${#PUSH_KEY}): '$PUSH_KEY'"
fi
check "push subscribe unresolvable host -> 400 (fail closed)" 400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$SUB_UNRESOLVABLE" $BASE/api/push/subscribe)"
check "push subscribe no endpoint -> 400"  400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$SUB_BAD" $BASE/api/push/subscribe)"
# Accept path needs real DNS (subscribe-time validation resolves the host); skip
# rather than fail when the network is down.
if nslookup -timeout=3 web.push.apple.com >/dev/null 2>&1; then
  check "push subscribe real host -> 200"  200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$SUB_APPLE" $BASE/api/push/subscribe)"
else
  echo "skip - push subscribe real host (no DNS)"
fi

# --- plugin runtime (docs/plugins.md) -------------------------------------
# The three fixtures were installed into the isolated daemon's plugins dir
# BEFORE it booted (top of this script), so discovery loaded them at start /
# first dispatch. We drive one event through and assert on plugin-produced
# state — fields, feed, marker files — never on send-keys side effects: the
# smoke contact rides tmux_target "@999", so nothing ever reaches a terminal.
#
# NOTE (spec corner these checks assume): in this harness a send can never truly
# "deliver" (there is no real tmux window; deliverToSession always fails), so
# checks (a)/(b) require the runtime to fire message.in when a phone message is
# ACCEPTED for a known contact (live-attempt OR mailbox-queue), independent of
# send-keys success. If message.in is gated on successful delivery it is simply
# unobservable here.

# smoke-agent is retired to offline by the reconcile loop (tmux @999 is dead);
# wait for that so the trigger send lands on a deterministic accept path.
for _ in $(seq 40); do
  curl -s "${DEV_AUTH[@]}" $BASE/api/status | grep -q '"offline"' && break
  sleep 0.25
done

# The message.in trigger: a phone->agent send with no client_id (always
# processed). To the offline smoke-agent it is accepted and mailboxed (409).
PLUGIN_SEND_BODY="{\"agent\":\"$CNAME\",\"text\":\"plugin runtime trigger\"}"
check "plugin trigger send accepted (offline -> 409)" 409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$PLUGIN_SEND_BODY" $BASE/api/send)"

# (a) recorder ran on message.in and its envelope names the right contact. It
#     appends every envelope to $BRIDGE_PLUGIN_HOME/events.log; tick envelopes
#     also land there, so we match the message.in line specifically.
REC_LOG="$HOME_DIR/.bridge/plugins/smoke-recorder.d/events.log"
ran=n
for _ in $(seq 40); do
  if [ -f "$REC_LOG" ] && grep -q '"event":"message.in"' "$REC_LOG" && grep -q "\"name\":\"$CNAME\"" "$REC_LOG"; then
    ran=y; break
  fi
  sleep 0.25
done
if [ "$ran" = y ]; then
  pass_msg "plugin ran on message.in carrying contact '$CNAME' (envelope recorded)"
else
  fail_msg "recorder logged no message.in envelope for '$CNAME' (see $REC_LOG)"
fi

# (b) recorder's set-field action is persisted and surfaced in /api/status.
seen=n
for _ in $(seq 40); do
  case "$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)" in
    *'"smoke":"seen"'*) seen=y; break ;;
  esac
  sleep 0.25
done
if [ "$seen" = y ]; then
  pass_msg "set-field persisted + surfaced in /api/status (smoke=seen)"
else
  fail_msg "set-field not visible in /api/status (needs Contact.fields wired daemon-side)"
fi

# (d) a garbage-printing plugin is dropped, not fatal: it still ran (marker),
#     and the daemon keeps serving both /api/status and the send path after.
ran=n
for _ in $(seq 40); do
  [ -e "$GARBAGE_MARKER" ] && { ran=y; break; }
  sleep 0.25
done
if [ "$ran" = y ]; then
  pass_msg "garbage-printing plugin ran (its stdout was dropped, not fatal)"
else
  fail_msg "garbage plugin never ran (marker $GARBAGE_MARKER absent)"
fi
check "daemon still serves after garbage plugin (status 200)" 200 "$(code "${DEV_AUTH[@]}" $BASE/api/status)"
check "send path still serves after garbage plugin (offline -> 409)" 409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$PLUGIN_SEND_BODY" $BASE/api/send)"

# (c) a world-writable plugin file is refused: it must NEVER run, and (since a
#     refused plugin is never invoked) its name appears in the audit log only as
#     a refusal.
if [ -e "$BADPERM_MARKER" ]; then
  fail_msg "world-writable plugin RAN ($BADPERM_MARKER present) — refusal failed"
else
  pass_msg "world-writable plugin refused (never ran)"
fi
if [ -f "$HOME_DIR/.bridge/audit.log" ]; then
  if grep -q "smoke-badperm" "$HOME_DIR/.bridge/audit.log"; then
    pass_msg "world-writable plugin refusal was audited"
  else
    fail_msg "audit log has no refusal line for 'smoke-badperm'"
  fi
else
  echo "skip - audit log not reachable"
fi

# --- retire: ghosts get funerals, the living are protected ----------------
check "retire unknown -> 404"              404 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d '{"contact":"nobody-here"}' $BASE/local/retire)"
check "retire offline contact -> 200"      200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d '{"contact":"twin-2"}' $BASE/local/retire)"
CONTACTS_BODY=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts)
case "$CONTACTS_BODY" in
  *'"name":"twin-2"'*) fail_msg "retired contact still on roster" ;;
  *)                   pass_msg "retired contact gone from roster" ;;
esac

# --- lockdown (LAST: it stops the daemon) ---------------------------------
check "lockdown -> 200"                    200 "$(code "${LOCAL_AUTH[@]}" -X POST $BASE/local/lockdown)"
gone=n
for _ in $(seq 20); do
  lsof -nP -iTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1 || { gone=y; break; }
  sleep 0.25
done
if [ "$gone" = y ]; then
  pass_msg "daemon exited after lockdown"
else
  fail_msg "daemon still listening after lockdown"
fi

echo "----"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]

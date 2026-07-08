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
# header; per-device pairing tokens are still enforced. wake_digest_min_away_s:6
# arms the since-you-woke digest at a smoke-fast threshold: a deliberate >6s wait
# (its own section below) trips it, while the brief offline→re-hello reconnects the
# other sections do stay well under it — so those sections' assertions are unmoved
# (a routine reconnect fires no digest: the "brief blip < threshold" negative).
printf '%s\n' '{"require_identity": false, "remote_ttl_s": 3, "remote_ack_timeout_s": 2, "wake_digest_min_away_s": 6}' > "$HOME_DIR/.bridge/config.json"

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
# BRIDGE_FANOUT_STAGGER_MS=150: a tiny per-recipient stagger step so the fan-out
# section below can watch a #crew message spread across live members fast (prod
# defaults to 2500ms). It only affects the multi-recipient paths; 1:1 is untouched.
HOME="$HOME_DIR" BRIDGE_COALESCE_MS=300 BRIDGE_FANOUT_STAGGER_MS=150 "$BIN" serve --port "$PORT" >"$LOG" 2>&1 &
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

# --- The Bridge Herald (morning paper) --------------------------------------
# `bridge paper` (POST /local/paper) publishes an on-demand edition: one
# "paper" event keyed to the #crew thread, composed from recorded facts.
PAPER_RESP=$(curl -s "${LOCAL_AUTH[@]}" -X POST $BASE/local/paper)
body_has "paper publishes edition 1"          '"edition":1' "$PAPER_RESP"
HIST_BODY=$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0")
body_has "history carries the paper in #crew" '"type":"paper","agent":"room:crew"' "$HIST_BODY"
body_has "the edition reports the wire"        'THE OVERNIGHT WIRE' "$HIST_BODY"

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

# --- remote transport (docs/transports.md) --------------------------------
# An external client (here, curl) HELLOs its agents, ATTESTs their state, and
# DRAINs/ACKs parked deliveries — the daemon answers the five Transport
# questions from that attested state alone, never reaching into the client.
# config.json set remote_ttl_s:3 + remote_ack_timeout_s:2 to keep lease/ack
# timings smoke-fast; a real client attests on a ~10s timer, so this section
# interleaves keepalive attests to hold the compressed 3s lease fresh across the
# mandated sleeps (single-threaded curl can't attest concurrently). Placed
# before lockdown, which stops the daemon these checks need alive. Bodies
# pre-defined (the JSON-in-$(...) gotcha); mail parsed with python3.

# rt_field <key>: a top-level string field of a JSON object on stdin.
rt_field() { python3 -c 'import json,sys
try: print(json.load(sys.stdin).get(sys.argv[1],"") or "")
except Exception: print("")' "$1"; }
# rt_agent0: agents[0].id of a hello response on stdin.
rt_agent0() { python3 -c 'import json,sys
try:
    a=json.load(sys.stdin).get("agents",[]); print(a[0]["id"] if a else "")
except Exception: print("")'; }
# rt_deliv <id|text|jsonids>: first delivery id/text, or all ids as JSON array
# elements ("a","b"), from a mail response on stdin.
rt_deliv() { python3 -c 'import json,sys
try: d=json.load(sys.stdin).get("deliveries",[])
except Exception: d=[]
k=sys.argv[1]
if k=="jsonids": print(",".join(json.dumps(x.get("id","")) for x in d))
elif d:         print(d[0].get(k,""))' "$1"; }

RT_DIR="$TESTDIR"

# hello: register sim-otter under flavor "sim". session_id is empty and the dir
# is the temp root — the outbound tail machinery no-ops on a live contact with
# no resolvable session file, so nothing spurious reaches the phone thread.
RT_HELLO_BODY="{\"transport\":\"sim\",\"agents\":[{\"name\":\"sim-otter\",\"directory\":\"$RT_DIR\",\"session_id\":\"\"}]}"
RT_HELLO=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_HELLO_BODY" $BASE/local/transport/hello)
LEASE=$(printf '%s' "$RT_HELLO" | rt_field lease)
RCID=$(printf '%s' "$RT_HELLO" | rt_agent0)
if [ -n "$LEASE" ] && [ -n "$RCID" ]; then
  pass_msg "hello issued a lease + contact id ($RCID)"
else
  fail_msg "hello returned no lease+id: $RT_HELLO"
fi
body_has "hello reports the clamped ttl"       '"ttl_s":3'                 "$RT_HELLO"

# the contact is live on the remote transport, tagged with the client's flavor
CONTACTS_BODY=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts)
body_has "sim-otter registered live"           '"name":"sim-otter"'        "$CONTACTS_BODY"
body_has "sim-otter uses the remote transport" '"transport":"remote"'      "$CONTACTS_BODY"
body_has "sim-otter carries the sim flavor"    '"transport_flavor":"sim"'  "$CONTACTS_BODY"

RT_ATTEST_READY="{\"lease\":\"$LEASE\",\"states\":[{\"id\":\"$RCID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
RT_ATTEST_NOTREADY="{\"lease\":\"$LEASE\",\"states\":[{\"id\":\"$RCID\",\"ready\":false,\"prompt_open\":false,\"screen_tail\":\"\"}]}"

# --- A: happy path (attested ready -> park -> drain -> ack) ----------------
check "attest ready -> 200"                    200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY" $BASE/local/transport/attest)"
RT_SEND_A="{\"agent\":\"$RCID\",\"text\":\"remote hello there\"}"
check "phone send to sim-otter -> 200"         200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$RT_SEND_A" $BASE/api/send)"
sleep 1
RT_MAIL_A=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE&wait=2")
body_has "mail carries the daemon-authored frame" '[From ' "$RT_MAIL_A"
body_has "mail carries the sent text"             'remote hello there' "$RT_MAIL_A"
RT_MID_A=$(printf '%s' "$RT_MAIL_A" | rt_deliv id)
if [ -n "$RT_MID_A" ]; then
  pass_msg "mail delivery carries an id ($RT_MID_A)"
else
  fail_msg "mail returned no delivery id: $RT_MAIL_A"
fi
RT_ACK_A="{\"lease\":\"$LEASE\",\"ids\":[\"$RT_MID_A\"]}"
check "ack the delivery -> 200"                200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ACK_A" $BASE/local/transport/ack)"
RT_MAIL_A2=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE&wait=1")
body_has "acked outbox drains to empty"        '"deliveries":[]' "$RT_MAIL_A2"
HIST_BODY=$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0")
body_has "history records the phone send"      'remote hello there' "$HIST_BODY"

# --- B: the guard rides attests (not-ready never types) --------------------
check "attest not-ready -> 200"                200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_NOTREADY" $BASE/local/transport/attest)"
RT_SEND_B="{\"agent\":\"$RCID\",\"text\":\"held while not ready\"}"
curl -s -o /dev/null "${DEV_AUTH[@]}" "${J[@]}" -d "$RT_SEND_B" $BASE/api/send
sleep 1
RT_MAIL_B=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE&wait=1")
body_has "nothing parked while not ready"      '"deliveries":[]' "$RT_MAIL_B"
check "attest ready again -> 200"              200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY" $BASE/local/transport/attest)"
RT_MAIL_B2=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE&wait=6")
body_has "the retry flush parks the held line" 'held while not ready' "$RT_MAIL_B2"
RT_MID_B=$(printf '%s' "$RT_MAIL_B2" | rt_deliv id)
RT_ACK_B="{\"lease\":\"$LEASE\",\"ids\":[\"$RT_MID_B\"]}"
check "ack the held line -> 200"               200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ACK_B" $BASE/local/transport/ack)"

# --- C: lease death + durable redelivery ----------------------------------
# Stop attesting: the lease goes stale (ttl 3s) and sim-otter strikes offline
# (two ~2s reconcile ticks). A send while dead is durably queued (409); a
# re-hello under the SAME name+dir returns the SAME contact id (identity
# survives lease death) and the queued line delivers on the revived lease.
RT_DEAD=n
for _ in $(seq 60); do
  case "$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts | python3 -c 'import json,sys
data=json.load(sys.stdin)
for c in (data.get("contacts") if isinstance(data,dict) else data) or []:
    if c.get("name")=="sim-otter": print(c.get("status",""))' 2>/dev/null)" in
    offline) RT_DEAD=y; break ;;
  esac
  sleep 0.25
done
if [ "$RT_DEAD" = y ]; then
  pass_msg "sim-otter struck offline after its lease went stale"
else
  fail_msg "sim-otter never went offline after the lease died"
fi
RT_SEND_C="{\"agent\":\"$RCID\",\"text\":\"while dead\"}"
check "phone send while dead queued -> 409"    409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$RT_SEND_C" $BASE/api/send)"
RT_HELLO2=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_HELLO_BODY" $BASE/local/transport/hello)
LEASE2=$(printf '%s' "$RT_HELLO2" | rt_field lease)
RCID2=$(printf '%s' "$RT_HELLO2" | rt_agent0)
check "re-hello returns the SAME contact id"   "$RCID" "$RCID2"
RT_ATTEST_READY2="{\"lease\":\"$LEASE2\",\"states\":[{\"id\":\"$RCID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
check "attest ready on the new lease -> 200"   200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY2" $BASE/local/transport/attest)"
RT_MAIL_C=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE2&wait=6")
body_has "the queued line delivers on revival" 'while dead' "$RT_MAIL_C"
RT_MID_C=$(printf '%s' "$RT_MAIL_C" | rt_deliv id)
RT_ACK_C="{\"lease\":\"$LEASE2\",\"ids\":[\"$RT_MID_C\"]}"
check "ack the revived delivery -> 200"        200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ACK_C" $BASE/local/transport/ack)"
# The offline-queued line emits its "sent" event on DELIVERY (after the ack
# wakes the blocked flush), not at queue time — poll for the sent-later bubble.
RT_SENT_LATER=n
for _ in $(seq 15); do
  case "$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0")" in
    *'while dead'*) RT_SENT_LATER=y; break ;;
  esac
  sleep 0.2
done
if [ "$RT_SENT_LATER" = y ]; then
  pass_msg "history shows the sent-later line"
else
  fail_msg "history never showed the sent-later 'while dead' line"
fi

# --- D: attesting the dead lease is 410 -----------------------------------
# The OLD lease (superseded by the re-hello) is stale and spent: attesting it
# tells the client to re-hello rather than silently accepting a zombie.
check "attest with the dead lease -> 410"      410 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY" $BASE/local/transport/attest)"

# --- E: ack-timeout redelivery (durable, NEW delivery id) -----------------
# A parked line the client never acks times out after remote_ack_timeout_s (2s):
# the entry is dropped and the lease marked suspect, but the mailbox retains the
# message, so the next ready attest (which clears suspect) redelivers the SAME
# TEXT under a NEW id. A keepalive attest mid-wait holds the 3s lease fresh while
# the ack timeout fires.
check "attest ready before probe -> 200"       200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY2" $BASE/local/transport/attest)"
RT_SEND_E="{\"agent\":\"$RCID\",\"text\":\"timeout probe\"}"
curl -s -o /dev/null "${DEV_AUTH[@]}" "${J[@]}" -d "$RT_SEND_E" $BASE/api/send
sleep 1
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY2" $BASE/local/transport/attest  # keepalive
RT_MAIL_E1=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE2&wait=2")
body_has "the probe parks once"                'timeout probe' "$RT_MAIL_E1"
RT_MID_E1=$(printf '%s' "$RT_MAIL_E1" | rt_deliv id)
sleep 2   # past the 2s ack timeout: Deliver gives up, drops the entry, mailbox retains
check "attest ready clears suspect -> 200"     200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_READY2" $BASE/local/transport/attest)"
RT_MAIL_E2=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE2&wait=6")
body_has "the probe is redelivered"            'timeout probe' "$RT_MAIL_E2"
RT_MID_E2=$(printf '%s' "$RT_MAIL_E2" | rt_deliv id)
if [ -n "$RT_MID_E2" ] && [ "$RT_MID_E2" != "$RT_MID_E1" ]; then
  pass_msg "redelivery carries a NEW delivery id ($RT_MID_E1 -> $RT_MID_E2)"
else
  fail_msg "redelivery id not fresh (e1='$RT_MID_E1' e2='$RT_MID_E2')"
fi
RT_ACK_E="{\"lease\":\"$LEASE2\",\"ids\":[$(printf '%s' "$RT_MAIL_E2" | rt_deliv jsonids)]}"
check "ack all redelivered ids -> 200"         200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ACK_E" $BASE/local/transport/ack)"

# --- F: attest-time permission cards + the delivery dialog belt (Phase 3) --
# An attest whose screen_tail shows a permission dialog RAISES the attention card
# — judged by looksLikePrompt on the ATTESTED tail (not the client's advisory
# prompt_open flag), the primary rung for a remote agent. That same dialog makes
# Ready() false (the paneShowsDialog belt), so nothing is typed into it; an
# approval is keyed through the same drain/ack as a delivery; and verifyPrompt
# clears the card once clean tails are attested. The dialog tail below is
# duplicated in remote_test.go (rtDialogTail), which asserts it genuinely
# satisfies looksLikePrompt so this fixture can never silently rot.

# Re-hello for a guaranteed-fresh lease: E's ack-timeout dance leaves LEASE2 at
# the edge of the 3s TTL, and a live-remote re-hello returns the SAME contact id
# (identity survives) on a brand-new lease — the client's ordinary reconnect.
RT_HELLO3=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_HELLO_BODY" $BASE/local/transport/hello)
LEASE3=$(printf '%s' "$RT_HELLO3" | rt_field lease)
RCID3=$(printf '%s' "$RT_HELLO3" | rt_agent0)
check "live re-hello keeps the same contact id" "$RCID" "$RCID3"

# Decision 4, both halves: the PHONE api (not just /local/contacts) surfaces
# where an agent lives, so the buddy list can tell a vterm from a tmux window.
RT_STATUS_F=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "api/status surfaces the remote transport" '"transport":"remote"' "$RT_STATUS_F"
body_has "api/status surfaces the client flavor"    '"transport_flavor":"sim"' "$RT_STATUS_F"

# A dialog tail that GENUINELY satisfies looksLikePrompt: the ❯ selector, a
# line-anchored numbered option (" ❯ 1. Yes"), and proceed vocabulary ("Do you
# want to proceed?"). Built with python3 so the newlines and multibyte ❯ survive
# JSON encoding intact. prompt_open:true here is the client's ADVISORY flag — the
# daemon ignores it and judges the tail itself.
RT_DIALOG_TAIL='Bash(git push origin main)
Do you want to proceed?
 ❯ 1. Yes
   2. No, and tell Claude what to do differently'
RT_ATTEST_DIALOG=$(RT_L="$LEASE3" RT_C="$RCID" RT_T="$RT_DIALOG_TAIL" python3 -c 'import json,os
print(json.dumps({"lease":os.environ["RT_L"],"states":[{"id":os.environ["RT_C"],"ready":True,"prompt_open":True,"screen_tail":os.environ["RT_T"]}]}))')
RT_ATTEST_CLEAN3="{\"lease\":\"$LEASE3\",\"states\":[{\"id\":\"$RCID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"

# rt_card: sim-otter's prompt_open off /local/contacts as open|shut (blank if gone).
rt_card() { curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts | python3 -c 'import json,sys
try: data=json.load(sys.stdin)
except Exception: sys.exit(0)
for c in (data.get("contacts") if isinstance(data,dict) else data) or []:
    if c.get("name")=="sim-otter": print("open" if c.get("prompt_open") else "shut")'; }

# The dialog attest both raises the card and (belt) makes Ready() false.
check "attest the dialog tail -> 200"          200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_DIALOG" $BASE/local/transport/attest)"

# CARD: the daemon raised prompt_open from the ATTESTED tail. Poll the roster;
# keepalive-attest the dialog each turn to hold the 3s lease and the card together.
RT_CARD=n
for _ in $(seq 10); do
  [ "$(rt_card 2>/dev/null)" = open ] && { RT_CARD=y; break; }
  curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_DIALOG" $BASE/local/transport/attest
  sleep 0.5
done
if [ "$RT_CARD" = y ]; then
  pass_msg "attest-time card raised (sim-otter prompt_open true)"
else
  fail_msg "attest-time card never raised prompt_open for sim-otter"
fi

# STALE-CAPTION FIX (#15): the card's caption (prompt_sig) is the COMMAND, and
# it must FOLLOW a dialog swapped for a different command — else an approval
# lands on a command the human never read. rt_sig reads sim-otter's prompt_sig.
rt_sig() { curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts | python3 -c 'import json,sys
try: data=json.load(sys.stdin)
except Exception: sys.exit(0)
for c in (data.get("contacts") if isinstance(data,dict) else data) or []:
    if c.get("name")=="sim-otter": print(c.get("prompt_sig") or "")'; }
body_has "card caption is dialog A'\''s command"    "git push origin main" "$(rt_sig)"
# Attest a DIFFERENT command under the same still-open card.
RT_DIALOG_TAIL_B='Bash(rm -rf build)
Do you want to proceed?
 ❯ 1. Yes
   2. No, and tell Claude what to do differently'
RT_ATTEST_DIALOG_B=$(RT_L="$LEASE3" RT_C="$RCID" RT_T="$RT_DIALOG_TAIL_B" python3 -c 'import json,os
print(json.dumps({"lease":os.environ["RT_L"],"states":[{"id":os.environ["RT_C"],"ready":True,"prompt_open":True,"screen_tail":os.environ["RT_T"]}]}))')
RT_REFRESH=n
for _ in $(seq 10); do
  curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_DIALOG_B" $BASE/local/transport/attest
  case "$(rt_sig 2>/dev/null)" in *"rm -rf build"*) RT_REFRESH=y; break;; esac
  sleep 0.5
done
if [ "$RT_REFRESH" = y ]; then
  pass_msg "card caption refreshed to the new command (no stale approval text)"
else
  fail_msg "card caption stayed stale after the dialog changed"
fi
[ "$(rt_card 2>/dev/null)" = open ] \
  && pass_msg "prompt stayed open across the refresh (approve gate never dropped)" \
  || fail_msg "prompt_open dropped during the caption refresh"

# BELT: with the dialog still attested (Ready false), a phone send is HELD — the
# trailing Enter must never blind-select the highlighted option (C2/C4, remote).
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_DIALOG" $BASE/local/transport/attest  # keepalive
RT_SEND_F="{\"agent\":\"$RCID\",\"text\":\"belt held by dialog\"}"
curl -s -o /dev/null "${DEV_AUTH[@]}" "${J[@]}" -d "$RT_SEND_F" $BASE/api/send
sleep 1
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_DIALOG" $BASE/local/transport/attest  # keepalive
RT_MAIL_F=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE3&wait=1")
body_has "dialog belt holds the send (nothing parked)" '"deliveries":[]' "$RT_MAIL_F"

# APPROVE LOOP: with the card up, /api/approve keys "1". Remote SendKey shares
# Deliver's ack discipline, so the endpoint BLOCKS until the client acks — run it
# backgrounded, drain the parked key, ack it, then reap and assert 200.
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_DIALOG" $BASE/local/transport/attest  # keepalive: fresh lease for the blocking key
RT_APPROVE_F="{\"agent\":\"$RCID\",\"key\":\"1\"}"
RT_APPROVE_CODE="$TESTDIR/rt-approve-code"
( code "${DEV_AUTH[@]}" "${J[@]}" -d "$RT_APPROVE_F" $BASE/api/approve > "$RT_APPROVE_CODE" ) &
RT_APPROVE_PID=$!
RT_KEY_MAIL=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE3&wait=2")
body_has "approve parks the key delivery"      '"key":"1"' "$RT_KEY_MAIL"
RT_KEY_ID=$(printf '%s' "$RT_KEY_MAIL" | rt_deliv id)
RT_KEY_ACK="{\"lease\":\"$LEASE3\",\"ids\":[\"$RT_KEY_ID\"]}"
check "ack the approval key -> 200"            200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_KEY_ACK" $BASE/local/transport/ack)"
wait "$RT_APPROVE_PID"
check "the blocked approve returned 200"       200 "$(cat "$RT_APPROVE_CODE")"

# BELT part 2 + CLEAR: a clean attest flips Ready() true (the held line now parks
# and delivers) AND starts verifyPrompt's two-strike clear of the card.
check "attest a clean tail -> 200"             200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_CLEAN3" $BASE/local/transport/attest)"
RT_MAIL_F2=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$LEASE3&wait=6")
body_has "the held line delivers once the belt lifts" 'belt held by dialog' "$RT_MAIL_F2"
RT_MID_F2=$(printf '%s' "$RT_MAIL_F2" | rt_deliv id)
RT_ACK_F2="{\"lease\":\"$LEASE3\",\"ids\":[\"$RT_MID_F2\"]}"
check "ack the freed line -> 200"              200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ACK_F2" $BASE/local/transport/ack)"

# CLEAR: keep attesting clean tails — verifyPrompt clears the card via the SAME
# looksLikePrompt judge that raised it (two misses at ~2s ticks). The keepalive
# holds the lease so this exercises the verifyPrompt clear, not an offline strike.
RT_CLEARED=n
for _ in $(seq 20); do
  curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$RT_ATTEST_CLEAN3" $BASE/local/transport/attest
  [ "$(rt_card 2>/dev/null)" = shut ] && { RT_CLEARED=y; break; }
  sleep 0.5
done
if [ "$RT_CLEARED" = y ]; then
  pass_msg "verifyPrompt cleared the card (prompt_open false again)"
else
  fail_msg "the card never cleared after clean attests"
fi

# --- fan-out stagger: the thundering-herd fix (429 defence) ----------------
# A #crew message and a plugin nudge storm are the two paths that deliver ONE
# event to MANY live agents at once — N Claude processes then continue their turn
# and hit the API in the same instant, the burst that drew the server-side 429s.
# The daemon now STAGGERS those fan-outs: live recipient k's in-memory delivery
# timer is armed at min(k*step,max)+jitter (step = BRIDGE_FANOUT_STAGGER_MS = 150ms
# at boot, so the spread is observable fast). Two invariants under test:
#   (a) NOTHING DROPPED — every live crew member still receives the message. The
#       stagger delays only the in-memory timer; the durable mailbox is the buffer,
#       so a fan-out to N live agents always reaches all N.
#   (b) 1:1 UNCHANGED — a direct phone->agent send never routes through the fan-out
#       path, so it delivers on the ordinary coalesce window, no added latency.
# Live agents are simulated on the remote/sim transport (a smoke daemon's only
# "live"), reusing the rt_* helpers above. Fresh contacts + leases, so this stands
# apart from the section above. Placed before lockdown, which stops the daemon.

# rt_agents: every agents[].id from a hello response on stdin, one per line.
rt_agents() { python3 -c 'import json,sys
try: a=json.load(sys.stdin).get("agents",[])
except Exception: a=[]
print("\n".join(x.get("id","") for x in a))'; }

# hello THREE fresh sim agents on one lease — a simulated live crew.
FO_HELLO_BODY="{\"transport\":\"sim\",\"agents\":[{\"name\":\"crew-ant\",\"directory\":\"$RT_DIR/fa\",\"session_id\":\"\"},{\"name\":\"crew-bee\",\"directory\":\"$RT_DIR/fb\",\"session_id\":\"\"},{\"name\":\"crew-cat\",\"directory\":\"$RT_DIR/fc\",\"session_id\":\"\"}]}"
FO_HELLO=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$FO_HELLO_BODY" $BASE/local/transport/hello)
FO_LEASE=$(printf '%s' "$FO_HELLO" | rt_field lease)
FO_IDS=$(printf '%s' "$FO_HELLO" | rt_agents)
check "fan-out hello registered 3 live agents" 3 "$(printf '%s\n' "$FO_IDS" | grep -c .)"

# attest all three ready on the lease (build the states array with python3).
FO_ATTEST=$(FO_L="$FO_LEASE" FO_I="$FO_IDS" python3 -c 'import json,os
ids=[x for x in os.environ["FO_I"].split("\n") if x]
print(json.dumps({"lease":os.environ["FO_L"],"states":[{"id":i,"ready":True,"prompt_open":False,"screen_tail":""} for i in ids]}))')
check "attest 3 fan-out agents ready -> 200"   200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$FO_ATTEST" $BASE/local/transport/attest)"

# Phone -> #crew: fans out to the whole roster; the 3 live members get staggered
# delivery, everyone offline gets a durable queue. Accepts 200 (round-2 rule).
FO_TEXT="crew fanout stagger marker"
FO_ROOM_BODY="{\"agent\":\"room:crew\",\"text\":\"$FO_TEXT\"}"
check "phone -> #crew fan-out accepted -> 200"  200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$FO_ROOM_BODY" $BASE/api/send)"

# (a) Collect: drain the shared lease outbox, record which contact ids received the
# marker, ack them, and keepalive-attest each turn (holds the 3s lease + clears any
# ack-timeout suspect so a redelivery still lands). Every live member must appear —
# proof the stagger drops nothing.
FO_SEEN="$TESTDIR/fo-seen"; : > "$FO_SEEN"
for _ in $(seq 40); do
  curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$FO_ATTEST" $BASE/local/transport/attest
  FO_MAIL=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$FO_LEASE&wait=1")
  FO_ACK=$(printf '%s' "$FO_MAIL" | FO_T="$FO_TEXT" SEEN="$FO_SEEN" python3 -c 'import json,os,sys
try: d=json.load(sys.stdin).get("deliveries",[])
except Exception: d=[]
with open(os.environ["SEEN"],"a") as f:
    for x in d:
        if os.environ["FO_T"] in (x.get("text") or ""): f.write((x.get("contact") or "")+"\n")
print(",".join(json.dumps(x.get("id","")) for x in d if x.get("id")))')
  [ -n "$FO_ACK" ] && curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "{\"lease\":\"$FO_LEASE\",\"ids\":[$FO_ACK]}" $BASE/local/transport/ack
  [ "$(sort -u "$FO_SEEN" | grep -c .)" -ge 3 ] && break
done
check "every live crew member received the fan-out (nothing dropped)" 3 "$(sort -u "$FO_SEEN" | grep -c .)"

# (b) A 1:1 phone->agent send is NOT staggered — it rides plain holdInbound (the
# unchanged path) and delivers on the ordinary coalesce window. Fresh agent+lease,
# registered AFTER the fan-out above so it was never a member of it.
FO_SOLO_BODY="{\"transport\":\"sim\",\"agents\":[{\"name\":\"solo-elk\",\"directory\":\"$RT_DIR/solo\",\"session_id\":\"\"}]}"
FO_SOLO=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$FO_SOLO_BODY" $BASE/local/transport/hello)
FO_SLEASE=$(printf '%s' "$FO_SOLO" | rt_field lease)
FO_SID=$(printf '%s' "$FO_SOLO" | rt_agent0)
FO_SATTEST="{\"lease\":\"$FO_SLEASE\",\"states\":[{\"id\":\"$FO_SID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$FO_SATTEST" $BASE/local/transport/attest
FO_11_BODY="{\"agent\":\"$FO_SID\",\"text\":\"one to one direct\"}"
check "1:1 phone send accepted -> 200"          200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$FO_11_BODY" $BASE/api/send)"
FO_11_OK=n
for _ in $(seq 20); do
  curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$FO_SATTEST" $BASE/local/transport/attest
  FO_11_MAIL=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$FO_SLEASE&wait=1")
  case "$FO_11_MAIL" in
    *'one to one direct'*)
      FO_11_MID=$(printf '%s' "$FO_11_MAIL" | rt_deliv id)
      curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "{\"lease\":\"$FO_SLEASE\",\"ids\":[\"$FO_11_MID\"]}" $BASE/local/transport/ack
      FO_11_OK=y; break ;;
  esac
done
if [ "$FO_11_OK" = y ]; then
  pass_msg "1:1 send delivered on the unchanged path (never staggered)"
else
  fail_msg "1:1 send never delivered on the unchanged path"
fi

# --- context gauge: the /compact action (WORK ITEM 2) ----------------------
# The phone triggers /compact on an IDLE agent. The daemon types a COMPILE-TIME
# CONSTANT "/compact" and nothing else — the request body carries no command
# string (a fixed-vocabulary action, like the approve keys; never raw
# passthrough). Gating: unknown/offline -> 400; not idle (a busy pane, or a
# permission prompt open) -> 409 busy; idle live -> 200 and the EXACT literal
# "/compact" is parked for the client. Live agents are the remote/sim transport
# (a smoke daemon's only "live"), reusing the rt_* helpers. Placed before
# lockdown, which stops the daemon these checks need alive.

# unknown agent -> 400 offline (needs no agent state).
check "compact unknown agent -> 400"           400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d '{"agent":"ghost-nobody"}' $BASE/api/compact)"

# a fresh idle sim agent on its own lease.
CO_HELLO_BODY="{\"transport\":\"sim\",\"agents\":[{\"name\":\"compact-owl\",\"directory\":\"$RT_DIR/co\",\"session_id\":\"\"}]}"
CO_HELLO=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CO_HELLO_BODY" $BASE/local/transport/hello)
CO_LEASE=$(printf '%s' "$CO_HELLO" | rt_field lease)
CO_ID=$(printf '%s' "$CO_HELLO" | rt_agent0)
CO_ATTEST_READY="{\"lease\":\"$CO_LEASE\",\"states\":[{\"id\":\"$CO_ID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
CO_ATTEST_NOTREADY="{\"lease\":\"$CO_LEASE\",\"states\":[{\"id\":\"$CO_ID\",\"ready\":false,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
CO_BODY="{\"agent\":\"$CO_ID\"}"

# IDLE happy path: attest ready+clean -> the agent is idle (Health ok, Ready
# true). /api/compact then delivers the CONSTANT; remote Deliver blocks on the
# client ack (like /api/approve), so background it, drain the parked line, assert
# it is EXACTLY "/compact" (constant-only — nothing request-supplied), ack, reap.
check "attest compact-owl ready -> 200"        200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CO_ATTEST_READY" $BASE/local/transport/attest)"
CO_CODE="$TESTDIR/co-code"
( code "${DEV_AUTH[@]}" "${J[@]}" -d "$CO_BODY" $BASE/api/compact > "$CO_CODE" ) &
CO_PID=$!
CO_MAIL=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$CO_LEASE&wait=2")
body_has "compact parks the EXACT literal /compact" '"text":"/compact"' "$CO_MAIL"
CO_MID=$(printf '%s' "$CO_MAIL" | rt_deliv id)
CO_ACK="{\"lease\":\"$CO_LEASE\",\"ids\":[\"$CO_MID\"]}"
check "ack the compact command -> 200"         200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CO_ACK" $BASE/local/transport/ack)"
wait "$CO_PID"
check "the idle compact returned 200"          200 "$(cat "$CO_CODE")"
HIST_BODY=$(curl -s "${DEV_AUTH[@]}" "$BASE/api/history?since=0")
body_has "history reflects the compact"        '"type":"compacted"' "$HIST_BODY"

# BUSY (the Ready half of the gate): a not-ready attest means the pane is working
# -> 409 busy, and nothing is typed. The attest refreshes the 3s lease.
check "attest compact-owl not-ready -> 200"    200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CO_ATTEST_NOTREADY" $BASE/local/transport/attest)"
check "compact a not-ready (busy) agent -> 409" 409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$CO_BODY" $BASE/api/compact)"
CO_MAIL_B=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$CO_LEASE&wait=1")
body_has "the busy compact parked nothing"     '"deliveries":[]' "$CO_MAIL_B"

# BUSY (the prompt half of the gate): a dialog tail opens a permission card
# (Health -> prompt) AND makes Ready false via the belt -> 409 busy. Reuses
# RT_DIALOG_TAIL (proven to satisfy looksLikePrompt in remote_test.go).
CO_ATTEST_DIALOG=$(RT_L="$CO_LEASE" RT_C="$CO_ID" RT_T="$RT_DIALOG_TAIL" python3 -c 'import json,os
print(json.dumps({"lease":os.environ["RT_L"],"states":[{"id":os.environ["RT_C"],"ready":True,"prompt_open":True,"screen_tail":os.environ["RT_T"]}]}))')
check "attest compact-owl a dialog -> 200"     200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CO_ATTEST_DIALOG" $BASE/local/transport/attest)"
check "compact while a prompt is open -> 409"  409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$CO_BODY" $BASE/api/compact)"

# --- since-you-woke welcome-back digest (docs/route-health.md) --------------
# When a contact WAKES after being genuinely away (>= wake_digest_min_away_s, set
# to 6s in this daemon's config), the daemon leads its held backlog with ONE
# daemon-authored line summarizing the gap — a sense of time for the waking agent.
# It rides the SAME guarded park/drain/ack path as any delivery (no second typing
# path), is From-less (no forgeable "[From …]" head, no client text), and fires AT
# MOST ONCE per wake. Live agents are the remote/sim transport, reusing rt_*.
# Placed before lockdown (which stops the daemon these checks need alive).

# rt_status <name>: a contact's status off /local/contacts (blank if absent).
rt_status() { curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts | WNAME="$1" python3 -c 'import json,os,sys
try: data=json.load(sys.stdin)
except Exception: sys.exit(0)
for c in (data.get("contacts") if isinstance(data,dict) else data) or []:
    if c.get("name")==os.environ["WNAME"]: print(c.get("status",""))'; }

# hello wake-otter fresh + attest ready. A FIRST connect is never a wake (no
# trusted prior last-seen), so nothing is prepended here.
WK_HELLO_BODY="{\"transport\":\"sim\",\"agents\":[{\"name\":\"wake-otter\",\"directory\":\"$RT_DIR/wk\",\"session_id\":\"\"}]}"
WK_HELLO=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_HELLO_BODY" $BASE/local/transport/hello)
WK_LEASE=$(printf '%s' "$WK_HELLO" | rt_field lease)
WK_ID=$(printf '%s' "$WK_HELLO" | rt_agent0)
if [ -n "$WK_LEASE" ] && [ -n "$WK_ID" ]; then
  pass_msg "wake digest: hello registered wake-otter ($WK_ID)"
else
  fail_msg "wake digest: hello returned no lease+id: $WK_HELLO"
fi
WK_ATTEST_READY="{\"lease\":\"$WK_LEASE\",\"states\":[{\"id\":\"$WK_ID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
check "wake digest: attest wake-otter ready -> 200" 200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_ATTEST_READY" $BASE/local/transport/attest)"

# Stop attesting -> the lease goes stale (ttl 3s) and wake-otter strikes OFFLINE.
WK_DEAD=n
for _ in $(seq 60); do
  [ "$(rt_status wake-otter 2>/dev/null)" = offline ] && { WK_DEAD=y; break; }
  sleep 0.25
done
[ "$WK_DEAD" = y ] && pass_msg "wake digest: wake-otter struck offline (lease stale)" \
                   || fail_msg "wake digest: wake-otter never went offline"

# While offline, queue a backlog: one DIRECT phone message + one #crew message.
# Both land durably in wake-otter's mailbox — Room "" vs "#crew", the split the
# digest counts as "N direct" / "N in #crew".
WK_DIRECT_BODY="{\"agent\":\"$WK_ID\",\"text\":\"direct while away\"}"
check "wake digest: direct to offline wake-otter queued -> 409" 409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$WK_DIRECT_BODY" $BASE/api/send)"
WK_CREW_BODY='{"agent":"room:crew","text":"crew while away"}'
check "wake digest: #crew fan-out accepted -> 200" 200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$WK_CREW_BODY" $BASE/api/send)"

# Wait past the 6s threshold, THEN re-hello (same name+dir -> SAME id, the offline
# contact adopted). This transition is a genuine wake: the digest leads the backlog.
sleep 8
WK_HELLO2=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_HELLO_BODY" $BASE/local/transport/hello)
WK_LEASE2=$(printf '%s' "$WK_HELLO2" | rt_field lease)
check "wake digest: re-hello keeps the same contact id" "$WK_ID" "$(printf '%s' "$WK_HELLO2" | rt_agent0)"
WK_ATTEST_READY2="{\"lease\":\"$WK_LEASE2\",\"states\":[{\"id\":\"$WK_ID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
check "wake digest: attest on the revived lease -> 200" 200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_ATTEST_READY2" $BASE/local/transport/attest)"

# The reconcile loop flushes; remote Deliver BLOCKS per group on ack, so the
# digest lead-in parks first (its own From-less group), then the backlog. Drain in
# ONE loop, acking each batch IMMEDIATELY (well within the 2s ack window) so the
# transport never redelivers — then the digest text appears exactly once and the
# count is exact. Keepalive-attest each turn to hold the 3s lease. WK_DIGEST
# captures the digest's own batch (it parks alone, so no backlog [From ...] frame
# rides with it — the no-forgeable-head assertion is scoped to it).
WK_ALL="$TESTDIR/wk-all"; : > "$WK_ALL"
WK_DIGEST=""
for _ in $(seq 40); do
  curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_ATTEST_READY2" $BASE/local/transport/attest
  WK_M=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$WK_LEASE2&wait=2")
  WK_ACK_IDS=$(printf '%s' "$WK_M" | rt_deliv jsonids)
  if [ -n "$WK_ACK_IDS" ]; then
    printf '%s\n' "$WK_M" >> "$WK_ALL"
    case "$WK_M" in *'back after'*) WK_DIGEST="$WK_M";; esac
    curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "{\"lease\":\"$WK_LEASE2\",\"ids\":[$WK_ACK_IDS]}" $BASE/local/transport/ack
  fi
  grep -q 'back after' "$WK_ALL" && grep -q 'direct while away' "$WK_ALL" && grep -q 'crew while away' "$WK_ALL" && break
done
# The lead-in: the ⏱ back-after clock with both daemon-computed counts.
body_has "wake digest: leads with the back-after clock"   'back after'          "$WK_DIGEST"
body_has "wake digest: reads 'while you were away'"        'while you were away' "$WK_DIGEST"
body_has "wake digest: counts the 1 #crew message"        '1 in #crew'          "$WK_DIGEST"
body_has "wake digest: counts the 1 direct message"       '1 direct'            "$WK_DIGEST"
# Daemon-authored: the digest's own delivery carries NO forgeable provenance head
# (the backlog frames DO wear "[From …]" — correctly — so this is scoped to WK_DIGEST).
case "$WK_DIGEST" in
  *'[From '*) fail_msg "wake digest carried a [From ...] head (must be daemon-authored/From-less)" ;;
  *)          pass_msg "wake digest is daemon-authored (no [From ...] head, no client text)" ;;
esac
# The real backlog delivered BEHIND the lead-in, and exactly ONE digest fired.
body_has "wake digest: the direct backlog delivered behind the lead-in" 'direct while away' "$(cat "$WK_ALL")"
body_has "wake digest: the #crew backlog delivered behind the lead-in"  'crew while away'   "$(cat "$WK_ALL")"
WK_DIGCOUNT=$(grep -o 'back after' "$WK_ALL" | wc -l | tr -d ' ')
check "wake digest: exactly ONE digest across the whole wake" 1 "$WK_DIGCOUNT"

# NEGATIVE — live-guard/debounce: wake-otter is LIVE now, so a routine re-hello is
# NOT a wake and must add no second digest (mailbox drained; nothing new should park).
WK_HELLO3=$(curl -s "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_HELLO_BODY" $BASE/local/transport/hello)
WK_LEASE3=$(printf '%s' "$WK_HELLO3" | rt_field lease)
check "wake digest: live re-hello keeps the same id" "$WK_ID" "$(printf '%s' "$WK_HELLO3" | rt_agent0)"
WK_ATTEST_READY3="{\"lease\":\"$WK_LEASE3\",\"states\":[{\"id\":\"$WK_ID\",\"ready\":true,\"prompt_open\":false,\"screen_tail\":\"\"}]}"
curl -s -o /dev/null "${J[@]}" "${LOCAL_AUTH[@]}" -d "$WK_ATTEST_READY3" $BASE/local/transport/attest
WK_MAIL_N=$(curl -s "${LOCAL_AUTH[@]}" "$BASE/local/transport/mail?lease=$WK_LEASE3&wait=2")
case "$WK_MAIL_N" in
  *'back after'*) fail_msg "a live re-hello produced a SECOND wake digest (live-guard/debounce failed)" ;;
  *)              pass_msg "a live re-hello of a woken agent produces no second digest" ;;
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

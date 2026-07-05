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
DUP_BODY="{\"agent\":\"ghost-dup\",\"text\":\"dup test\",\"client_id\":\"cid-smoke-1\"}"
CONNECT_BODY="{\"name\":\"$CNAME\",\"directory\":\"/private/tmp/smoke-bridge\",\"session_id\":\"sess-smoke\",\"tmux_target\":\"bridge-smoke-nonexistent:0\"}"
APPROVE_BADKEY='{"agent":"ghost-nobody","key":"q"}'
APPROVE_OFFLINE='{"agent":"ghost-nobody","key":"1"}'
SUB_BODY='{"endpoint":"https://push.example.com/smoke-endpoint-xyz","keys":{"p256dh":"BFakeKeyForSmokeTestingPurposesOnly","auth":"ZmFrZWF1dGg"}}'
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

# --- register a fake contact via the local API ----------------------------
check "local/connect registers contact"   200 "$(code "${J[@]}" "${LOCAL_AUTH[@]}" -d "$CONNECT_BODY" $BASE/local/connect)"
STATUS_BODY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/status)
body_has "status now lists the contact"   "\"name\":\"$CNAME\"" "$STATUS_BODY"
CONTACTS_BODY=$(curl -s "${LOCAL_AUTH[@]}" $BASE/local/contacts)
body_has "local/contacts lists the contact" "\"name\":\"$CNAME\"" "$CONTACTS_BODY"
check "local/contacts needs local token"  401 "$(code $BASE/local/contacts)"

# --- send: offline, idempotency, validation -------------------------------
check "send to unknown contact -> 409"    409 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$GHOST_BODY" $BASE/api/send)"
check "empty send rejected -> 400"        400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$EMPTY_BODY" $BASE/api/send)"
# First send records the client_id (returns 409 since the agent is offline);
# the retry with the SAME client_id must be acked as a duplicate, not resent.
curl -s -o /dev/null "${DEV_AUTH[@]}" "${J[@]}" -d "$DUP_BODY" $BASE/api/send
DUP_RESP=$(curl -s "${DEV_AUTH[@]}" "${J[@]}" -d "$DUP_BODY" $BASE/api/send)
body_has "duplicate client_id acked"      '"duplicate":true' "$DUP_RESP"

# --- approve gating -------------------------------------------------------
check "approve rejects bad key -> 400"    400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$APPROVE_BADKEY" $BASE/api/approve)"
check "approve on offline agent -> 400"   400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$APPROVE_OFFLINE" $BASE/api/approve)"

# --- web push -------------------------------------------------------------
check "push key -> 200"                    200 "$(code "${DEV_AUTH[@]}" $BASE/api/push/key)"
PUSH_KEY=$(curl -s "${DEV_AUTH[@]}" $BASE/api/push/key | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
if [ "${#PUSH_KEY}" -ge 80 ]; then
  pass_msg "VAPID public key looks valid (len ${#PUSH_KEY})"
else
  fail_msg "VAPID key too short (len ${#PUSH_KEY}): '$PUSH_KEY'"
fi
check "push subscribe -> 200"              200 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$SUB_BODY" $BASE/api/push/subscribe)"
check "push subscribe no endpoint -> 400"  400 "$(code "${DEV_AUTH[@]}" "${J[@]}" -d "$SUB_BAD" $BASE/api/push/subscribe)"

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

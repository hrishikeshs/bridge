package main

// httpapi.go — the device API a paired phone speaks: pairing, status,
// history, the SSE event stream, send/interrupt/approve, and photo upload.
// Every handler here sits behind route's identity + device-token gates.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// handlePair redeems a pairing code and, on success, sets the device token as
// an HttpOnly Secure SameSite=Lax cookie.
func handlePair(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	_ = json.Unmarshal(data, &req)

	token := tryPair(req.Code, req.Device)
	if token == "" {
		audit("pair-failed", "", id)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad-code"})
		return
	}
	audit("paired", req.Device, id)
	http.SetCookie(w, &http.Cookie{
		Name:     "bridge_token",
		Value:    token,
		Path:     "/",
		MaxAge:   31536000,
		HttpOnly: true,
		Secure:   true,
		// Lax, not Strict: standalone iOS PWAs drop Strict cookies on
		// EventSource/background requests, breaking the live event stream.
		// The tailnet perimeter is the real CSRF defense, and no GET mutates.
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStatus returns the roster and daemon version.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Directory string            `json:"directory"`
		Status    string            `json:"status"`
		Health    string            `json:"health"`
		Attention bool              `json:"attention"`
		Away      string            `json:"away,omitempty"`   // agent's self-set status line
		Fields    map[string]string `json:"fields,omitempty"` // plugin annotations
		// Where the agent lives — omitted for the tmux default; "remote" plus a
		// client-supplied flavor ("emacs") for client-hosted agents. The half of
		// Decision 4 (docs/transports.md) that registry.go promised the phone;
		// until now only /local/contacts told the truth.
		Transport string `json:"transport,omitempty"`
		Flavor    string `json:"transport_flavor,omitempty"`
		// ContextPct is the agent's context-window usage, 0–100, derived passively
		// from its session-JSONL usage line (context.go). Omitted (omitempty) when
		// unknown — no usage parsed yet, or a sessionless agent — so the phone hides
		// the bar rather than drawing a misleading 0%.
		ContextPct int `json:"context_pct,omitempty"`
		// HoldReason and LastSeenS are Route Health Layer 1 (routehealth.go): a
		// DERIVED, honest delivery state so a stuck route stops reading a sticky
		// health:"ok". HoldReason is "" (omitempty → hidden) unless mail is genuinely
		// held — "stale"/"unconfirmed"/"at-prompt"/"busy"/"stalled"; LastSeenS is the
		// seconds since a remote route last attested (0/hidden for tmux). Purely
		// additive: Health above is untouched, so every existing consumer stays
		// byte-identical; the PWA composes the dot color from these in a later change.
		HoldReason string `json:"hold_reason,omitempty"`
		LastSeenS  int    `json:"last_seen_s,omitempty"`
	}
	items := []item{}
	for _, c := range registry.Roster() {
		hr, ls := deriveRouteHealth(c)
		items = append(items, item{c.ID, c.Name, c.Directory, c.Status, c.Health, c.PromptOpen, c.Away, c.Fields, c.Transport, c.TransportFlavor, contextPct(c.ContextTokens, c.ContextModel), hr, ls})
	}
	// Clocks for phone-side presence truth (round 4): the phone compares `now`
	// to its own clock and its last-contact timestamp to distinguish "my
	// network is down" from "Mac unreachable"; `started` reveals a daemon
	// restart; `wake_from`/`wake_to` surface the most recent sleep window so a
	// reconnecting phone can say "Mac was asleep 10:02–12:55" instead of a bare
	// "unreachable".
	resp := map[string]any{
		"contacts":  items,
		"rooms":     roomList(), // static for v1: the one built-in #crew party line
		"version":   version,
		"now":       timeNowUnix(),
		"started":   daemonStartUnix,
		"my_status": myStatus(), // the human's away line, for the settings sheet
	}
	if from, to := wakeGap(); to != 0 {
		resp["wake_from"], resp["wake_to"] = from, to
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHistory returns stored events newer than ?since=N.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	evs := eventsSince(since)
	if evs == nil {
		evs = []Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// handleEvents streams Server-Sent Events, replaying anything past the client's
// cursor (Last-Event-ID header or ?since) before switching to live delivery.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	since := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}

	// Register before replaying so events emitted during replay are not lost;
	// an event landing in both is harmless (the client dedups by id).
	client := &sseClient{ch: make(chan string, 64)}
	addSSEClient(client)
	defer removeSSEClient(client)

	for _, ev := range eventsSince(since) {
		_, _ = io.WriteString(w, frameFor(ev))
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-client.ch:
			if !ok {
				return
			}
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleSend delivers a phone message to a live contact, or mailboxes it and
// returns 409 when the contact is offline.
func handleSend(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent    string `json:"agent"`
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
		Quote    *struct {
			Name    string `json:"name"`
			Excerpt string `json:"excerpt"`
		} `json:"quote"`
	}
	_ = json.Unmarshal(data, &req)

	if strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty"})
		return
	}
	if len(req.Text) > maxMessageLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too-long"})
		return
	}
	// An optional reply-quote. Clamp+sanitize it AUTHORITATIVELY here (the
	// client's clamp is cosmetic): the excerpt is body-derived and must not be
	// able to forge a provenance frame (H9). The quote rides INLINE in the
	// delivered/queued text (decorateQuote) so the mailbox and coalescer need no
	// schema change; the structured fields travel on the "sent" event for the
	// phone's inset. Both delivery branches below store the decorated text.
	var qName, qExcerpt string
	if req.Quote != nil {
		qName = sanitizeExcerpt(req.Quote.Name, quoteMaxNameRunes)
		qExcerpt = sanitizeExcerpt(req.Quote.Excerpt, quoteMaxExcerptRunes)
	}
	deliverText := decorateQuote(qName, qExcerpt, req.Text)

	// Party line: a send to a room fans out to every registered contact instead
	// of resolving one. The client_id claim/commit is identical to the 1:1 path
	// (a racing retry is a safe duplicate ack), and a durably-queued fan-out IS
	// success — so an all-offline crew still acks 200 (the round-2 rule); there
	// is no per-member "offline" 409 here. Branched before registry.Resolve
	// because a room is never a registered contact.
	if isRoom(req.Agent) {
		if !claimClientID(req.ClientID) {
			audit("send-duplicate-dropped", req.Text, id)
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
			return
		}
		fanoutRoom(authConfig.UserMention, "phone", deliverText, "")
		releaseClientID(req.ClientID, true) // durably queued to every member: a retry is a safe duplicate ack
		audit("send-room", req.Text, id)
		roomHumanSpoke(req.Agent) // a human message reopens every agent's speaking slot
		// Exactly one event carries the room message — the ORIGINAL text plus the
		// structured quote fields (like a 1:1 "sent"), keyed to the room id so the
		// phone folds it into the #crew thread. The fan-out mail is all
		// Emitted:true, so no member's flush emits a second bubble.
		emitEvent(Event{Type: "sent", Agent: roomCrewID, Name: roomCrewName, Text: req.Text, ClientID: req.ClientID, QuoteName: qName, QuoteExcerpt: qExcerpt})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Reserve the client_id before delivering: a completed send, or a racing
	// retry whose original delivery is still in flight, is acked as a duplicate
	// without re-delivering — closing the window where the phone's 10s-timeout
	// retry and the slow original both reach send-keys (#3). The claim is
	// released below: committed on a durable accept, dropped on failure so a
	// genuine retry runs for real (H1).
	if !claimClientID(req.ClientID) {
		audit("send-duplicate-dropped", req.Text, id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}

	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		if c != nil {
			registry.Queue(c.ID, MailMessage{From: authConfig.UserMention, Via: "phone", Text: deliverText, TS: nowUTC()})
			releaseClientID(req.ClientID, true) // durably queued: a retry must not re-queue
			// message.in fires on durable ACCEPT, not send-keys success — a
			// queued message is in the system (same standard H1 holds the
			// client_id to). data.queued lets plugins tell the paths apart.
			dispatchPluginEvent("message.in", c, map[string]any{"text": req.Text, "via": "phone", "queued": true})
		} else {
			releaseClientID(req.ClientID, false) // nothing durable happened: allow the retry
		}
		audit("send-offline", req.Agent, id)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}

	// Live contact: hold-and-batch instead of immediate send-keys. The message
	// is durably queued (mailbox) before the ack, so "ok" still means "the
	// daemon has it"; delivery follows when the burst window closes
	// (coalesce.go). Emitted:true — the "sent" event below is the one the
	// thread shows; the flush must not emit a duplicate.
	holdInbound(c, MailMessage{From: authConfig.UserMention, Via: "phone", Text: deliverText, TS: nowUTC(), Emitted: true})
	releaseClientID(req.ClientID, true) // durably queued: a retry is now a safe duplicate ack
	audit("send", c.Name+": "+req.Text, id)
	// The "sent" event carries the ORIGINAL text plus the structured quote
	// fields, so the phone renders the quote inset (live + history replay) while
	// the agent got the (re …) decoration inline above.
	emitEvent(Event{Type: "sent", Agent: c.ID, Name: c.Name, Text: req.Text, ClientID: req.ClientID, QuoteName: qName, QuoteExcerpt: qExcerpt})
	dispatchPluginEvent("message.in", c, map[string]any{"text": req.Text, "via": "phone", "queued": false})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// reactableType reports whether an event kind can carry a reaction — the inbound
// agent messages a phone taps back on.
func reactableType(t string) bool {
	return t == "reply" || t == "peer" || t == "mention"
}

// handleReact records a phone's emoji reaction to one agent message. It emits a
// durable "reaction" event (the phone folds it into a badge on the target
// bubble) and delivers the feedback inline to the agent so it FEELS the tap-back.
// Idempotent: the same emoji on the same message from the user re-emits nothing
// and re-delivers nothing.
func handleReact(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent   string `json:"agent"`
		EventID int64  `json:"event_id"`
		Emoji   string `json:"emoji"`
	}
	_ = json.Unmarshal(data, &req)

	// The phone may send ANY emoji now (the fixed 6-emoji whitelist is gone).
	// Sanitize + validate it before anything downstream touches it — it gets
	// typed into the agent's terminal (reactionDelivery), so an arbitrary field
	// must never carry control bytes, forge the framing alphabet, or be plain
	// text (never trust the phone; H9). Reassign the cleaned value so the dedup,
	// the stored badge event, and the delivery all ride the safe emoji.
	safeEmoji, okEmoji := reactionSafe(req.Emoji)
	if !okEmoji {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-emoji"})
		return
	}
	req.Emoji = safeEmoji
	// The target must be a real, reactable event in THIS agent's thread. Resolve
	// the handle to its contact id and match it against the stored event's Agent
	// (the thread it lives in), so a phone can't react across threads or onto a
	// non-message (an approval card, a status).
	c := registry.Resolve(req.Agent)
	target, found := eventByID(req.EventID)
	if c == nil || !found || target.Agent != c.ID || !reactableType(target.Type) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no-event"})
		return
	}
	// Idempotent: a double-tap (or two phones agreeing) is a quiet 200, no second
	// event and no second delivery.
	if reactionExists(req.EventID, authConfig.UserMention, req.Emoji) {
		audit("react-duplicate", c.Name+" "+req.Emoji, id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}
	audit("react", fmt.Sprintf("%s %s: %.60s", c.Name, req.Emoji, target.Text), id)
	// The durable record the phone folds into a badge (live SSE + history replay).
	emitEvent(Event{Type: "reaction", Agent: c.ID, Name: authConfig.UserMention, Text: req.Emoji, Target: req.EventID})
	// Let the agent feel it — a guarded, coalesced delivery (never a bare
	// send-keys) of a sanitized echo of its own line. Emitted:true so the flush
	// adds no "sent" bubble; the "reaction" event above is the only record.
	holdInbound(c, MailMessage{
		From:    authConfig.UserMention,
		Via:     "phone",
		Text:    reactionDelivery(req.Emoji, target.Text),
		TS:      nowUTC(),
		Emitted: true,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleInterrupt delivers a bare Escape to a live contact — stop mid-thought,
// from the phone (field request, 2026-07-06: rejecting permission prompts was
// the only interrupt available). Unlike approve, this is honored with or
// without an open prompt: Escape aborts a running turn, and on an open dialog
// it is the dialog's own cancel. Any held burst is then delivered immediately —
// the agent just went idle and should hear what was waiting.
func handleInterrupt(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal(data, &req)

	// A room is a fan-out thread, not a pane — there is no window to Escape.
	// (Resolve would 409 it anyway since rooms aren't registered; refuse
	// explicitly so the reason is "no-room", not a misleading "offline".)
	if isRoom(req.Agent) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no-room-interrupt"})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}
	if err := transportFor(c).SendKey(c, "esc"); err != nil {
		audit("interrupt-failed", err.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("interrupt", c.Name, id)
	Emit("interrupted", c.ID, c.Name, "")
	go coalesceDeliver(c.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleApprove delivers a whitelisted keystroke, honored only while the
// contact is hook-attested as prompting (no blind key injection).
func handleApprove(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent string `json:"agent"`
		Key   string `json:"key"`
	}
	_ = json.Unmarshal(data, &req)

	if !approveKeys[req.Key] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key-not-allowed"})
		return
	}
	// A room has no pane to key an approval into (and Resolve would 400 it anyway,
	// since rooms aren't registered) — refuse it explicitly.
	if isRoom(req.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no-room-approve"})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offline"})
		return
	}
	if !c.PromptOpen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not-waiting"})
		return
	}
	var deliverErr error
	if remoteUsesSemanticProtocol(c.ID) {
		deliverErr = deliverLegacySemanticApproval(c, req.Key)
	} else {
		deliverErr = transportFor(c).SendKey(c, req.Key)
	}
	if deliverErr != nil {
		audit("approve-failed", deliverErr.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": deliverErr.Error()})
		return
	}
	audit("approve", c.Name+" <- "+req.Key, id)
	Emit("approved", c.ID, c.Name, req.Key)
	// The lock-screen "needs you" notification is now stale; replace it with a
	// same-tag ✓ so it can't sit there demanding attention it already got.
	clearAttnPush(c.ID, c.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// compactCommand is the ONLY line /api/compact ever types — a compile-time
// constant. See handleCompact's safety invariant: no request-supplied text can
// reach the transport, this string is the whole vocabulary.
const compactCommand = "/compact"

// handleCompact triggers a context-window /compact on an IDLE agent — the phone's
// half of the context gauge. It is a FIXED-VOCABULARY action, exactly like the
// approve keys (1/2/3/y/n/esc), NOT raw passthrough (the user explicitly declined
// a raw shell).
//
// THE SAFETY INVARIANT (load-bearing): the request body carries NO command string
// — only which agent to compact. The daemon delivers the compile-time constant
// compactCommand ("/compact") and nothing else; there is no code path by which any
// request-supplied text reaches the transport's send-keys. If this handler ever
// grows a field that flows into Deliver, that is the one thing it must never do.
//
// Gate order, both required: the contact must be live (else 400 "offline"), and it
// must be IDLE — Health "ok" (never "working", never "prompt") AND its transport
// Ready this instant (a clean prompt, no open dialog) — else 409 "busy". Idle is
// deliberately STRICTER than Ready: we only compact an agent that has put its pen
// down, never one mid-turn or with a permission card up.
func handleCompact(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent string `json:"agent"` // the ONLY field — never a command string
	}
	_ = json.Unmarshal(data, &req)

	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offline"})
		return
	}
	// Idle only. Health must be a settled "ok" (not "working", not "prompt") AND
	// the transport must be Ready right now (clean prompt, no dialog). A working
	// or prompting agent is busy: 409, retry once it settles.
	if c.Health != "ok" || !transportFor(c).Ready(c) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "busy"})
		return
	}
	// Deliver the CONSTANT, submitted, straight through the transport. This is a
	// direct command, not a held chat message, so it does NOT route through the
	// coalescing mailbox (holdInbound): for tmux it types "/compact"+Enter, for a
	// remote client it parks "/compact" for the client to type, exactly as the
	// client types any daemon-delivered line. Nothing request-supplied is here.
	if err := transportFor(c).Deliver(c, compactCommand); err != nil {
		audit("compact-failed", c.Name+": "+err.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("compact", c.Name, id)
	Emit("compacted", c.ID, c.Name, "") // so the PWA/feed can reflect it
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

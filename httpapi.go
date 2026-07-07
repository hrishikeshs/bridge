package main

// httpapi.go — the device API a paired phone speaks: pairing, status,
// history, the SSE event stream, send/interrupt/approve, and photo upload.
// Every handler here sits behind route's identity + device-token gates.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
	}
	items := []item{}
	for _, c := range registry.Roster() {
		items = append(items, item{c.ID, c.Name, c.Directory, c.Status, c.Health, c.PromptOpen, c.Away, c.Fields})
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

// myStatusText holds the human's away line — the AIM auto-responder delivered to
// an agent the moment it messages the phone (handleLocalSend). It is persisted
// under ~/.bridge/mystatus.json so it survives a daemon restart, guarded by
// myStatusMu since both the HTTP handler and the send path touch it.
var (
	myStatusMu   sync.Mutex
	myStatusText string
)

// myStatusFile is the on-disk shape: an object, not a bare string, so the file
// can gain fields later without a format break (matching tokens.json's shape).
type myStatusFile struct {
	Text string `json:"text"`
}

// loadMyStatus restores the human's away line at startup (called from runServe).
// A missing or unparseable file leaves it empty — the secure, quiet default.
func loadMyStatus() {
	myStatusMu.Lock()
	defer myStatusMu.Unlock()
	data, err := os.ReadFile(bridgePath("mystatus.json"))
	if err != nil {
		return
	}
	var f myStatusFile
	if json.Unmarshal(data, &f) == nil {
		myStatusText = f.Text
	}
}

// setMyStatus records the human's away line and persists it 0600. Empty clears.
func setMyStatus(text string) {
	myStatusMu.Lock()
	defer myStatusMu.Unlock()
	myStatusText = text
	data, _ := json.Marshal(myStatusFile{Text: text})
	_ = writeFilePrivate(bridgePath("mystatus.json"), data)
}

// myStatus returns the current human away line ("" when none).
func myStatus() string {
	myStatusMu.Lock()
	defer myStatusMu.Unlock()
	return myStatusText
}

// handleMyStatus sets (or clears, on empty text) the human's away line. It is
// device-token authed like every /api mutation; the value is flattened/capped
// exactly like an agent status (clampAway), surfaced on /api/status as
// my_status, and pushed live as a "mystatus" event so every open phone syncs.
func handleMyStatus(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(data, &req)

	text := clampAway(req.Text)
	setMyStatus(text)
	if text == "" {
		audit("mystatus-clear", "", id)
	} else {
		audit("mystatus", text, id)
	}
	Emit("mystatus", "", authConfig.UserMention, text)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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

// reactionEmoji is the closed set of reactions /api/react accepts — the phone
// offers exactly these; anything else is a 400.
var reactionEmoji = map[string]bool{
	"👍": true, "❤️": true, "😂": true, "🎉": true, "👀": true, "🚀": true,
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

	if !reactionEmoji[req.Emoji] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-emoji"})
		return
	}
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
	if err := sendKey(c, "esc"); err != nil {
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
	if err := sendKey(c, req.Key); err != nil {
		audit("approve-failed", err.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("approve", c.Name+" <- "+req.Key, id)
	Emit("approved", c.ID, c.Name, req.Key)
	// The lock-screen "needs you" notification is now stale; replace it with a
	// same-tag ✓ so it can't sit there demanding attention it already got.
	clearAttnPush(c.ID, c.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUpload saves a photo locally under a server-chosen name (0600) and
// hands the agent the path to Read. Client filenames are never trusted.
func handleUpload(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent    string `json:"agent"`
		Text     string `json:"text"`
		Image    string `json:"image"`
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(data, &req)

	img, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil || len(img) < 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-image"})
		return
	}
	// Reserve the client_id (same TOCTOU-closing claim as handleSend, #3): a
	// completed or still-in-flight upload is acked as a duplicate; the claim is
	// released below — committed on delivery, dropped on any failure so the retry
	// re-runs (H1).
	if !claimClientID(req.ClientID) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		releaseClientID(req.ClientID, false) // never delivered: allow the retry
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}
	pathOnDisk, err := saveAttachment(img)
	if err != nil {
		releaseClientID(req.ClientID, false) // nothing durable: allow the retry
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save-failed"})
		return
	}
	msg := fmt.Sprintf("%s [photo saved at %s — use the Read tool to view it]", strings.TrimSpace(req.Text), pathOnDisk)
	// Same hold-and-batch as handleSend, so a photo can never overtake the
	// held texts that were sent before it (ordering lives in one queue).
	holdInbound(c, MailMessage{From: authConfig.UserMention, Via: "phone", Text: msg, TS: nowUTC(), Emitted: true})
	releaseClientID(req.ClientID, true) // durably queued: a retry is now a safe duplicate ack
	audit("upload", fmt.Sprintf("%s <- %s (%d bytes)", c.Name, pathOnDisk, len(img)), id)
	Emit("sent", c.ID, c.Name, strings.TrimSpace(req.Text)+" 📷 photo", req.ClientID)
	dispatchPluginEvent("message.in", c, map[string]any{"text": msg, "via": "phone", "queued": false})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// saveAttachment writes image bytes under ~/.bridge/attachments with a
// server-chosen, timestamped name (0600).
func saveAttachment(img []byte) (string, error) {
	dir := bridgePath("attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "photo-" + time.Now().UTC().Format("20060102-150405.000000000") + imageExt(img)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, img, 0o600); err != nil {
		return "", err
	}
	_ = os.Chmod(p, 0o600)
	return p, nil
}

// imageExt sniffs the file type so the saved photo carries a sensible extension.
func imageExt(img []byte) string {
	switch http.DetectContentType(img) {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

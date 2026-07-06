package main

// local.go — the same-machine API (lockfile-token auth): agent registration
// and switchboard sends, the Notification-hook receiver with its parked-event
// retry (H8), retire, pair-code minting, and lockdown.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// handleLocalRetire removes an OFFLINE contact from the roster (`bridge
// retire <name>`). Live contacts are refused by Registry.Retire — a running
// agent must lose its window before it can lose its registration — so a
// shared name can only ever match the ghost, never the living.
func handleLocalRetire(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Contact string `json:"contact"`
	}
	_ = json.Unmarshal(data, &req)
	c := registry.Retire(strings.TrimSpace(req.Contact))
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not-found-or-live"})
		return
	}
	audit("retire", c.Name+" ("+c.ID[:8]+")", "local")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": c.Name})
}

// handleLocal routes same-machine CLI/hook requests, authenticated by the
// lockfile token rather than the tailnet identity + device token.
func handleLocal(w http.ResponseWriter, r *http.Request) {
	if localToken == "" || requestToken(r) != localToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "local-auth"})
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/local/connect":
		handleLocalConnect(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/local/event":
		handleLocalEvent(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/local/send":
		handleLocalSend(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/local/contacts":
		cs := registry.Roster()
		if cs == nil {
			cs = []*Contact{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"contacts": cs})
	case r.Method == http.MethodPost && r.URL.Path == "/local/pair":
		writeJSON(w, http.StatusOK, map[string]string{"code": issuePairingCode()})
	case r.Method == http.MethodPost && r.URL.Path == "/local/retire":
		handleLocalRetire(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/local/lockdown":
		revokeAllDevices()
		audit("lockdown", "revoke-all + shutdown", "local")
		pluginOff.Store(true) // no plugin runs again this process (docs/plugins.md)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		requestShutdown()
	case r.Method == http.MethodPost && r.URL.Path == "/local/push-test":
		handlePushTest(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not-found"})
	}
}

// handleLocalConnect registers or revives a contact and flushes any queued
// messages it accumulated while offline.
func handleLocalConnect(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Name       string `json:"name"`
		Directory  string `json:"directory"`
		SessionID  string `json:"session_id"`
		TmuxTarget string `json:"tmux_target"`
	}
	if json.Unmarshal(data, &req) != nil || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-request"})
		return
	}
	c := registry.Connect(req.Name, req.Directory, req.SessionID, req.TmuxTarget)
	audit("connect", c.Name+" "+c.Directory, "local")
	Emit("connected", c.ID, c.Name, "")
	flushMailbox(c)
	writeJSON(w, http.StatusOK, map[string]string{"id": c.ID, "name": c.Name})
}

// handleLocalEvent processes a Notification-hook POST {session_id, message,
// kind}: an idle notification clears the prompt state; anything else captures
// one pane snapshot and marks the contact as prompting.
func handleLocalEvent(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		Kind      string `json:"kind"`
	}
	_ = json.Unmarshal(data, &req)

	c := registry.BySession(req.SessionID)
	if c == nil {
		// No contact claims this session id. For a bridge-managed agent this
		// is the signature of a session roll (/clear, auto-compaction) the
		// daemon hasn't adopted yet — and the hook fires exactly ONCE per
		// event, so dropping it here would lose the prompt permanently (H8).
		// Park it: the reconcile loop nudges the chain scan and retries until
		// the roll is adopted or the event expires. (Most misses are OTHER
		// Claude sessions on this machine — the hook install is global — and
		// those simply age out.)
		parkHookEvent(req.SessionID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "parked": true})
		return
	}
	applyHookEvent(c)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyHookEvent reconciles a contact's attention state with its pane after a
// Notification-hook event. The hook fires for both permission prompts and
// routine idle/waiting notifications; don't trust the notification kind alone —
// gate the attention card on the terminal actually showing a permission dialog
// right now. An idle notification, or a screen with no prompt, clears any open
// prompt instead of raising a false card.
//
// The dialog may still be painting when the hook fires, so the capture retries
// briefly until it looks like a prompt rather than grabbing a stale frame.
func applyHookEvent(c *Contact) {
	snapshot := capturePrompt(c)
	for i := 0; i < 5 && !looksLikePrompt(snapshot); i++ {
		time.Sleep(150 * time.Millisecond)
		snapshot = capturePrompt(c)
	}
	if looksLikePrompt(snapshot) {
		registry.SetPrompt(c.ID, true)
		Emit("attention", c.ID, c.Name, snapshot)
		notifyPush(c.Name+" needs you", firstPromptLine(snapshot), "attn-"+c.ID, c.ID)
		markAttnPushed(c.ID)
		dispatchPluginEvent("permission.prompt", c, map[string]any{"prompt": firstPromptLine(snapshot)})
	} else {
		if c.PromptOpen {
			registry.SetPrompt(c.ID, false)
			Emit("attention-clear", c.ID, c.Name, "")
			clearAttnPush(c.ID, c.Name)
		}
	}
}

// pendingHooks parks hook events whose session id resolved to no contact (H8).
// Guarded by pendingHookMu; drained by the reconcile goroutine.
type hookEvent struct {
	sessionID string
	at        time.Time
	nudged    bool
}

var (
	pendingHookMu sync.Mutex
	pendingHooks  []hookEvent
)

const (
	maxPendingHooks   = 32
	pendingHookMaxAge = 60 * time.Second
)

// parkHookEvent queues an unresolvable hook event for retry, one entry per
// session id (re-applying reads the CURRENT pane, so the latest state wins).
func parkHookEvent(sessionID string) {
	if sessionID == "" {
		return
	}
	pendingHookMu.Lock()
	defer pendingHookMu.Unlock()
	for _, ev := range pendingHooks {
		if ev.sessionID == sessionID {
			return
		}
	}
	if len(pendingHooks) >= maxPendingHooks {
		pendingHooks = pendingHooks[1:]
	}
	pendingHooks = append(pendingHooks, hookEvent{sessionID: sessionID, at: time.Now()})
}

// drainPendingHooks retries parked hook events. Reconcile goroutine only (it
// writes chainForce). For each parked id: once a contact claims it, apply the
// event against the live pane; until then, nudge every live contact whose
// project dir holds that session file, so sessionFileFor re-scans its rollover
// chain on this very pass instead of after the two-minute quiet window — the
// gap that used to swallow the one hook delivery a roll ever gets.
func drainPendingHooks() {
	pendingHookMu.Lock()
	events := append([]hookEvent(nil), pendingHooks...)
	pendingHookMu.Unlock()
	if len(events) == 0 {
		return
	}
	keep := events[:0]
	for _, ev := range events {
		if time.Since(ev.at) > pendingHookMaxAge {
			continue // an uninstrumented session's hook; let it age out
		}
		if c := registry.BySession(ev.sessionID); c != nil {
			applyHookEvent(c)
			continue
		}
		if !ev.nudged {
			file := ev.sessionID + ".jsonl"
			for _, c := range registry.Roster() {
				if c.Status != "live" {
					continue
				}
				if _, err := os.Stat(filepath.Join(projectDir(c.Directory), file)); err == nil {
					chainForce[c.ID] = true
				}
			}
			ev.nudged = true
		}
		keep = append(keep, ev)
	}
	pendingHookMu.Lock()
	// New events may have parked while we worked; keep those arrivals too.
	pendingHooks = append(append([]hookEvent(nil), keep...), pendingHooks[len(events):]...)
	pendingHookMu.Unlock()
}

// handleLocalSend handles an agent-composed outbound message. Without "to" the
// text is surfaced to the phone in the sender's thread; with "to" it is routed
// agent-to-agent through the same mailboxes (the switchboard).
func handleLocalSend(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Contact string `json:"contact"` // the calling agent (from BRIDGE_CONTACT)
		Text    string `json:"text"`
		To      string `json:"to"` // optional switchboard target
	}
	_ = json.Unmarshal(data, &req)

	if strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty"})
		return
	}

	// The sender must resolve to a registered contact: senderName becomes the
	// "From" in the recipient's provenance frame and the byline on the phone,
	// so relaying an arbitrary string here is an identity-forgery vector (H9).
	s := registry.Resolve(req.Contact)
	if s == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown sender"})
		return
	}
	senderName, senderID := s.Name, s.ID

	if req.To == "" {
		audit("agent-send", senderName+": "+req.Text, "local")
		Emit("reply", senderID, senderName, req.Text)
		notifyPush(senderName, req.Text, "msg-"+senderID, senderID)
		dispatchPluginEvent("reply.out", registry.Resolve(senderID), map[string]any{"text": req.Text})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	target := registry.Resolve(req.To)
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	if target.Status != "live" {
		// Durably queued IS success — the daemon delivers it on revival. The
		// old 409 here read as failure, so well-behaved senders retried and
		// the recipient woke up to duplicates (review round 2).
		registry.Queue(target.ID, MailMessage{From: senderName, Via: "bridge", Text: req.Text, TS: nowUTC()})
		audit("switchboard-queued", senderName+" -> "+target.Name+": "+req.Text, "local")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": true})
		return
	}
	// Switchboard rides the same hold-and-batch as phone sends: it keeps one
	// ordered queue per recipient and, critically, never types past an open
	// permission dialog (the hold refuses to fire while one is up).
	//
	// The event is "peer", Name = the SENDER: an agent-to-agent message in the
	// recipient's thread must wear its author, not render as one of the user's
	// own bubbles (found live 2026-07-06 — Marvin's status check showed as
	// "you → vint" on the phone).
	audit("switchboard", senderName+" -> "+target.Name+": "+req.Text, "local")
	Emit("peer", target.ID, senderName, req.Text)
	holdInbound(target, MailMessage{From: senderName, Via: "bridge", Text: req.Text, TS: nowUTC(), Emitted: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

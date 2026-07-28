package main

// semantic.go — protocol v2 for clients that speak an agent API instead of a
// terminal.  It is deliberately layered on the existing remote lease/outbox:
// v1 clients continue to drain text/key deliveries, while v2 clients drain
// typed commands and publish typed events.  The durable mailbox and ack rule
// therefore remain the source of truth for both protocols.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type SemanticCommandType string

const (
	SemanticCommandInput     SemanticCommandType = "input"
	SemanticCommandInterrupt SemanticCommandType = "interrupt"
	SemanticCommandApproval  SemanticCommandType = "approval"
	SemanticCommandCompact   SemanticCommandType = "compact"
)

// SemanticCommand is provider-neutral. A Codex client maps input to turn/start
// or turn/steer, interrupt to turn/interrupt, and approval to the response for
// the app-server request identified by RequestID.
type SemanticCommand struct {
	ID         string              `json:"id"`
	Contact    string              `json:"contact"`
	Type       SemanticCommandType `json:"type"`
	Text       string              `json:"text,omitempty"`
	DeliveryID string              `json:"delivery_id,omitempty"`
	RequestID  string              `json:"request_id,omitempty"`
	Decision   string              `json:"decision,omitempty"`
	// ApprovalKind selects the app-server response schema. Permissions carries
	// the exact profile Codex requested so an approval can grant no more than
	// that profile; both are empty for non-approval commands.
	ApprovalKind string          `json:"approval_kind,omitempty"`
	Permissions  json.RawMessage `json:"permissions,omitempty"`
}

type SemanticEventType string

const (
	SemanticEventAgentMessage      SemanticEventType = "agent_message"
	SemanticEventStatus            SemanticEventType = "status"
	SemanticEventPlan              SemanticEventType = "plan"
	SemanticEventApprovalRequested SemanticEventType = "approval_requested"
	SemanticEventApprovalResolved  SemanticEventType = "approval_resolved"
)

type SemanticEvent struct {
	ID                 string            `json:"id,omitempty"`
	Contact            string            `json:"contact"`
	Type               SemanticEventType `json:"type"`
	Text               string            `json:"text,omitempty"`
	Status             string            `json:"status,omitempty"`
	RequestID          string            `json:"request_id,omitempty"`
	ApprovalKind       string            `json:"approval_kind,omitempty"`
	Reason             string            `json:"reason,omitempty"`
	Command            string            `json:"command,omitempty"`
	Cwd                string            `json:"cwd,omitempty"`
	AvailableDecisions []string          `json:"available_decisions,omitempty"`
	Permissions        json.RawMessage   `json:"permissions,omitempty"`
}

type semanticApproval struct {
	RequestID   string
	Kind        string
	Permissions json.RawMessage
}

var (
	semanticApprovalMu sync.Mutex
	semanticApprovals  = map[string]semanticApproval{} // contact id -> current request
)

func semanticApprovalPending(contactID string) bool {
	semanticApprovalMu.Lock()
	defer semanticApprovalMu.Unlock()
	_, ok := semanticApprovals[contactID]
	return ok
}

func clearSemanticApproval(contactID string) {
	semanticApprovalMu.Lock()
	delete(semanticApprovals, contactID)
	semanticApprovalMu.Unlock()
}

func resolveSemanticApproval(contactID, requestID string) bool {
	semanticApprovalMu.Lock()
	defer semanticApprovalMu.Unlock()
	pending, exists := semanticApprovals[contactID]
	if !exists || requestID == "" || pending.RequestID != requestID {
		return false
	}
	delete(semanticApprovals, contactID)
	return true
}

// handleTransportCommands is the v2 equivalent of /mail. It returns only
// semantic commands, retaining the v1 all-unacked/idempotent drain contract.
func handleTransportCommands(w http.ResponseWriter, r *http.Request) {
	lease := r.URL.Query().Get("lease")
	deadline := time.Now().Add(clampMailWait(r.URL.Query().Get("wait")))
	for {
		remoteMu.Lock()
		l := remoteLeases[lease]
		if l == nil || l.stale() || l.protocol < 2 {
			remoteMu.Unlock()
			writeJSON(w, http.StatusGone, map[string]string{"error": "lease-expired-or-not-v2"})
			return
		}
		commands := make([]SemanticCommand, 0, len(l.outbox))
		for _, pd := range l.outbox {
			if pd.Command != nil {
				commands = append(commands, *pd.Command)
			}
		}
		remoteMu.Unlock()
		if len(commands) > 0 || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"commands": commands})
			return
		}
		time.Sleep(remotePollInterval)
	}
}

// handleTransportEvents accepts normalized events from a semantic client.
// Raw model reasoning and tool output have no event type here by design; only
// user-visible messages, plan/status summaries, and approvals cross the bridge.
func handleTransportEvents(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Lease  string          `json:"lease"`
		Events []SemanticEvent `json:"events"`
	}
	if json.Unmarshal(data, &req) != nil || len(req.Events) == 0 || len(req.Events) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-request"})
		return
	}

	remoteMu.Lock()
	l := remoteLeases[req.Lease]
	if l == nil || l.stale() || l.protocol < 2 {
		remoteMu.Unlock()
		writeJSON(w, http.StatusGone, map[string]string{"error": "lease-expired-or-not-v2"})
		return
	}
	hosted := make(map[string]bool, len(l.agents))
	for id := range l.agents {
		hosted[id] = true
	}
	remoteMu.Unlock()

	accepted := 0
	for _, ev := range req.Events {
		if !hosted[ev.Contact] {
			continue
		}
		c := registry.Resolve(ev.Contact)
		if c == nil || c.Status != "live" {
			continue
		}
		if applySemanticEvent(c, ev) {
			accepted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": accepted})
}

func applySemanticEvent(c *Contact, ev SemanticEvent) bool {
	switch ev.Type {
	case SemanticEventAgentMessage:
		if text := strings.TrimSpace(stripControl(ev.Text)); text != "" {
			registry.SetHealth(c.ID, "ok")
			_, fresh := emitEventOnce(Event{
				Type: "reply", Agent: c.ID, Name: c.Name, Text: text,
				SourceID: semanticEventSourceID(c.ID, ev.ID),
			})
			if fresh {
				dispatchPluginEvent("reply.out", c, map[string]any{"text": text})
			}
			return true
		}
	case SemanticEventStatus:
		switch ev.Status {
		case "working", "inProgress":
			registry.SetHealth(c.ID, "working")
			EmitTyping(c.ID, c.Name)
		case "idle", "completed":
			registry.SetHealth(c.ID, "ok")
		case "interrupted", "failed", "declined":
			registry.SetHealth(c.ID, "ok")
			Emit("agent-status", c.ID, c.Name, ev.Status)
		default:
			return false
		}
		return true
	case SemanticEventPlan:
		if text := strings.TrimSpace(stripControl(ev.Text)); text != "" {
			emitEventOnce(Event{
				Type: "plan", Agent: c.ID, Name: c.Name, Text: text,
				SourceID: semanticEventSourceID(c.ID, ev.ID),
			})
			return true
		}
	case SemanticEventApprovalRequested:
		if ev.RequestID == "" {
			return false
		}
		semanticApprovalMu.Lock()
		previous, existed := semanticApprovals[c.ID]
		semanticApprovals[c.ID] = semanticApproval{
			RequestID: ev.RequestID, Kind: ev.ApprovalKind, Permissions: ev.Permissions,
		}
		semanticApprovalMu.Unlock()
		caption := strings.TrimSpace(ev.Reason)
		if caption == "" {
			caption = strings.TrimSpace(ev.Command)
		}
		if caption == "" {
			caption = "Codex wants your approval"
		}
		change := registry.MarkPrompt(c.ID, caption)
		// Event delivery is at-least-once. A retry for the same request must not
		// ring the phone again, while a genuinely newer request must refresh the
		// card even when it happens to have the same human caption.
		if existed && previous.RequestID == ev.RequestID && change == promptNoChange {
			return true
		}
		body := formatSemanticApproval(ev, caption)
		Emit("attention", c.ID, c.Name, body)
		notifyPush(c.Name+" needs you", caption, "attn-"+c.ID, c.ID)
		markAttnPushed(c.ID)
		return true
	case SemanticEventApprovalResolved:
		if resolveSemanticApproval(c.ID, ev.RequestID) {
			registry.SetPrompt(c.ID, false)
			Emit("attention-clear", c.ID, c.Name, "")
			clearAttnPush(c.ID, c.Name)
			return true
		}
	}
	return false
}

func semanticEventSourceID(contactID, eventID string) string {
	if eventID == "" {
		return "" // backwards-compatible v2 clients remain accepted, without dedup
	}
	return contactID + ":" + eventID
}

func formatSemanticApproval(ev SemanticEvent, caption string) string {
	var lines []string
	lines = append(lines, caption)
	if ev.Command != "" && ev.Command != caption {
		lines = append(lines, "Command: "+ev.Command)
	}
	if ev.Cwd != "" {
		lines = append(lines, "Directory: "+ev.Cwd)
	}
	// Preserve the existing PWA's numbered-button wire contract. These map to
	// named semantic decisions only for a v2 contact; v1 still sends keypresses.
	lines = append(lines,
		"1. Yes",
		"2. Yes, and allow for this session",
		"3. No")
	return strings.Join(lines, "\n")
}

func semanticDecisionForLegacyKey(key string) (string, bool) {
	switch key {
	case "1", "y":
		return "accept", true
	case "2":
		return "acceptForSession", true
	case "3", "n":
		return "decline", true
	case "esc":
		return "cancel", true
	default:
		return "", false
	}
}

func deliverLegacySemanticApproval(c *Contact, key string) error {
	decision, ok := semanticDecisionForLegacyKey(key)
	if !ok {
		return fmt.Errorf("approval key %q has no semantic decision", key)
	}
	semanticApprovalMu.Lock()
	pending, exists := semanticApprovals[c.ID]
	semanticApprovalMu.Unlock()
	if !exists {
		return fmt.Errorf("no semantic approval is pending for %s", c.Name)
	}
	return remoteParkCommandAndWait(c, SemanticCommand{
		Type: SemanticCommandApproval, RequestID: pending.RequestID, Decision: decision,
		ApprovalKind: pending.Kind, Permissions: pending.Permissions,
	})
}

var semanticDecisions = map[string]bool{
	"accept": true, "acceptForSession": true, "decline": true, "cancel": true,
}

// handleSemanticApprove is additive to /api/approve. The old endpoint keeps
// its key vocabulary; this endpoint uses Codex/app-server decision names and
// validates the request id against the currently surfaced structured prompt.
func handleSemanticApprove(w http.ResponseWriter, r *http.Request, actor string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent     string `json:"agent"`
		RequestID string `json:"request_id"`
		Decision  string `json:"decision"`
	}
	if json.Unmarshal(data, &req) != nil || !semanticDecisions[req.Decision] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decision-not-allowed"})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" || !c.PromptOpen {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not-waiting"})
		return
	}
	semanticApprovalMu.Lock()
	pending, exists := semanticApprovals[c.ID]
	semanticApprovalMu.Unlock()
	if !exists || pending.RequestID != req.RequestID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "stale-request"})
		return
	}
	if !remoteUsesSemanticProtocol(c.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not-semantic"})
		return
	}
	command := SemanticCommand{
		Type: SemanticCommandApproval, RequestID: req.RequestID, Decision: req.Decision,
		ApprovalKind: pending.Kind, Permissions: pending.Permissions,
	}
	if err := remoteParkCommandAndWait(c, command); err != nil {
		audit("semantic-approve-failed", err.Error(), actor)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("semantic-approve", fmt.Sprintf("%s <- %s", c.Name, req.Decision), actor)
	Emit("approved", c.ID, c.Name, req.Decision)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deliverCompact preserves the terminal behavior for v1 clients while giving
// semantic clients a typed command. Treating the literal "/compact" as normal
// input would create a user turn instead of invoking app-server's dedicated
// thread/compact/start operation.
func deliverCompact(c *Contact) error {
	if remoteUsesSemanticProtocol(c.ID) {
		return remoteParkCommandAndWait(c, SemanticCommand{Type: SemanticCommandCompact})
	}
	return transportFor(c).Deliver(c, compactCommand)
}

// deliverMailboxGroup carries the registry's durable group identity only on
// the semantic lane. Terminal/v1 transports still receive exactly the prepared
// text line, while a v2 redelivery under a fresh lease retains DeliveryID.
func deliverMailboxGroup(c *Contact, text, deliveryID string) error {
	if remoteUsesSemanticProtocol(c.ID) {
		return remoteParkCommandAndWait(c, SemanticCommand{
			Type: SemanticCommandInput, Text: text, DeliveryID: deliveryID,
		})
	}
	return transportFor(c).Deliver(c, text)
}

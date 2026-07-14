package main

// remote.go — the remote transport: agents that live in an external client (an
// Emacs/vterm crew, or a curl loop) instead of a daemon-managed tmux window.
// The daemon never reaches into the client; the client reaches into the daemon
// (docs/transports.md). A connected client HELLOs its agents, then continuously
// ATTESTs their liveness/readiness/screen; the daemon answers the five
// Transport questions (transport.go) purely from that attested state. Delivery
// is a composed line PARKED in a per-lease outbox that the client DRAINs
// (long-poll /mail) and ACKs — flushMailbox treats a parked line as in-flight
// exactly like tmux's send-keys, so every safety rule above the transport line
// (the guarded flush, provenance framing, coalescing, the cooldown) holds
// unchanged. A disconnected client is byte-for-byte an offline contact: its
// lease goes stale, its agents read dead, mail waits durably, a re-hello revives.
//
// Concurrency rule (load-bearing): NOTHING sleeps, long-polls, or does I/O
// while holding remoteMu. The two blocking primitives — Deliver's ack wait and
// /mail's long poll — take the lock only for short reads/writes and sleep
// between them. Localhost is cheap; a 150ms poll beats cond-var choreography.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func init() { registerTransport("remote", remoteTransport{}) }

// remoteState is one agent's last attested view of itself. The daemon trusts it
// only while the lease is fresh; a stale lease reads as dead and every answer
// fails safe (never ready, empty capture) — the same shape as an unreadable
// tmux pane today.
type remoteState struct {
	Ready      bool
	PromptOpen bool
	ScreenTail string
}

// parkedDelivery is one composed line (or one approval key) waiting for the
// client to drain and ack. The frame text is daemon-authored — provenance is
// never client-authored — so the client only types and acks. acked is flipped
// by the ack handler under remoteMu and observed by the parking Deliver/SendKey
// through its retained pointer, so a removed entry a caller still holds can
// still report its ack: a race between ack and the ack-timeout resolves
// consistently (whoever wins under the lock decides, the loser is a no-op).
type parkedDelivery struct {
	ID      string
	Contact string
	Text    string
	Key     string
	// Command is set only for protocol-v2 semantic clients.  V1 clients keep
	// seeing the exact text/key delivery shape they have always understood.
	Command *SemanticCommand
	acked   bool
}

// remoteLease is one client's liveness grant, keyed by an opaque token. A lease
// is FRESH iff the client has hello'd or attested within the TTL; a stale lease's
// agents read dead until the client re-hellos. mail/ack deliberately do NOT
// refresh it — only the heartbeat (hello/attest) proves the client is alive, so
// a client that only long-polls without attesting still ages out.
type remoteLease struct {
	token    string
	flavor   string                 // client-supplied environment label ("emacs", "sim")
	protocol int                    // 1 = legacy text/key drain; 2 = semantic command/event lane
	provider string                 // optional semantic runtime label ("codex", etc.)
	lastSeen time.Time              // last hello/attest; the freshness clock
	suspect  bool                   // a Deliver timed out against this lease; the next attest clears it
	agents   map[string]bool        // contact ids this lease hosts
	states   map[string]remoteState // contact id -> last attested state
	outbox   []*parkedDelivery      // parked deliveries awaiting drain+ack (cap remoteOutboxCap)
}

var (
	remoteMu             sync.Mutex
	remoteLeases         = map[string]*remoteLease{}
	remoteLeaseByContact = map[string]string{} // contact id -> current lease token (hello replaces)
)

const (
	// remoteMaxHelloAgents caps agents per hello so one call can't register an
	// unbounded roster in a single request.
	remoteMaxHelloAgents = 8
	// remoteOutboxCap bounds a lease's un-acked backlog so a client that stops
	// draining can't grow it without bound. Parking past it errors, and the
	// durable mailbox retries on the next tick — no loss, just backpressure.
	remoteOutboxCap = 32
	// remotePollInterval is the lock-free sleep between outbox/lease checks in
	// Deliver's ack wait and /mail's long poll. Short because localhost is cheap.
	remotePollInterval = 150 * time.Millisecond
)

// remoteTTL is the freshness window for a lease: config remote_ttl_s (default
// 30), floored at 2s so a pathological config can't make every lease instantly
// stale and strand every remote agent offline.
func remoteTTL() time.Duration {
	ttl := 30
	if authConfig.RemoteTTLs != nil {
		ttl = *authConfig.RemoteTTLs
	}
	if ttl < 2 {
		ttl = 2
	}
	return time.Duration(ttl) * time.Second
}

// remoteTTLSeconds is the clamped TTL in whole seconds, echoed to the client on
// hello/attest so it knows how often to re-attest.
func remoteTTLSeconds() int { return int(remoteTTL() / time.Second) }

// remoteAckTimeout is how long Deliver/SendKey block waiting for the client to
// ack a parked line before giving up (and letting the mailbox redeliver): config
// remote_ack_timeout_s (default 10), floored at 1s. Kept well under the 90s
// reconcile watchdog so a blocking flush never trips the sleep detector.
func remoteAckTimeout() time.Duration {
	t := 10
	if authConfig.RemoteAckTimeoutS != nil {
		t = *authConfig.RemoteAckTimeoutS
	}
	if t < 1 {
		t = 1
	}
	return time.Duration(t) * time.Second
}

// stale reports whether the lease has not been attested within the TTL. Caller
// holds remoteMu.
func (l *remoteLease) stale() bool { return time.Since(l.lastSeen) > remoteTTL() }

// remoteFreshLeaseLocked returns the contact's current lease iff it exists, is
// fresh, and still hosts the contact — nil otherwise. Every Transport answer
// funnels through it, so "stale ⇒ dead" is enforced in exactly one place and no
// branch can forget to fail safe. Caller holds remoteMu.
func remoteFreshLeaseLocked(contactID string) *remoteLease {
	token := remoteLeaseByContact[contactID]
	if token == "" {
		return nil
	}
	l := remoteLeases[token]
	if l == nil || l.stale() || !l.agents[contactID] {
		return nil
	}
	return l
}

// remoteReapLocked deletes leases stale for more than 10x the TTL — a lazy GC
// run at the top of hello/attest so a crashed client's lease (and its contact
// mappings) doesn't linger forever. The window is wide on purpose: a merely
// stale lease is kept so attest can 410 (and delete) it explicitly, telling the
// client to re-hello. Caller holds remoteMu.
func remoteReapLocked() {
	cutoff := 10 * remoteTTL()
	for token, l := range remoteLeases {
		if time.Since(l.lastSeen) > cutoff {
			remoteDeleteLeaseLocked(token)
		}
	}
}

// remoteDeleteLeaseLocked removes a lease and any contact mappings still
// pointing at it (a mapping already re-pointed by a newer hello is left alone).
// Caller holds remoteMu.
func remoteDeleteLeaseLocked(token string) {
	delete(remoteLeases, token)
	for cid, t := range remoteLeaseByContact {
		if t == token {
			delete(remoteLeaseByContact, cid)
		}
	}
}

// remoteRemoveParkedLocked drops the parked delivery with id from a lease's
// outbox (no-op if already gone). Caller holds remoteMu.
func remoteRemoveParkedLocked(l *remoteLease, id string) {
	for i, pd := range l.outbox {
		if pd.ID == id {
			l.outbox = append(l.outbox[:i], l.outbox[i+1:]...)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// The Transport implementation (transport.go): five answers from attested state
// ---------------------------------------------------------------------------

type remoteTransport struct{}

// Alive: the contact has a lease and it is fresh.
func (remoteTransport) Alive(c *Contact) bool {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	return remoteFreshLeaseLocked(c.ID) != nil
}

// Ready: fresh AND not suspect AND the last attestation said ready. Terminal
// clients also get the screen-dialog belt; semantic clients additionally get
// the structured prompt_open belt. No attest yet, a stale lease, or a Deliver
// that timed out all read false, so guarded delivery always fails safe.
func (remoteTransport) Ready(c *Contact) bool {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	l := remoteFreshLeaseLocked(c.ID)
	if l == nil || l.suspect {
		return false
	}
	st, ok := l.states[c.ID]
	// The delivery dialog belt. A naive client attesting ready:true with a dialog
	// still on screen must NOT get text typed into it — the composed line's
	// trailing Enter would blind-select whatever option is highlighted (the C2/C4
	// critical, remote edition). So the daemon overrules the client's boolean on
	// the evidence of a dialog it can see in the attested tail, exactly as the tmux
	// guard refuses on paneShowsDialog(capture-pane). paneShowsDialog is a pure
	// in-memory string parse — fine under remoteMu — and paneShowsDialog("") is
	// false, so a client that attests no screen_tail keeps its boolean's word: the
	// belt only ever SUBTRACTS, and only on a dialog it actually has bytes for.
	return ok && st.Ready && !(l.protocol >= 2 && st.PromptOpen) && !paneShowsDialog(st.ScreenTail)
}

// Capture: the last attested screen tail while fresh, "" when stale — callers
// treat "" as "assume the worst, hold delivery", exactly like an unreadable pane.
func (remoteTransport) Capture(c *Contact) string {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if l := remoteFreshLeaseLocked(c.ID); l != nil {
		return l.states[c.ID].ScreenTail
	}
	return ""
}

// Deliver parks one composed line and blocks until the client acks it (nil), the
// ack timeout elapses, or the lease dies (error). An error leaves the mail queued
// for the next flush — identical to a failed tmux send-keys.
func (remoteTransport) Deliver(c *Contact, text string) error {
	if remoteUsesSemanticProtocol(c.ID) {
		return remoteParkCommandAndWait(c, SemanticCommand{Type: SemanticCommandInput, Text: text})
	}
	return remoteParkAndWait(c, text, "")
}

// SendKey parks one approval keystroke with the same ack discipline. The endpoint
// already validates the key, but defend the transport boundary too: only the
// approve whitelist (delivery.go) ever reaches a client's send-key.
func (remoteTransport) SendKey(c *Contact, key string) error {
	if !approveKeys[key] {
		return fmt.Errorf("remote: key %q not in the approve whitelist", key)
	}
	// Escape is the legacy Transport spelling of interrupt.  A v2 client gets
	// the provider-neutral semantic command instead of a fake terminal key.
	if key == "esc" && remoteUsesSemanticProtocol(c.ID) {
		return remoteParkCommandAndWait(c, SemanticCommand{Type: SemanticCommandInterrupt})
	}
	return remoteParkAndWait(c, "", key)
}

// remoteUsesSemanticProtocol reports whether the contact's current fresh
// lease negotiated v2. It deliberately returns false for missing/stale leases,
// preserving the v1 fail-safe path and allowing remoteParkAndWait to produce
// the existing disconnected error.
func remoteUsesSemanticProtocol(contactID string) bool {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	l := remoteFreshLeaseLocked(contactID)
	return l != nil && l.protocol >= 2
}

func remoteParkCommandAndWait(c *Contact, command SemanticCommand) error {
	return remotePark(c, "", "", &command)
}

// remoteParkAndWait is Deliver/SendKey's shared body: park a delivery in the
// contact's current lease outbox, then poll for its ack. It NEVER holds remoteMu
// across a sleep. On timeout it marks the lease suspect (so the next flush defers
// until an attest proves the client back) and audits, then returns an error so
// the mailbox — the source of truth — redelivers under a fresh id on a later tick.
func remoteParkAndWait(c *Contact, text, key string) error {
	return remotePark(c, text, key, nil)
}

func remotePark(c *Contact, text, key string, command *SemanticCommand) error {
	remoteMu.Lock()
	l := remoteFreshLeaseLocked(c.ID)
	if l == nil {
		remoteMu.Unlock()
		// No fresh lease is the remote analogue of a dead window: fail safe, the
		// mailbox holds the line and the next reconcile tick retries. Not an error
		// worth marking offline over — the reconcile loop owns liveness.
		return fmt.Errorf("remote agent %s is not connected", c.Name)
	}
	if len(l.outbox) >= remoteOutboxCap {
		remoteMu.Unlock()
		return fmt.Errorf("remote outbox for %s is full", c.Name)
	}
	token := l.token
	pd := &parkedDelivery{ID: newID(), Contact: c.ID, Text: text, Key: key, Command: command}
	if pd.Command != nil {
		pd.Command.ID = pd.ID
		pd.Command.Contact = c.ID
	}
	l.outbox = append(l.outbox, pd)
	remoteMu.Unlock()

	deadline := time.Now().Add(remoteAckTimeout())
	for {
		time.Sleep(remotePollInterval)
		remoteMu.Lock()
		if pd.acked {
			remoteMu.Unlock()
			return nil
		}
		l := remoteLeases[token]
		// The lease must still exist, still be THIS contact's current lease (a
		// re-hello mints a new token and re-points the mapping), and still be
		// fresh — otherwise the client this line was parked for is gone.
		if l == nil || remoteLeaseByContact[c.ID] != token || l.stale() {
			if l != nil {
				remoteRemoveParkedLocked(l, pd.ID)
			}
			remoteMu.Unlock()
			return fmt.Errorf("remote lease for %s ended before ack", c.Name)
		}
		if time.Now().After(deadline) {
			remoteRemoveParkedLocked(l, pd.ID)
			l.suspect = true // silent client: hold delivery until the next attest proves it back
			remoteMu.Unlock()
			audit("remote-ack-timeout", c.Name, "daemon") // file I/O — must be outside the lock
			return fmt.Errorf("remote ack timeout for %s", c.Name)
		}
		remoteMu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Route-health read accessor (route-health Layer 1, routehealth.go)
// ---------------------------------------------------------------------------

// remoteRoute is a read-only snapshot of a contact's RAW lease state, for the
// route-health surfacing in /api/status (deriveRouteHealth). It exists because
// remoteFreshLeaseLocked — the funnel every Transport answer uses — COLLAPSES a
// stale lease to "no lease" so those answers fail safe; that same collapse is
// exactly what made the July 7 silence invisible (a stale lease read as plain
// offline, no "last seen 21m"). This snapshot reports staleness honestly instead.
type remoteRoute struct {
	hasLease    bool // a lease exists AND still hosts this contact
	stale       bool // the lease has aged past the TTL (client stopped attesting)
	suspect     bool // a Deliver timed out against this lease; the next attest clears it
	ready       bool // the last attest's raw ready boolean (before the dialog belt)
	dialog      bool // the attested tail shows an interactive dialog (paneShowsDialog)
	lastSeenAgo int  // whole seconds since the lease's last hello/attest
	outbox      int  // parked, un-acked deliveries still awaiting drain
}

// remoteRouteInfo returns a read-only snapshot of a contact's raw lease state. It
// deliberately does NOT route through remoteFreshLeaseLocked (which hides
// staleness): it looks the lease up raw — remoteLeaseByContact[id] -> the lease,
// checking it still hosts the contact — so a stale-but-present lease reports
// hasLease:true, stale:true rather than vanishing. Takes remoteMu for a short
// read exactly like Alive/Ready/Capture. paneShowsDialog is the same pure
// in-memory parse Ready() already runs under this lock. MUTATES NOTHING.
func remoteRouteInfo(contactID string) remoteRoute {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	token := remoteLeaseByContact[contactID]
	if token == "" {
		return remoteRoute{}
	}
	l := remoteLeases[token]
	if l == nil || !l.agents[contactID] {
		return remoteRoute{}
	}
	st := l.states[contactID] // missing key -> zero remoteState (Ready false, tail "")
	return remoteRoute{
		hasLease:    true,
		stale:       l.stale(),
		suspect:     l.suspect,
		ready:       st.Ready,
		dialog:      paneShowsDialog(st.ScreenTail),
		lastSeenAgo: int(time.Since(l.lastSeen) / time.Second),
		outbox:      len(l.outbox),
	}
}

// ---------------------------------------------------------------------------
// The /local/transport/* endpoints (lockfile-token auth enforced in handleLocal)
// ---------------------------------------------------------------------------

// handleTransportHello registers (or revives) a client's agents and issues a
// lease. Body: {"transport":<flavor>,"agents":[{name,directory,session_id}]}.
// Modeled on handleLocalConnect: each agent registers through the same identity
// ladder (ConnectRemote), emits "connected", and gets a flush attempt (a harmless
// no-op pre-attest, since Ready is false without a lease state — called for
// symmetry). The lease is minted AFTER registration and carries the fresh ids.
func handleTransportHello(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Transport string `json:"transport"`
		Protocol  int    `json:"protocol"`
		Provider  string `json:"provider"`
		Agents    []struct {
			Name      string `json:"name"`
			Directory string `json:"directory"`
			SessionID string `json:"session_id"`
		} `json:"agents"`
	}
	if json.Unmarshal(data, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-request"})
		return
	}
	if len(req.Agents) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no-agents"})
		return
	}
	if len(req.Agents) > remoteMaxHelloAgents {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too-many-agents"})
		return
	}
	flavor := sanitizeFlavor(req.Transport)
	protocol := negotiatedTransportProtocol(r.URL.Path, req.Protocol)
	provider := sanitizeFlavor(req.Provider)
	if req.Provider == "" {
		provider = ""
	}

	type outAgent struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	agents := make([]outAgent, 0, len(req.Agents))
	ids := make(map[string]bool, len(req.Agents))
	names := make([]string, 0, len(req.Agents))
	for _, a := range req.Agents {
		c := registry.ConnectRemote(a.Name, a.Directory, a.SessionID, flavor)
		agents = append(agents, outAgent{ID: c.ID, Name: c.Name})
		ids[c.ID] = true
		names = append(names, c.Name)
		Emit("connected", c.ID, c.Name, "")
		prependWakeDigest(c) // lead the backlog with a since-you-woke line if this was a real wake
		flushMailbox(c)      // defers pre-attest (Ready false); the digest waits durably with the backlog
	}
	audit("remote-hello", flavor+" "+strings.Join(names, " "), "local")

	token := randomHex(32)
	remoteMu.Lock()
	remoteReapLocked()
	remoteLeases[token] = &remoteLease{
		token:    token,
		flavor:   flavor,
		protocol: protocol,
		provider: provider,
		lastSeen: time.Now(),
		agents:   ids,
		states:   map[string]remoteState{},
	}
	for id := range ids {
		remoteLeaseByContact[id] = token // hello replaces any prior mapping for these agents
	}
	remoteMu.Unlock()

	resp := map[string]any{
		"agents": agents,
		"lease":  token,
		"ttl_s":  remoteTTLSeconds(),
	}
	if protocol >= 2 {
		resp["protocol"] = 2
		resp["capabilities"] = []string{"input", "steer", "interrupt", "approval", "compact", "events"}
	}
	writeJSON(w, http.StatusOK, resp)
}

// The versioned URL is itself a negotiation signal. Keeping explicit protocol
// support on the legacy hello route is backwards-compatible, while making the
// documented /v2/hello alias impossible to accidentally downgrade to v1.
func negotiatedTransportProtocol(path string, requested int) int {
	if path == "/local/transport/v2/hello" || requested == 2 {
		return 2
	}
	return 1
}

// handleTransportAttest is the heartbeat that answers Alive/Ready/Capture: it
// refreshes the lease's freshness clock, clears any suspect mark, and records
// each agent's attested state. Body: {"lease",states:[{id,ready,prompt_open,
// screen_tail}]}. An unknown or stale lease is 410 (and a stale one is deleted) —
// the client must re-hello.
func handleTransportAttest(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Lease  string `json:"lease"`
		States []struct {
			ID         string `json:"id"`
			Ready      bool   `json:"ready"`
			PromptOpen bool   `json:"prompt_open"`
			ScreenTail string `json:"screen_tail"`
		} `json:"states"`
	}
	_ = json.Unmarshal(data, &req)

	remoteMu.Lock()
	remoteReapLocked()
	l := remoteLeases[req.Lease]
	if l == nil || l.stale() {
		if l != nil {
			remoteDeleteLeaseLocked(req.Lease) // a stale lease is spent; force a re-hello
		}
		remoteMu.Unlock()
		writeJSON(w, http.StatusGone, map[string]string{"error": "lease-expired"})
		return
	}
	l.lastSeen = time.Now()
	l.suspect = false
	// Attest-time prompt cards — the Phase 3 raise. While storing each agent's
	// state, judge the CLAMPED tail (the exact bytes Capture will later hand
	// verifyPrompt) and collect the agents newly showing a permission dialog. The
	// raise itself must run AFTER the lock: it calls registry.SetPrompt, which
	// takes the registry mutex AND saves to disk, and notifyPush/dispatchPluginEvent
	// do I/O — all three are forbidden under remoteMu by the load-bearing rule at
	// the top of this file. So collect here, act below.
	type promptRaise struct{ id, tail string }
	var raises []promptRaise
	for _, s := range req.States {
		if !l.agents[s.ID] {
			continue // only agents this lease hosts
		}
		tail := clampScreenTail(s.ScreenTail)
		l.states[s.ID] = remoteState{
			Ready:      s.Ready,
			PromptOpen: s.PromptOpen,
			ScreenTail: tail,
		}
		// The raise is judged by looksLikePrompt on the ATTESTED tail, never by the
		// client's advisory prompt_open boolean. verifyPrompt (reconcile.go) already
		// clears remote cards via the SAME two-strike looksLikePrompt(Capture) — and
		// raise and clear MUST share one judge, or a client whose prompt_open flag
		// disagreed with what its tail actually shows would flap the card and ring
		// the phone in a loop. prompt_open stays stored above, but purely advisory.
		if looksLikePrompt(tail) {
			raises = append(raises, promptRaise{id: s.ID, tail: tail})
		}
	}
	remoteMu.Unlock()

	// Raise or refresh a card for each agent showing a dialog — the SAME shared
	// judge (raiseOrRefreshPrompt) the tmux paths run, so a remote card is
	// indistinguishable downstream. Attest-time detection is the PRIMARY rung for
	// a remote agent: the user-global Notification hook (cli.go installs it in
	// ~/.claude/settings.json, so even a vterm-hosted session fires it) judges
	// transportFor(c).Capture(c) the instant it lands — but for a remote contact
	// that is the LAST attested tail, up to an attest interval stale, so the hook
	// usually sees no dialog yet and, firing exactly once, never retries. The
	// attest that CARRIES the dialog is the reliable raise. MarkPrompt (inside the
	// helper) decides raise-vs-refresh-vs-nothing atomically, so re-attesting an
	// unchanged dialog every ~10s never re-rings, and a dialog SWAPPED for a
	// different command re-captions the card instead of leaving stale text under
	// the approve buttons. No clear path here: verifyPrompt owns the clear.
	// MUST run after remoteMu is released — the helper touches the registry and
	// pushes (the load-bearing concurrency rule at the top of this file).
	for _, pr := range raises {
		c := registry.Resolve(pr.id)
		if c == nil || c.Status != "live" {
			continue
		}
		raiseOrRefreshPrompt(c, pr.tail)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ttl_s": remoteTTLSeconds()})
}

// handleTransportMail long-polls a lease's outbox: it returns EVERY currently
// parked (un-acked) delivery at once (the client dedups by id) and then the
// client acks. GET /local/transport/mail?lease=…&wait=N (wait clamped to
// [0,25]s). An unknown/stale lease is 410, including if it dies mid-poll.
func handleTransportMail(w http.ResponseWriter, r *http.Request) {
	lease := r.URL.Query().Get("lease")
	deadline := time.Now().Add(clampMailWait(r.URL.Query().Get("wait")))

	type outDelivery struct {
		ID      string `json:"id"`
		Contact string `json:"contact"`
		Text    string `json:"text,omitempty"`
		Key     string `json:"key,omitempty"`
	}
	for {
		remoteMu.Lock()
		l := remoteLeases[lease]
		if l == nil || l.stale() {
			remoteMu.Unlock()
			writeJSON(w, http.StatusGone, map[string]string{"error": "lease-expired"})
			return
		}
		if len(l.outbox) > 0 {
			out := make([]outDelivery, 0, len(l.outbox))
			for _, pd := range l.outbox {
				out = append(out, outDelivery{ID: pd.ID, Contact: pd.Contact, Text: pd.Text, Key: pd.Key})
			}
			remoteMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
			return
		}
		remoteMu.Unlock()
		if !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"deliveries": []outDelivery{}})
			return
		}
		time.Sleep(remotePollInterval)
	}
}

// handleTransportAck marks parked deliveries done: the client typed them, so the
// blocked Deliver/SendKey returns success and the entry leaves the outbox. Body:
// {"lease","ids":[…]}. Unknown ids (already timed out and redelivered under a new
// id, or never ours) are ignored — ack is idempotent and never refreshes the
// lease (only the heartbeat proves liveness).
func handleTransportAck(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Lease string   `json:"lease"`
		IDs   []string `json:"ids"`
	}
	_ = json.Unmarshal(data, &req)

	remoteMu.Lock()
	l := remoteLeases[req.Lease]
	if r.URL.Path == "/local/transport/v2/ack" &&
		(l == nil || l.stale() || l.protocol < 2) {
		if l != nil && l.stale() {
			remoteDeleteLeaseLocked(req.Lease)
		}
		remoteMu.Unlock()
		writeJSON(w, http.StatusGone, map[string]string{"error": "lease-expired-or-not-v2"})
		return
	}
	if l != nil {
		for _, id := range req.IDs {
			for _, pd := range l.outbox {
				if pd.ID == id {
					pd.acked = true // observed by the parking Deliver via its retained pointer
					break
				}
			}
			remoteRemoveParkedLocked(l, id)
		}
	}
	remoteMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// Sanitizers
// ---------------------------------------------------------------------------

// sanitizeFlavor reduces a client-supplied transport label to a safe token:
// lowercased, [a-z0-9-] only, at most 16 runes. Empty or fully-stripped input
// falls back to "remote" — the mechanism's own name, the honest default when a
// client offers no usable environment label.
func sanitizeFlavor(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 16 { // every kept rune is single-byte ASCII, so bytes == runes
		out = out[:16]
	}
	if out == "" {
		out = "remote"
	}
	return out
}

// clampScreenTail bounds an attested screen snapshot to 4096 bytes, KEEPING THE
// TAIL: a permission dialog lives at the bottom of the screen, so the head is the
// disposable end. The byte cut is nudged forward to the next rune boundary so a
// multi-byte character split by the cut is dropped whole, never left as a broken
// leading fragment.
func clampScreenTail(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// clampMailWait parses and bounds the /mail long-poll duration to [0,25]s.
func clampMailWait(s string) time.Duration {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		n = 0
	}
	if n > 25 {
		n = 25
	}
	return time.Duration(n) * time.Second
}

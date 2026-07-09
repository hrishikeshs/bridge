package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Contact is a managed agent: a stable, daemon-minted identity that outlives
// session churn. The Contact (and its thread and outbox) persists across daemon
// restarts and reconnects, keyed by name+directory; its id never changes.
type Contact struct {
	ID          string `json:"id"`                   // daemon-minted uuid for the agent; the immutable identity
	Name        string `json:"name"`                 // self-chosen address, unique among the living (a display name; may be suffixed)
	Directory   string `json:"directory"`            // the agent's working directory
	SessionID   string `json:"session_id"`           // Claude Code conversation id; `claude --resume` PRESERVES it (verified empirically), so it is stable across resume — the tail pins on it, falling back to the newest .jsonl only if it goes missing/stale (see sessionFileFor)
	TmuxTarget  string `json:"tmux_target"`          // tmux window id ("@N") hosting the agent; legacy rows may hold "bridge:<name>"
	Status      string `json:"status"`               // "live" | "offline"
	Health      string `json:"health"`               // "ok" | "working" | "prompt" | "offline"
	PromptOpen  bool   `json:"prompt_open"`          // a permission prompt is hook-attested open
	PromptSince int64  `json:"prompt_since"`         // unix seconds the current prompt opened; 0 when none (drives frozen-agent escalation)
	PromptSig   string `json:"prompt_sig,omitempty"` // firstPromptLine of the dialog CURRENTLY on the card; set/cleared with PromptOpen under the lock so a dialog swapped for a different command refreshes the card instead of showing stale text. "" when no card is up.

	// Transport names the mechanism that physically reaches this agent
	// (transport.go). Empty means "tmux" — every roster row registered before
	// the field existed migrates by doing nothing.
	Transport string `json:"transport,omitempty"`

	// TransportFlavor is the remote client's self-reported environment label
	// ("emacs", "sim") — surfaced in /api/status so the phone can tell where an
	// agent actually lives. "remote" is the MECHANISM (Transport); the flavor is
	// the ENVIRONMENT riding it. Empty for tmux agents.
	TransportFlavor string `json:"transport_flavor,omitempty"`

	// Away is the agent's self-set AIM-style status line ("brb, compiling"),
	// set by `bridge status` and shown beside its name on the phone. Empty when
	// none. It is durable, agent-authored metadata — unlike Health (a live
	// runtime signal), it survives an offline blip until the agent clears it.
	Away string `json:"away,omitempty"`

	// Fields are plugin-set key/value annotations (docs/plugins.md set-field):
	// expertise tags, last-memory-save stamps, whatever a plugin wants to pin
	// on a contact. Capped at maxContactFields; surfaced in /api/status.
	Fields map[string]string `json:"fields,omitempty"`

	// ContextTokens is the newest current-context token count read passively from
	// this agent's session-JSONL usage line (input + both cache tiers, tail.go);
	// ContextModel is the model that reading came from, whose window sizes the
	// percentage (context.go). Set by the tail via SetContext; 0/"" until a
	// usage-bearing assistant line is seen, so a sessionless agent simply omits
	// context_pct on /api/status. Both omitempty — a pre-context roster loads
	// unchanged. This is a live gauge, not durable identity, but it persists
	// harmlessly across a restart (the next tail line refreshes it).
	ContextTokens int    `json:"context_tokens,omitempty"`
	ContextModel  string `json:"context_model,omitempty"`

	// LastSeen is the unix time the daemon last had positive evidence this contact
	// was alive: stamped when it connects/revives (it is live now) and when it
	// strikes offline (last seen alive ≈ the strike). Its gap to a later wake is
	// the "since you woke" away duration (wakedigest.go), and it is the last-seen
	// substrate route-health's staleness will read (docs/route-health.md). 0 until
	// first seen; omitempty so a pre-feature roster loads unchanged.
	LastSeen int64 `json:"last_seen,omitempty"`
	// LastDigestAt is the unix time the last since-you-woke digest fired for this
	// contact — the debounce that keeps a flapping reconnect within one wake from
	// re-firing a second digest (at most one per wake). 0 = never; omitempty.
	LastDigestAt int64 `json:"last_digest_at,omitempty"`

	// wakeAwaySeconds is a transient, in-memory-only hand-off from Connect/
	// ConnectRemote to the wake site (wakedigest.go's prependWakeDigest): when > 0
	// this connect was a genuine wake after that many seconds away, so the caller
	// should lead the flush with a since-you-woke digest. Unexported, so it never
	// serializes and never persists — a one-shot hint carried only on the returned
	// copy, not durable identity.
	wakeAwaySeconds int64
}

// maxContactFields bounds plugin annotations per contact so a chatty plugin
// can't bloat contacts.json.
const maxContactFields = 16

// copy returns a detached copy safe to hand out from under the registry lock.
func (c *Contact) copy() *Contact {
	cp := *c
	return &cp
}

// MailMessage is an inbound message queued for a contact — offline mail, or a
// live message briefly held for coalescing (coalesce.go) — delivered when the
// contact connects, revives, or its hold timer fires.
type MailMessage struct {
	From string `json:"from"` // sender's address (bare name)
	Via  string `json:"via"`  // channel it arrived on ("phone" | "bridge")
	Text string `json:"text"`
	TS   string `json:"ts"`
	// Room is the room this message was fanned out to ("#crew"), empty for a 1:1
	// message. It authors the " in #crew" fragment of the delivered frame
	// (formatInbound) and is part of the mailbox grouping key (PeekMailboxGroup),
	// so a room message and a 1:1 message from the same sender never merge into
	// one frame. Additive and omitempty — a pre-rooms mailbox file loads unchanged.
	Room string `json:"room,omitempty"`
	// Emitted marks a message whose "sent" event was already emitted at accept
	// time (the live/coalescing path), so the flush must not emit it again.
	Emitted bool `json:"emitted,omitempty"`
}

// Registry is the daemon's roster of contacts plus a per-contact mailbox for
// offline delivery, persisted together to ~/.bridge/contacts.json.
type Registry struct {
	mu       sync.Mutex
	contacts map[string]*Contact
	mailbox  map[string][]MailMessage
	flushing map[string]bool // ids with a mailbox flush in progress
}

// registry is the process-wide roster.
var registry = &Registry{
	contacts: map[string]*Contact{},
	mailbox:  map[string][]MailMessage{},
	flushing: map[string]bool{},
}

// maxMailbox caps a contact's offline queue; enqueuing past it drops the oldest
// so a peer flooding an offline contact can't grow the file unbounded (M8).
const maxMailbox = 100

// registryFile is the on-disk shape of the registry.
type registryFile struct {
	Contacts []*Contact               `json:"contacts"`
	Mailbox  map[string][]MailMessage `json:"mailbox"`
}

// loadRegistry restores contacts and mailboxes from disk. Every contact loads
// offline: a fresh daemon owns no tmux sessions until agents reconnect.
func loadRegistry() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	data, err := os.ReadFile(bridgePath("contacts.json"))
	if err != nil {
		return
	}
	var rf registryFile
	if json.Unmarshal(data, &rf) != nil {
		// Corrupt roster: preserve it for forensics rather than letting the next
		// save() silently clobber it with an empty registry (M3). Atomic writes
		// make this rare; this is the belt-and-suspenders half, matching events.go.
		_ = os.Rename(bridgePath("contacts.json"), bridgePath("contacts.json.corrupt"))
		return
	}
	registry.contacts = map[string]*Contact{}
	for _, c := range rf.Contacts {
		c.Status = "offline"
		c.Health = "offline"
		c.PromptOpen = false
		registry.contacts[c.ID] = c
	}
	if rf.Mailbox != nil {
		registry.mailbox = rf.Mailbox
	}
}

// save persists the roster and mailboxes 0600. Caller holds r.mu.
func (r *Registry) save() {
	rf := registryFile{Mailbox: r.mailbox}
	for _, c := range r.contacts {
		rf.Contacts = append(rf.Contacts, c)
	}
	data, _ := json.Marshal(rf)
	_ = writeFilePrivate(bridgePath("contacts.json"), data)
}

// Connect registers or revives a contact and marks it live. A contact is
// identified across restarts by name+directory: reconnecting under the same
// pair revives the existing identity (keeping its thread and outbox); anything
// new mints a fresh uuid. Returns a copy of the live contact.
func (r *Registry) Connect(name, directory, sessionID, tmuxTarget string) *Contact {
	r.mu.Lock()
	var c *Contact
	for _, existing := range r.contacts {
		if existing.Name == name && existing.Directory == directory {
			c = existing
			break
		}
	}
	// Snapshot the pre-connect liveness BEFORE any mutation, for the wake detector
	// (noteSeenAndWakeLocked): a genuine wake is prevStatus != "live" plus a
	// trusted, long-enough LastSeen gap. A brand-new contact leaves these zero.
	prevStatus, prevLastSeen := "", int64(0)
	if c != nil {
		prevStatus, prevLastSeen = c.Status, c.LastSeen
	}
	// Sanitize a NEW registration here, at the choke point every producer flows
	// through, so no caller of the local connect endpoint (a rollback binary, a
	// stale client) can bypass runConnect's CLI-side checks and register a
	// routing-hostile identity (C2). A *revive* of an existing contact is left
	// alone: its name was validated when first registered, and the session
	// manager has already migrated a legacy name-based target to a window id
	// before calling us — so legacy stored state still reconciles.
	if c == nil {
		if !nameConnectRe.MatchString(name) {
			// A numeric/relative/whitespace name is exactly what tmux misresolves
			// via its target grammar; replace it with a safe generated address
			// rather than store it as a routing key.
			name = generateName(r.liveNames())
		}
		if !strings.HasPrefix(tmuxTarget, "@") {
			// Only a window id ("@N") is immune to tmux's target grammar. A
			// name-based target ("bridge:1") could send-keys to a window *index* —
			// the original misroute. Neutralize it: an empty target never routes
			// (tmuxAlive is false), so the contact stays offline until it
			// reconnects with a real window id.
			tmuxTarget = ""
		}
	}
	// Guarantee the name is unique among *live* contacts so the phone can address
	// an agent unambiguously. On collision with a different live contact, append
	// a numeric suffix (swift-fox-2, -3, ...). The final name is returned via
	// Contact.Name so the caller can reconcile the tmux window title.
	final := name
	for n := 2; r.liveNameTaken(final, c); n++ {
		final = fmt.Sprintf("%s-%d", name, n)
	}
	reason := "revive"
	if c == nil {
		reason = "connect"
		c = &Contact{ID: newID(), Name: final, Directory: directory}
		r.contacts[c.ID] = c
	} else {
		c.Name = final
		// Connect IS the tmux path: a contact that had been living on the remote
		// transport and now reconnects via tmux must flip back, or it would keep
		// Transport="remote" atop a fresh TmuxTarget and never deliver — the remote
		// transport keys liveness off a lease this tmux reconnect has no part in.
		c.Transport = ""
		c.TransportFlavor = ""
	}
	if final != name {
		reason = "suffixed"
	}
	c.SessionID = sessionID
	c.TmuxTarget = tmuxTarget
	c.Status = "live"
	c.Health = "ok"
	c.PromptOpen = false
	away := r.noteSeenAndWakeLocked(c, prevStatus, prevLastSeen)
	r.save()
	cp := c.copy()
	r.mu.Unlock()
	// Dispatch the plugin event OUTSIDE registry.mu: dispatchPluginEvent →
	// maybeRescanPlugins os.Stats the plugins dir and, on any mtime bump, execs
	// each plugin's manifest (10s timeout, sequential) — holding registry.mu
	// across that would freeze every registry op (send, status, flushMailbox) and
	// stall the phone. The post-mutation copy is captured under the lock, so the
	// dispatch sees exactly this connect's state with nothing held. Same hoist the
	// remote transport does (remote.go) and Queue/QueueFront do for post-lock I/O.
	dispatchPluginEvent("agent.connect", cp, map[string]any{"reason": reason})
	cp.wakeAwaySeconds = away // one-shot wake hint for prependWakeDigest (transient, not persisted)
	return cp
}

// ConnectRemote registers or revives a contact hosted by a remote client
// (docs/transports.md) — the remote analogue of Connect. It shares Connect's
// name+directory identity key and all its sanitization, but flips the contact
// onto the remote transport and records the client's flavor label. The one rule
// Connect never needs: a LIVE tmux contact under this name+directory is NEVER
// adopted here — a hello that collides with a running tmux agent mints a fresh
// suffixed identity (marvin-2) rather than hijacking the pane. Returns a copy.
func (r *Registry) ConnectRemote(name, directory, sessionID, flavor string) *Contact {
	r.mu.Lock()
	var c *Contact
	for _, existing := range r.contacts {
		if existing.Name == name && existing.Directory == directory {
			c = existing
			break
		}
	}
	// (a) A live tmux agent must not be hijacked by a remote hello: drop the
	// match so the suffix loop below mints a NEW contact. (b) A live REMOTE match
	// is the same client re-hello'ing — adopt it (keep the id, refresh the
	// session). (c) An offline match of either transport is adopted and rehomed
	// onto remote below (identity, thread and mailbox survive). (d) No match at
	// all mints a new contact.
	if c != nil && c.Status == "live" && c.Transport != "remote" {
		c = nil
	}
	// Snapshot pre-connect liveness for the wake detector, AFTER the hijack-avoid
	// drop above so a dropped live-tmux collision is treated as a fresh contact
	// (no wake), never a wake of the pane it declined to hijack.
	prevStatus, prevLastSeen := "", int64(0)
	if c != nil {
		prevStatus, prevLastSeen = c.Status, c.LastSeen
	}
	if c == nil {
		// Sanitize a NEW registration at the choke point, exactly as Connect does:
		// a numeric/relative name is a tmux target-grammar hazard even for a
		// contact that starts life on the remote transport, because it may later
		// rehome to tmux and /api addressing must stay unambiguous.
		if !nameConnectRe.MatchString(name) {
			name = generateName(r.liveNames())
		}
	}
	// Unique among the living, the same suffix ladder Connect uses.
	final := name
	for n := 2; r.liveNameTaken(final, c); n++ {
		final = fmt.Sprintf("%s-%d", name, n)
	}
	reason := "revive"
	if c == nil {
		reason = "hello"
		c = &Contact{ID: newID(), Name: final, Directory: directory}
		r.contacts[c.ID] = c
	} else {
		c.Name = final
	}
	if final != name {
		reason = "suffixed"
	}
	c.SessionID = sessionID
	c.Status = "live"
	c.Health = "ok"
	c.PromptOpen = false
	// Rehome onto the remote transport: a formerly-tmux offline row flips here
	// (keeping its id, thread and mailbox), and TmuxTarget is cleared so a stale
	// window id can never route (tmuxAlive is false on "").
	c.Transport = "remote"
	c.TransportFlavor = flavor
	c.TmuxTarget = ""
	away := r.noteSeenAndWakeLocked(c, prevStatus, prevLastSeen)
	r.save()
	cp := c.copy()
	r.mu.Unlock()
	// Dispatch OUTSIDE registry.mu — the plugin manifest exec (10s) must never run
	// under the lock (see Connect and remote.go's load-bearing rule). The
	// post-mutation copy captured above is what the dispatch reads.
	dispatchPluginEvent("agent.connect", cp, map[string]any{"reason": reason})
	cp.wakeAwaySeconds = away // one-shot wake hint for prependWakeDigest (transient, not persisted)
	return cp
}

// noteSeenAndWakeLocked stamps a contact's last-seen bookkeeping at the moment it
// becomes live and decides whether this transition earns a since-you-woke digest.
// It is the transport-agnostic heart of the wake detector, shared by Connect and
// ConnectRemote. A digest fires only when ALL hold:
//
//   - the feature is on (wakeDigestMinAwaySeconds() > 0; 0 disables it entirely);
//   - the contact was genuinely AWAY — prevStatus != "live" (a routine re-hello of
//     a still-live contact is not a wake);
//   - the away gap is TRUSTWORTHY — prevLastSeen was stamped by THIS daemon
//     instance (>= daemonStartUnix), so a restart's blank slate can never invent a
//     giant "back after 40d" gap out of a pre-restart timestamp;
//   - the gap is at least the threshold; and
//   - the per-contact debounce has elapsed (now - LastDigestAt >= threshold), so a
//     flapping reconnect within one wake can't re-fire.
//
// It returns the away duration in seconds when a digest should fire, else 0, and
// always advances LastSeen (and, on a fire, LastDigestAt) so the next cycle is
// clean. Caller holds r.mu and has already set c.Status live.
func (r *Registry) noteSeenAndWakeLocked(c *Contact, prevStatus string, prevLastSeen int64) int64 {
	now := timeNowUnix()
	away := int64(0)
	if threshold := wakeDigestMinAwaySeconds(); threshold > 0 &&
		prevStatus != "live" &&
		prevLastSeen >= daemonStartUnix &&
		now-prevLastSeen >= threshold &&
		now-c.LastDigestAt >= threshold {
		away = now - prevLastSeen
		c.LastDigestAt = now
	}
	c.LastSeen = now
	return away
}

// liveNameTaken reports whether a live contact other than `self` already answers
// to `name`. Caller holds r.mu.
func (r *Registry) liveNameTaken(name string, self *Contact) bool {
	for _, c := range r.contacts {
		if c != self && c.Status == "live" && c.Name == name {
			return true
		}
	}
	return false
}

// liveNames returns the set of names held by live contacts, for generating a
// fresh non-colliding address. Caller holds r.mu.
func (r *Registry) liveNames() map[string]bool {
	taken := map[string]bool{}
	for _, c := range r.contacts {
		if c.Status == "live" && c.Name != "" {
			taken[c.Name] = true
		}
	}
	return taken
}

// Resolve maps a handle (contact id, or a name unique among the living) to a
// contact, preferring a live match. Returns nil when nothing matches.
func (r *Registry) Resolve(handle string) *Contact {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.contacts[handle]; ok {
		return c.copy()
	}
	var offline *Contact
	for _, c := range r.contacts {
		if c.Name == handle {
			if c.Status == "live" {
				return c.copy()
			}
			offline = c
		}
	}
	if offline != nil {
		return offline.copy()
	}
	return nil
}

// BySession returns the contact whose current session matches sessionID (used
// to route Notification-hook events), or nil.
func (r *Registry) BySession(sessionID string) *Contact {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID == "" {
		return nil
	}
	for _, c := range r.contacts {
		if c.SessionID == sessionID {
			return c.copy()
		}
	}
	return nil
}

// Roster returns a snapshot of all contacts, live first then offline, each
// group sorted by name for a stable ordering.
func (r *Registry) Roster() []*Contact {
	r.mu.Lock()
	defer r.mu.Unlock()
	var live, offline []*Contact
	for _, c := range r.contacts {
		if c.Status == "live" {
			live = append(live, c.copy())
		} else {
			offline = append(offline, c.copy())
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	sort.Slice(offline, func(i, j int) bool { return offline[i].Name < offline[j].Name })
	return append(live, offline...)
}

// SetPrompt marks whether a contact has an open (hook-attested) permission
// prompt and updates its health to match. Closing a prompt also clears its
// signature (PromptSig), so a later revival can never inherit a stale card
// caption. RAISING and REFRESHING go through MarkPrompt instead — this method
// is the CLEAR path (and the direct revive set); it never opens a card whose
// caption text hasn't been recorded.
func (r *Registry) SetPrompt(id string, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok {
		return
	}
	// Stamp the moment a prompt opens (and clear it when it closes) so the
	// frozen-agent watchdog can age it. Don't restamp an already-open prompt.
	if open && !c.PromptOpen {
		c.PromptSince = timeNowUnix()
	} else if !open {
		c.PromptSince = 0
		c.PromptSig = ""
	}
	c.PromptOpen = open
	switch {
	case open:
		c.Health = "prompt"
	case c.Status == "live":
		c.Health = "ok"
	}
	r.save()
}

// promptChange is what MarkPrompt decided: nothing, a fresh card, or the same
// card re-captioned for a new command.
type promptChange int

const (
	promptNoChange promptChange = iota
	promptRaised
	promptRefreshed
)

// MarkPrompt records that a dialog with caption `sig` (the command line) is
// showing, and reports — atomically under the one lock — whether that NEWLY
// raised the card (was closed), REFRESHED it (open, but a different command
// than the caption it last carried), or changed nothing (same command). The
// decision lives here, not in a caller reading a Contact copy, so two
// goroutines detecting the same dialog (the hook handler and the reconcile
// re-check) can never both "raise" it and double-ring the phone. PromptSince is
// stamped only on a true raise — a refresh is still one continuous wait, so the
// frozen-prompt clock must not reset when only the command text changes.
func (r *Registry) MarkPrompt(id, sig string) promptChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok {
		return promptNoChange
	}
	switch {
	case !c.PromptOpen:
		c.PromptOpen = true
		c.PromptSig = sig
		c.PromptSince = timeNowUnix()
		c.Health = "prompt"
		r.save()
		return promptRaised
	case sig != c.PromptSig:
		c.PromptSig = sig // a new command under the same open card; clock unchanged
		r.save()
		return promptRefreshed
	default:
		return promptNoChange
	}
}

// SetOffline marks a contact offline (its managed tmux session ended).
func (r *Registry) SetOffline(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok {
		return
	}
	c.Status = "offline"
	c.Health = "offline"
	c.PromptOpen = false
	c.PromptSig = "" // no card survives going offline; don't let a revival inherit its caption
	c.LastSeen = timeNowUnix() // last observed alive ≈ now; the away clock the wake digest measures from
	r.save()
}

// Queue appends a message to a contact's offline mailbox. The queue is
// bounded at maxMailbox; past the cap, the flooder pays first — the oldest
// message from the SAME sender is evicted before anyone else's, so a peer
// hammering an offline contact can't push out the user's own words. Every
// eviction is audited (outside the lock): each queued message was already
// acked to its sender as accepted, so a silent drop is a quiet lie (review
// round 2).
func (r *Registry) Queue(id string, m MailMessage) {
	r.mu.Lock()
	q := append(r.mailbox[id], m)
	var dropped *MailMessage
	if len(q) > maxMailbox {
		cut := 0 // fallback: oldest overall
		for i, old := range q[:len(q)-1] {
			if old.From == m.From {
				cut = i
				break
			}
		}
		d := q[cut]
		dropped = &d
		q = append(q[:cut], q[cut+1:]...)
	}
	r.mailbox[id] = q
	r.save()
	r.mu.Unlock()
	if dropped != nil {
		audit("mailbox-overflow",
			fmt.Sprintf("dropped %s -> %.8s (cap %d): %.80s", dropped.From, id, maxMailbox, dropped.Text),
			"daemon")
	}
}

// QueueFront prepends a message to the HEAD of a contact's mailbox so it is the
// next group flushMailbox delivers — the seam the since-you-woke digest uses to
// lead a waking contact's backlog (wakedigest.go). Every already-queued message
// keeps its relative order; only a new head is inserted, so the real backlog is
// never reordered. Unlike Queue it does NOT evict at the cap: the lead-in is one
// short daemon line that delivers (and drops) first, so a one-past-cap blip must
// never push out a real queued message. Persisted like every mailbox mutation.
func (r *Registry) QueueFront(id string, m MailMessage) {
	r.mu.Lock()
	r.mailbox[id] = append([]MailMessage{m}, r.mailbox[id]...)
	r.save()
	r.mu.Unlock()
}

// MailboxBacklogCounts reports how many queued messages are #crew/room traffic vs
// direct 1:1 messages — read straight from the durable mailbox, which is the
// exact backlog a wake flush is about to deliver ("held above"). The split is the
// daemon-set Room field (never body text; H9): Room != "" is room/crew traffic,
// empty is direct. A From-less daemon lead-in (a wake digest itself) is never
// counted. prependWakeDigest uses this to color the since-you-woke line. Caller
// must NOT hold r.mu.
func (r *Registry) MailboxBacklogCounts(id string) (crew, direct int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.mailbox[id] {
		switch {
		case m.From == "": // a daemon lead-in (e.g. a prior digest) — never self-count
			continue
		case m.Room != "":
			crew++
		default:
			direct++
		}
	}
	return crew, direct
}

// BeginFlush claims the (single) flush slot for a mailbox; a false return
// means another flush is already draining it and the caller must not start a
// second one. Pair every true return with EndFlush.
func (r *Registry) BeginFlush(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flushing == nil {
		r.flushing = map[string]bool{}
	}
	if r.flushing[id] {
		return false
	}
	r.flushing[id] = true
	return true
}

// EndFlush releases the flush slot claimed by BeginFlush.
func (r *Registry) EndFlush(id string) {
	r.mu.Lock()
	delete(r.flushing, id)
	r.mu.Unlock()
}

// Group caps for PeekMailboxGroup: a combined delivery stays a readable,
// send-keys-safe single line.
const (
	mailGroupMaxMsgs  = 8
	mailGroupMaxBytes = 6000
)

// HasMail reports whether a contact has any queued mail awaiting delivery —
// the cheap gate the reconcile loop checks before attempting a flush.
func (r *Registry) HasMail(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.mailbox[id]) > 0
}

// MailboxOldestAgeSeconds returns how long, in whole seconds, the OLDEST message
// still queued for a contact has waited — 0 when the mailbox is empty. It parses
// the RFC3339 MailMessage.TS that every queue path stamps (nowUTC) and compares
// against the same clock (time.Now), so route-health (routehealth.go) can tell a
// fresh burst still inside the coalescing/fan-out window from a genuinely STALLED
// backlog. A message whose TS is empty or unparseable is skipped — it can't prove
// its age, and must never be counted as infinitely old. Pure read, zero writes.
func (r *Registry) MailboxOldestAgeSeconds(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var oldest time.Time
	found := false
	for _, m := range r.mailbox[id] {
		ts, err := time.Parse(time.RFC3339, m.TS)
		if err != nil {
			continue
		}
		if !found || ts.Before(oldest) {
			oldest, found = ts, true
		}
	}
	if !found {
		return 0
	}
	age := int(time.Since(oldest) / time.Second)
	if age < 0 { // a clock skew / future TS must never report negative age
		age = 0
	}
	return age
}

// PeekMailboxGroup returns (a copy of) the longest queue prefix that shares
// one sender and channel, within the group caps — the run flushMailbox will
// deliver as a single combined line. Empty when the mailbox is empty.
func (r *Registry) PeekMailboxGroup(id string) []MailMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.mailbox[id]
	if len(q) == 0 {
		return nil
	}
	out := []MailMessage{q[0]}
	total := len(q[0].Text)
	for _, m := range q[1:] {
		// Room joins From+Via in the same-group predicate: the frame is applied
		// once per group from group[0], so a 1:1 message must never be swallowed
		// into a room frame (or a room message into a 1:1 one).
		if m.From != q[0].From || m.Via != q[0].Via || m.Room != q[0].Room {
			break
		}
		if len(out) >= mailGroupMaxMsgs || total+len(m.Text) > mailGroupMaxBytes {
			break
		}
		out = append(out, m)
		total += len(m.Text)
	}
	return out
}

// DropMailbox removes the given delivered messages and persists. Each is
// re-found by value rather than assumed to still head the queue — a concurrent
// Queue at the maxMailbox cap may have shifted or dropped it (#5). Crash
// between delivery and this call at worst redelivers one group (at-least-once,
// same contract the per-message flush had).
func (r *Registry) DropMailbox(id string, delivered []MailMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range delivered {
		q := r.mailbox[id]
		for i := range q {
			if q[i] == m {
				r.mailbox[id] = append(q[:i:i], q[i+1:]...)
				break
			}
		}
	}
	if len(r.mailbox[id]) == 0 {
		delete(r.mailbox, id)
	}
	r.save()
}

// SetHealth updates a live contact's health indicator (e.g. "working" while
// its session file is growing). No-op for offline contacts.
func (r *Registry) SetHealth(id, health string) {
	var idle *Contact
	r.mu.Lock()
	if c, ok := r.contacts[id]; ok && c.Status == "live" {
		wasWorking := c.Health == "working"
		c.Health = health
		r.save()
		if wasWorking && health == "ok" {
			// The tail went quiet after activity: the agent settled — the
			// agent.idle plugins hear about (docs/plugins.md). Capture the
			// post-mutation copy now; DISPATCH below, once the lock is released.
			idle = c.copy()
		}
	}
	r.mu.Unlock()
	if idle != nil {
		// Outside registry.mu: dispatchPluginEvent may exec a plugin's manifest
		// (10s timeout); holding the lock across it would freeze every registry
		// op — the availability rule remote.go and Queue/QueueFront already follow.
		dispatchPluginEvent("agent.idle", idle, nil)
	}
}

// SetContext records the newest context-window reading (token count + the model
// that produced it) for a contact, parsed passively from its session JSONL by
// the tail. It mirrors SetHealth: live-only — a stale tail must never move an
// offline agent's gauge — and persisted. It additionally skips the write when
// the reading is unchanged, so an agent re-emitting an identical usage line
// never churns the roster file (the tail only calls this when the file
// advanced, so a real change is the common case). The model rides along so
// /api/status can size the percentage against the right window (context.go).
func (r *Registry) SetContext(id string, tokens int, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok || c.Status != "live" {
		return
	}
	if c.ContextTokens == tokens && c.ContextModel == model {
		return
	}
	c.ContextTokens = tokens
	c.ContextModel = model
	r.save()
}

// SetAway records (or clears, on "") a contact's away/status line. It follows
// SetHealth's shape but is deliberately NOT gated on live: an away message is
// durable, agent-authored metadata (like SetField), not a live runtime signal,
// so an agent that set "brb" keeps it across a brief offline blip until it
// clears it. The caller has already flattened and capped the text
// (handleLocalStatus) — this only persists it.
func (r *Registry) SetAway(id, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.contacts[id]; ok {
		c.Away = text
		r.save()
	}
}

// Retire removes an OFFLINE contact — and its mailbox — from the roster.
// Live contacts are never retirable: a running agent must lose its window
// before it can lose its registration, or a typo could orphan someone
// mid-conversation. Matching a name therefore can only ever hit a ghost,
// even when a live contact shares it. Returns the removed contact, or nil.
func (r *Registry) Retire(handle string) *Contact {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.contacts {
		if (c.ID == handle || c.Name == handle) && c.Status != "live" {
			delete(r.contacts, id)
			delete(r.mailbox, id)
			r.save()
			return c.copy()
		}
	}
	return nil
}

// SetField pins a plugin annotation on a contact. Returns false when the
// contact is unknown or the per-contact field cap would be exceeded by a new
// key (updates to existing keys always succeed).
func (r *Registry) SetField(id, key, value string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok {
		return false
	}
	if c.Fields == nil {
		c.Fields = map[string]string{}
	}
	if _, exists := c.Fields[key]; !exists && len(c.Fields) >= maxContactFields {
		return false
	}
	c.Fields[key] = value
	r.save()
	return true
}

// SessionHeldByOther reports whether a live contact other than exceptID is
// already tailing sessionID's JSONL. The stale-file fallback consults it so it
// never adopts a sibling agent's conversation in the same directory (H2).
func (r *Registry) SessionHeldByOther(sessionID, exceptID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.contacts {
		if c.ID != exceptID && c.Status == "live" && c.SessionID == sessionID {
			return true
		}
	}
	return false
}

// SetSession records a contact's current Claude Code session id. `claude
// --resume` preserves the id today, so this changes rarely; the tail loop
// re-resolves the JSONL when it does.
func (r *Registry) SetSession(id, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok {
		return
	}
	// Don't adopt a session id already held by another live contact: two agents
	// rehomed from the same directory must not converge on one JSONL, which would
	// cross their reply threads and misroute permission cards (H2).
	if sessionID != "" {
		for _, other := range r.contacts {
			if other != c && other.Status == "live" && other.SessionID == sessionID {
				return
			}
		}
	}
	c.SessionID = sessionID
	r.save()
}

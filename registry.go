package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Contact is a managed agent: a stable, daemon-minted identity that outlives
// session churn. The Contact (and its thread and outbox) persists across daemon
// restarts and reconnects, keyed by name+directory; its id never changes.
type Contact struct {
	ID          string `json:"id"`           // daemon-minted uuid for the agent; the immutable identity
	Name        string `json:"name"`         // self-chosen address, unique among the living (a display name; may be suffixed)
	Directory   string `json:"directory"`    // the agent's working directory
	SessionID   string `json:"session_id"`   // Claude Code conversation id; `claude --resume` PRESERVES it (verified empirically), so it is stable across resume — the tail pins on it, falling back to the newest .jsonl only if it goes missing/stale (see sessionFileFor)
	TmuxTarget  string `json:"tmux_target"`  // tmux window id ("@N") hosting the agent; legacy rows may hold "bridge:<name>"
	Status      string `json:"status"`       // "live" | "offline"
	Health      string `json:"health"`       // "ok" | "working" | "prompt" | "offline"
	PromptOpen  bool   `json:"prompt_open"`  // a permission prompt is hook-attested open
	PromptSince int64  `json:"prompt_since"` // unix seconds the current prompt opened; 0 when none (drives frozen-agent escalation)

	// Fields are plugin-set key/value annotations (docs/plugins.md set-field):
	// expertise tags, last-memory-save stamps, whatever a plugin wants to pin
	// on a contact. Capped at maxContactFields; surfaced in /api/status.
	Fields map[string]string `json:"fields,omitempty"`
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

// newID mints a random RFC 4122 v4 UUID string (no external dependency).
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("bridge: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nameAdjectives and nameAnimals seed auto-generated agent addresses
// (adjective-animal, e.g. "swift-fox"). Ported from Magnus's wordlists; indices
// are drawn with the shared randInt (uniform, rejection-sampled), so the list
// lengths no longer have to divide 256 to stay unbiased.
var (
	nameAdjectives = []string{
		"swift", "bright", "calm", "bold", "keen", "wise", "quick", "sharp",
		"cool", "warm", "brave", "clever", "gentle", "lively", "noble", "merry",
	}
	nameAnimals = []string{
		"fox", "owl", "hawk", "wolf", "bear", "deer", "crow", "lynx",
		"hare", "wren", "otter", "raven", "finch", "seal", "moth", "toad",
	}
)

// generateName returns an "adjective-animal" address absent from `taken`,
// regenerating on collision up to 100 times before falling back to an
// id-suffixed form. It lives on the registry side so both the connect CLI (via
// the /local/contacts roster) and the daemon can reuse it. `taken` may be nil.
func generateName(taken map[string]bool) string {
	for i := 0; i < 100; i++ {
		n := nameAdjectives[randInt(len(nameAdjectives))] + "-" + nameAnimals[randInt(len(nameAnimals))]
		if !taken[n] {
			return n
		}
	}
	return nameAdjectives[randInt(len(nameAdjectives))] + "-" +
		nameAnimals[randInt(len(nameAnimals))] + "-" + newID()[:4]
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
	defer r.mu.Unlock()
	var c *Contact
	for _, existing := range r.contacts {
		if existing.Name == name && existing.Directory == directory {
			c = existing
			break
		}
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
	}
	if final != name {
		reason = "suffixed"
	}
	c.SessionID = sessionID
	c.TmuxTarget = tmuxTarget
	c.Status = "live"
	c.Health = "ok"
	c.PromptOpen = false
	r.save()
	dispatchPluginEvent("agent.connect", c, map[string]any{"reason": reason})
	return c.copy()
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
// prompt and updates its health to match.
func (r *Registry) SetPrompt(id string, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contacts[id]
	if !ok {
		return
	}
	// Stamp the moment a prompt opens (and clear it when it closes) so the
	// frozen-agent watchdog can age it. Don't restamp an already-open prompt —
	// re-attestation of the same dialog must not reset its clock.
	if open && !c.PromptOpen {
		c.PromptSince = timeNowUnix()
	} else if !open {
		c.PromptSince = 0
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
		if m.From != q[0].From || m.Via != q[0].Via {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.contacts[id]; ok && c.Status == "live" {
		wasWorking := c.Health == "working"
		c.Health = health
		r.save()
		if wasWorking && health == "ok" {
			// The tail went quiet after activity: the agent settled. This is
			// the agent.idle plugins hear about (docs/plugins.md).
			dispatchPluginEvent("agent.idle", c, nil)
		}
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

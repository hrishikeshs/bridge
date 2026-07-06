package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Contact is a managed agent: a stable, daemon-minted identity that outlives
// session churn. Each --resume mints a new SessionID, but the Contact (and its
// thread and outbox) persists.
type Contact struct {
	ID         string `json:"id"`          // daemon-minted uuid for the agent
	Name       string `json:"name"`        // self-chosen address, unique among the living
	Directory  string `json:"directory"`   // the agent's working directory
	SessionID  string `json:"session_id"`  // current Claude Code conversation id (churns on resume)
	TmuxTarget string `json:"tmux_target"` // tmux window id ("@N") hosting the agent; legacy rows may hold "bridge:<name>"
	Status     string `json:"status"`      // "live" | "offline"
	Health     string `json:"health"`      // "ok" | "working" | "prompt" | "offline"
	PromptOpen bool   `json:"prompt_open"` // a permission prompt is hook-attested open
}

// copy returns a detached copy safe to hand out from under the registry lock.
func (c *Contact) copy() *Contact {
	cp := *c
	return &cp
}

// MailMessage is an inbound message queued for a contact that was offline when
// it was sent, delivered when the contact next connects or revives.
type MailMessage struct {
	From string `json:"from"` // sender's address (bare name)
	Via  string `json:"via"`  // channel it arrived on ("phone" | "bridge")
	Text string `json:"text"`
	TS   string `json:"ts"`
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
// (adjective-animal, e.g. "swift-fox"). Ported from Magnus's wordlists and
// padded to 16 each for collision headroom (and clean modulo — no rejection
// sampling needed since 256 % 16 == 0).
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
		n := nameAdjectives[randIndex(len(nameAdjectives))] + "-" + nameAnimals[randIndex(len(nameAnimals))]
		if !taken[n] {
			return n
		}
	}
	return nameAdjectives[randIndex(len(nameAdjectives))] + "-" +
		nameAnimals[randIndex(len(nameAnimals))] + "-" + newID()[:4]
}

// randIndex returns a crypto-random index in [0,n); n must be in (0,256].
func randIndex(n int) int {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("bridge: crypto/rand unavailable: " + err.Error())
	}
	return int(b[0]) % n
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
	// Guarantee the name is unique among *live* contacts so the phone can address
	// an agent unambiguously. On collision with a different live contact, append
	// a numeric suffix (swift-fox-2, -3, ...). The final name is returned via
	// Contact.Name so the caller can reconcile the tmux window title.
	final := name
	for n := 2; r.liveNameTaken(final, c); n++ {
		final = fmt.Sprintf("%s-%d", name, n)
	}
	if c == nil {
		c = &Contact{ID: newID(), Name: final, Directory: directory}
		r.contacts[c.ID] = c
	} else {
		c.Name = final
	}
	c.SessionID = sessionID
	c.TmuxTarget = tmuxTarget
	c.Status = "live"
	c.Health = "ok"
	c.PromptOpen = false
	r.save()
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

// Queue appends a message to a contact's offline mailbox, dropping the oldest
// messages beyond maxMailbox so the queue (and its on-disk footprint) is bounded.
func (r *Registry) Queue(id string, m MailMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := append(r.mailbox[id], m)
	if len(q) > maxMailbox {
		q = append([]MailMessage(nil), q[len(q)-maxMailbox:]...)
	}
	r.mailbox[id] = q
	r.save()
}

// FlushMailbox delivers queued messages one at a time, removing (and persisting)
// each only after deliver returns nil. Stops at the first delivery error, leaving
// the remaining messages queued. Safe against crash-mid-flush (no delete-then-lose):
// a message leaves disk only after its delivery succeeded, so a crash between
// deliver and save at worst redelivers the last message (deliveries are idempotent
// enough for that). Concurrent flushes of the same id are coalesced to one.
func (r *Registry) FlushMailbox(id string, deliver func(MailMessage) error) {
	r.mu.Lock()
	if r.flushing == nil {
		r.flushing = map[string]bool{}
	}
	if r.flushing[id] {
		r.mu.Unlock()
		return // another flush is already draining this mailbox
	}
	r.flushing[id] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.flushing, id)
		r.mu.Unlock()
	}()

	for {
		r.mu.Lock()
		q := r.mailbox[id]
		if len(q) == 0 {
			r.mu.Unlock()
			return
		}
		m := q[0]
		r.mu.Unlock()

		if err := deliver(m); err != nil {
			return // leave m and the rest queued for the next flush
		}

		// Delivered: drop the front (still the message we delivered — Queue only
		// appends) and persist before moving on.
		r.mu.Lock()
		if q := r.mailbox[id]; len(q) > 0 {
			r.mailbox[id] = q[1:]
			if len(r.mailbox[id]) == 0 {
				delete(r.mailbox, id)
			}
		}
		r.save()
		r.mu.Unlock()
	}
}

// TakeMailbox returns and clears a contact's queued messages.
func (r *Registry) TakeMailbox(id string) []MailMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	msgs := r.mailbox[id]
	delete(r.mailbox, id)
	r.save()
	return msgs
}

// Requeue restores messages to the front of a contact's mailbox when delivery
// of a drained mailbox fails partway through.
func (r *Registry) Requeue(id string, msgs []MailMessage) {
	if len(msgs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mailbox[id] = append(append([]MailMessage{}, msgs...), r.mailbox[id]...)
	r.save()
}

// SetHealth updates a live contact's health indicator (e.g. "working" while
// its session file is growing). No-op for offline contacts.
func (r *Registry) SetHealth(id, health string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.contacts[id]; ok && c.Status == "live" {
		c.Health = health
		r.save()
	}
}

// SetSession records a contact's current Claude Code session id, which churns
// on every --resume; the tail loop re-resolves the JSONL when it changes.
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

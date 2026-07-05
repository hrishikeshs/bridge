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
	TmuxTarget string `json:"tmux_target"` // tmux target hosting the agent (bridge-<name>)
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
}

// registry is the process-wide roster.
var registry = &Registry{
	contacts: map[string]*Contact{},
	mailbox:  map[string][]MailMessage{},
}

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
	if c == nil {
		c = &Contact{ID: newID(), Name: name, Directory: directory}
		r.contacts[c.ID] = c
	}
	c.SessionID = sessionID
	c.TmuxTarget = tmuxTarget
	c.Status = "live"
	c.Health = "ok"
	c.PromptOpen = false
	r.save()
	return c.copy()
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

// Queue appends a message to a contact's offline mailbox.
func (r *Registry) Queue(id string, m MailMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mailbox[id] = append(r.mailbox[id], m)
	r.save()
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
	if c, ok := r.contacts[id]; ok {
		c.SessionID = sessionID
		r.save()
	}
}

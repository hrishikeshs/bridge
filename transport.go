package main

// transport.go — how words physically reach an agent, made pluggable.
//
// The daemon's core (mailboxes, coalescing, rooms, the guarded flush, the
// JSONL tail) never cared that agents live in tmux — only five operations
// ever did: is the agent's host alive, is it SAFE to type, deliver one line,
// capture the screen, press one approval key. This interface names those
// five. tmux is the first implementation (tmux.go); an Emacs/vterm crew — or
// any environment that can speak the local API — becomes a second without
// the core learning anything new (the 2026-07-06 "tmux is just one
// mechanism" design; docs/transports.md carries the remote-client protocol).
//
// A contact names its transport in Contact.Transport; empty means "tmux", so
// every pre-transport roster row migrates by doing nothing. An unknown name
// resolves to nullTransport, which FAILS SAFE: never alive, never ready,
// delivery errors — mail waits in the durable mailbox exactly as it does for
// an offline contact, and nothing is ever typed anywhere on a guess.

type Transport interface {
	// Alive reports whether the contact's host (pane, buffer, client) exists
	// right now. Liveness strikes and revival key off it (reconcile.go).
	Alive(c *Contact) bool
	// Ready reports whether it is safe to deliver text THIS instant — the
	// guard the 2026-07-06 criticals converged on. False defers, never drops:
	// the durable mailbox retries on the next tick.
	Ready(c *Contact) bool
	// Deliver types one prepared line into the agent's input, submitted.
	Deliver(c *Contact, text string) error
	// Capture returns a snapshot of the agent's screen for dialog detection
	// and permission cards; "" means unknown, which every caller treats as
	// "assume the worst, hold delivery".
	Capture(c *Contact) string
	// SendKey delivers one whitelisted approval keystroke ("1".."3", "y",
	// "n", or a bare "esc"). Separate from Deliver: approvals carry no
	// sender frame and esc takes no Enter.
	SendKey(c *Contact, key string) error
}

// defaultTransport is the transport of every contact registered before the
// field existed — and of every tmux rehoming since.
const defaultTransport = "tmux"

// transports is the registry of live implementations, filled by each
// transport's init(). Written only during init, read-only afterwards, so no
// lock is needed.
var transports = map[string]Transport{}

func registerTransport(name string, t Transport) { transports[name] = t }

// transportFor resolves a contact's transport, defaulting legacy rows to
// tmux and unknown names to the fail-safe null transport.
func transportFor(c *Contact) Transport {
	name := c.Transport
	if name == "" {
		name = defaultTransport
	}
	if t := transports[name]; t != nil {
		return t
	}
	return nullTransport{}
}

// nullTransport is what an unknown transport name resolves to: nothing is
// alive, nothing is ready, and every action reports the missing adapter.
// Mail queues durably and waits — the same safe shape as an offline contact.
type nullTransport struct{}

func (nullTransport) Alive(*Contact) bool            { return false }
func (nullTransport) Ready(*Contact) bool            { return false }
func (nullTransport) Deliver(*Contact, string) error { return errSessionAdapter }
func (nullTransport) Capture(*Contact) string        { return "" }
func (nullTransport) SendKey(*Contact, string) error { return errSessionAdapter }

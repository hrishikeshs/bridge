package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
	"time"
)

const (
	// historySize is how many recent events are kept in memory for reconnecting clients.
	historySize = 500
	// heartbeatEvery is the interval between SSE keep-alive comments.
	heartbeatEvery = 25 * time.Second
	// historyMaxBytes triggers a compaction of the durable history file on load.
	historyMaxBytes = 2 * 1024 * 1024
)

// Event is a durable, monotonically-numbered item in a conversation thread.
// Every subsystem speaks by emitting one. Transient signals (typing) are sent
// without an id and never stored.
type Event struct {
	ID    int64  `json:"id"`
	TS    string `json:"ts"`
	Type  string `json:"type"`
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	// ClientID echoes the phone's client_id back on a "sent" acknowledgement so
	// the PWA can dedup its optimistic local echo by id rather than by text
	// (M5). Empty (and omitted) for every server-originated event.
	ClientID string `json:"client_id,omitempty"`
	// SourceID is the stable id of a retryable semantic-transport event. It is
	// stored in ordinary durable history so a daemon restart can recognize that
	// an already-visible reply/plan is a retry, not a second phone bubble.
	SourceID string `json:"source_id,omitempty"`
	// Target is the id of the event a "reaction" decorates (0/omitted on every
	// other type), so the phone can fold an emoji onto the bubble it points at.
	Target int64 `json:"target,omitempty"`
	// QuoteName / QuoteExcerpt ride on a "sent" event that quotes an earlier
	// bubble: the quoted author and a short, sanitized excerpt of it, rendered as
	// an inset above the message. Additive and optional, so pre-quote history
	// still loads unchanged.
	QuoteName    string `json:"quote_name,omitempty"`
	QuoteExcerpt string `json:"quote_excerpt,omitempty"`
}

var (
	eventsMu       sync.Mutex
	events         []Event // newest first, capped at historySize
	eventCounter   int64
	historyAppends int
	// historyWriteMu serializes history-file writes performed outside eventsMu,
	// so a lock-free append can't interleave with a compaction rewrite (M9).
	historyWriteMu sync.Mutex
)

// sseClient is one connected /api/events subscriber; frames are queued on ch.
type sseClient struct {
	ch chan string
}

var (
	hubMu      sync.Mutex
	sseClients = map[*sseClient]struct{}{}
)

// addSSEClient registers c to receive broadcast frames.
func addSSEClient(c *sseClient) {
	hubMu.Lock()
	sseClients[c] = struct{}{}
	hubMu.Unlock()
}

// removeSSEClient unregisters c and closes its channel exactly once.
func removeSSEClient(c *sseClient) {
	hubMu.Lock()
	if _, ok := sseClients[c]; ok {
		delete(sseClients, c)
		close(c.ch)
	}
	hubMu.Unlock()
}

// broadcast delivers a preformatted SSE frame to every connected client,
// dropping any whose buffer is full (a stalled reader) rather than blocking.
func broadcast(frame string) {
	hubMu.Lock()
	defer hubMu.Unlock()
	for c := range sseClients {
		select {
		case c.ch <- frame:
		default:
			delete(sseClients, c)
			close(c.ch)
		}
	}
}

// Emit is the convenience entry point every subsystem uses to speak: it builds
// an Event from the common fields and files it via emitEvent (which assigns the
// id, appends to history and broadcasts), returning the stored event. An
// optional clientID is echoed on "sent" acknowledgements for phone-echo dedup
// (M5); server-originated events pass none.
func Emit(typ, agent, name, text string, clientID ...string) Event {
	ev := Event{Type: typ, Agent: agent, Name: name, Text: text}
	if len(clientID) > 0 {
		ev.ClientID = clientID[0]
	}
	return emitEvent(ev)
}

// emitEvent files a pre-built event: it stamps the next monotonic id and a UTC
// timestamp, appends it to the in-memory ring and the durable history file, and
// broadcasts it to SSE clients. Emit builds the common case; callers that must
// set an optional field (a "sent" that carries a quote, a "reaction" that
// carries a Target) construct the Event themselves and file it here, so id
// assignment, durable append and broadcast keep living in exactly one place. The
// caller leaves ID and TS zero; both are stamped here under eventsMu.
func emitEvent(ev Event) Event {
	stored, _ := emitEventOnce(ev)
	return stored
}

// emitEventOnce files ev unless its non-empty SourceID is already present in
// durable history. The duplicate check and in-memory append share eventsMu;
// disk append completes before broadcast, so every user-visible semantic event
// has a persisted receipt before a client can observe it.
func emitEventOnce(ev Event) (Event, bool) {
	eventsMu.Lock()
	if ev.SourceID != "" {
		for _, existing := range events {
			if existing.SourceID == ev.SourceID {
				eventsMu.Unlock()
				return existing, false
			}
		}
	}
	eventCounter++
	ev.ID = eventCounter
	ev.TS = nowUTC()
	events = append([]Event{ev}, events...)
	if len(events) > historySize {
		events = events[:historySize]
	}
	historyAppends++
	if historyAppends > 2*historySize {
		// Compaction rewrites the whole file from the in-memory ring, so it needs
		// a consistent snapshot and must not interleave with a lock-free append.
		// It stays under eventsMu (rare: ~once per 2*historySize events); taking
		// historyWriteMu keeps its atomic rename from clobbering an append that
		// already released eventsMu and is mid-flight.
		historyWriteMu.Lock()
		compactHistoryLocked()
		historyWriteMu.Unlock()
		eventsMu.Unlock()
		broadcast(frameFor(ev))
		return ev, true
	}
	eventsMu.Unlock()

	// M9: append the one new line to disk *outside* eventsMu, so a slow or full
	// disk can never stall readers (eventsSince) or other emitters on the lock.
	// The id and the in-memory ring (the source of truth for live delivery) are
	// already fixed above under eventsMu; only the disk write moves out.
	// historyWriteMu serializes this append against a compaction rewrite. Disk
	// order among simultaneous appends may not match id order, but loadHistory
	// sorts by id on restart, so recovery order stays correct.
	historyWriteMu.Lock()
	appendHistory(ev)
	historyWriteMu.Unlock()

	broadcast(frameFor(ev))
	return ev, true
}

// EmitTyping broadcasts an ephemeral typing indicator for a contact. Transient
// events carry no id and are never stored, so reconnect cursors are unaffected.
func EmitTyping(agent, name string) {
	payload, _ := json.Marshal(map[string]string{"type": "typing", "agent": agent, "name": name})
	broadcast(fmt.Sprintf("data: %s\n\n", payload))
}

// frameFor renders a stored event as an SSE frame with an id: line.
func frameFor(ev Event) string {
	data, _ := json.Marshal(ev)
	return fmt.Sprintf("id: %d\ndata: %s\n\n", ev.ID, data)
}

// eventsSince returns stored events with id greater than since, oldest first.
func eventsSince(since int64) []Event {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	var out []Event
	// events is newest-first; walk backwards to yield oldest-first.
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].ID > since {
			out = append(out, events[i])
		}
	}
	return out
}

// eventByID returns the stored event with the given id (and true), or a zero
// Event and false when it is no longer in the in-memory history — the lookup the
// react endpoint uses to validate a reaction target.
func eventByID(id int64) (Event, bool) {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, ev := range events {
		if ev.ID == id {
			return ev, true
		}
	}
	return Event{}, false
}

// reactionExists reports whether name already reacted with emoji to target — the
// idempotency check that keeps a re-tapped reaction from re-emitting a duplicate
// event or re-delivering the feedback to the agent.
func reactionExists(target int64, name, emoji string) bool {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, ev := range events {
		if ev.Type == "reaction" && ev.Target == target && ev.Name == name && ev.Text == emoji {
			return true
		}
	}
	return false
}

// historyPath is the durable JSONL chat log.
func historyPath() string { return bridgePath("history.jsonl") }

// appendHistory writes ev as one JSON line to the history file. It touches no
// shared in-memory state and runs outside eventsMu; callers hold historyWriteMu
// to serialize it against a compaction rewrite. Best-effort: losing history must
// never break serving.
func appendHistory(ev Event) {
	if ensureBridgeDir() != nil {
		return
	}
	f, err := os.OpenFile(historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(ev)
	_, _ = f.Write(append(data, '\n'))
}

// compactHistoryLocked rewrites the history file from the in-memory ring so it
// cannot grow unbounded across a long-lived daemon. Caller holds eventsMu.
func compactHistoryLocked() {
	var buf bytes.Buffer
	for i := len(events) - 1; i >= 0; i-- { // oldest first on disk
		data, _ := json.Marshal(events[i])
		buf.Write(data)
		buf.WriteByte('\n')
	}
	_ = writeFilePrivate(historyPath(), buf.Bytes())
	historyAppends = 0
}

// loadHistory restores persisted events at startup so threads survive daemon
// restarts. It keeps the newest historySize events, advances the id counter
// past the highest stored id, and compacts an oversized file.
func loadHistory() {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 0 {
		return
	}
	f, err := os.Open(historyPath())
	if err != nil {
		return
	}
	defer f.Close()

	var all []Event // as written; sorted by id below
	corrupt := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil || ev.ID == 0 {
			corrupt = true // an unparseable / id-less line: don't clobber the file
			continue
		}
		all = append(all, ev)
	}
	if sc.Err() != nil {
		corrupt = true // truncated or oversized line: the log didn't read cleanly
	}

	// Disk order isn't guaranteed to match id order (M9 appends outside the lock),
	// so sort by id before trimming so we keep the genuinely newest events.
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	// A compaction rewrite can race a lock-free append, landing the same id on
	// disk twice; drop the adjacent duplicates the sort just grouped so a restart
	// can't resurrect a doubled bubble (#8).
	all = slices.CompactFunc(all, func(a, b Event) bool { return a.ID == b.ID })
	if len(all) > historySize {
		all = all[len(all)-historySize:]
	}
	events = make([]Event, len(all)) // newest first in memory
	for i, ev := range all {
		events[len(all)-1-i] = ev
		if ev.ID > eventCounter {
			eventCounter = ev.ID
		}
	}

	if corrupt {
		// The on-disk log had unparseable content. Preserve it as .corrupt so a
		// later append or compaction can't silently overwrite and lose it, then
		// rewrite a clean history.jsonl from the events we salvaged (M3). Renaming
		// an open file is fine on this platform; the deferred Close still applies.
		_ = os.Rename(historyPath(), historyPath()+".corrupt")
		compactHistoryLocked()
		return
	}
	if fi, err := os.Stat(historyPath()); err == nil && fi.Size() > historyMaxBytes {
		compactHistoryLocked()
	}
}

// startHeartbeat pings every SSE client on an interval so idle connections and
// intervening proxies keep the stream open; dead clients are pruned on write.
// The ping is a NAMED SSE event, not a comment: comments are invisible to
// EventSource JavaScript, so a half-dead socket (Mac slept, NAT dropped the
// path) used to sit in readyState OPEN receiving nothing while the app showed
// a green dot — the silent-blackout class (H12). A named event reaches the
// client's 'hb' listener (proof of life) without firing onmessage on older
// cached clients.
func startHeartbeat() {
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for range t.C {
			broadcast("event: hb\ndata: {}\n\n")
		}
	}()
}

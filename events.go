package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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

// Emit records a durable event, assigns it the next monotonic id, appends it
// to the history file, and broadcasts it to SSE clients. It is the single
// entry point every subsystem uses to speak, and returns the stored event. An
// optional clientID is echoed on "sent" acknowledgements for phone-echo dedup
// (M5); server-originated events pass none.
func Emit(typ, agent, name, text string, clientID ...string) Event {
	eventsMu.Lock()
	eventCounter++
	ev := Event{ID: eventCounter, TS: nowUTC(), Type: typ, Agent: agent, Name: name, Text: text}
	if len(clientID) > 0 {
		ev.ClientID = clientID[0]
	}
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
		return ev
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
	return ev
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
func startHeartbeat() {
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for range t.C {
			broadcast(": hb\n\n")
		}
	}()
}

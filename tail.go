package main

// tail.go — the outbound relay: follow each contact's session JSONL and
// forward new visible assistant text to the phone. Offsets persist across
// daemon restarts (round 4b) and survive offline flaps (H6), so agent words
// written during a gap are relayed, never skipped.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Session manager: reply tailing + liveness (runs inside the daemon)
// ---------------------------------------------------------------------------

type tailState struct {
	path   string
	offset int64
	seen   map[string]int64 // path -> resume offset: a re-adopted file continues where it left off
}

var tails = map[string]*tailState{}

// tailsDirty marks that a tail offset advanced since the last persist. Reconcile
// goroutine only — saveTails runs from the same goroutine, so tails needs no
// lock.
var tailsDirty bool

// loadedTails holds offsets restored from disk at startup, consumed once each
// as pollReplies first sees the matching contact. A restored offset lets the
// tail RESUME where the previous daemon stopped instead of skipping to EOF —
// so output an agent wrote while the daemon was down (a restart, a crash, the
// deploy dance) is relayed rather than silently swallowed (round 4b).
var loadedTails map[string]tailStateDisk

// tailStateDisk is the persisted shape of a tailState.
type tailStateDisk struct {
	Path   string           `json:"path"`
	Offset int64            `json:"offset"`
	Seen   map[string]int64 `json:"seen"`
}

func tailsPath() string { return bridgePath("tails.json") }

// loadTails restores persisted tail offsets. Called once from runServe before
// the reconcile goroutine starts, so it needs no lock.
func loadTails() {
	data, err := os.ReadFile(tailsPath())
	if err != nil {
		return
	}
	var snap struct {
		Contacts map[string]tailStateDisk `json:"contacts"`
	}
	if json.Unmarshal(data, &snap) == nil {
		loadedTails = snap.Contacts
	}
}

// saveTails persists the current tail offsets 0600 (atomic write). Called from
// the reconcile goroutine when an offset advanced, so the next daemon resumes
// instead of skipping. Best-effort: a failed persist just risks re-relaying a
// little output on the next restart, never losing any.
func saveTails() {
	snap := struct {
		Contacts map[string]tailStateDisk `json:"contacts"`
	}{Contacts: make(map[string]tailStateDisk, len(tails))}
	for id, st := range tails {
		snap.Contacts[id] = tailStateDisk{Path: st.path, Offset: st.offset, Seen: st.seen}
	}
	data, _ := json.Marshal(snap)
	_ = writeFilePrivate(tailsPath(), data)
}

// pollReplies relays new visible output for one contact. It re-resolves the
// session JSONL each pass (the path changes on --resume) and, on a switch,
// starts at end-of-file so replayed history is not re-sent to the phone.
func pollReplies(c *Contact) {
	path := sessionFileFor(c)
	if path == "" {
		return
	}
	st := tails[c.ID]
	if st == nil {
		st = &tailState{path: path, seen: map[string]int64{}}
		size := fileSize(path)
		resumed := false
		// A persisted offset for this exact file lets the tail resume where the
		// previous daemon stopped — relaying whatever the agent wrote while the
		// daemon was down instead of skipping it (round 4b). Guard against a
		// rotated/truncated file (offset past EOF): fall back to EOF then.
		if saved, ok := loadedTails[c.ID]; ok {
			delete(loadedTails, c.ID) // consume once, however it resolves
			if saved.Path == path && saved.Offset >= 0 && saved.Offset <= size {
				st.offset = saved.Offset
				if saved.Seen != nil {
					st.seen = saved.Seen
				}
				resumed = true
			}
		}
		if !resumed {
			st.offset = size
		}
		st.seen[path] = st.offset
		tails[c.ID] = st
		if base := sessionIDFromPath(path); base != "" && base != c.SessionID {
			registry.SetSession(c.ID, base)
		}
		if !resumed {
			return // genuinely first sight of this file: skip its backlog
		}
		// resumed: fall through and relay everything since the saved offset
	}
	if st.path != path {
		st.seen[st.path] = st.offset
		off, known := st.seen[path]
		st.path = path
		if known {
			st.offset = off // resume: a path flip must never eat the gap
		} else {
			st.offset = fileSize(path)
			st.seen[path] = st.offset
			if base := sessionIDFromPath(path); base != "" && base != c.SessionID {
				registry.SetSession(c.ID, base)
			}
			return // genuinely new file: skip its backlog once
		}
	}
	size := fileSize(path)
	if size <= st.offset {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, 0); err != nil {
		return
	}
	// Read a fixed [offset,size) window so a line still being appended can't be
	// half-consumed: LimitReader pins the region, letting us distinguish a
	// newline-terminated line (safe to advance past) from a trailing partial one
	// (re-read whole next poll — M12).
	region := size - st.offset
	var consumed int64
	var texts []string
	sc := bufio.NewScanner(io.LimitReader(f, region))
	sc.Buffer(make([]byte, 0, 1024*1024), maxJSONLLine)
	for sc.Scan() {
		line := sc.Bytes()
		end := consumed + int64(len(line))
		if end >= region {
			// The token runs to the end of the region with no newline after it:
			// a partial line still being written. Leave it for the next poll.
			break
		}
		consumed = end + 1 // token + its stripped newline
		if t := assistantText(line); t != "" {
			texts = append(texts, t)
		}
	}
	if err := sc.Err(); err == bufio.ErrTooLong {
		// A single JSONL line exceeded the scanner cap (e.g. a giant tool
		// result). Step past it instead of re-reading it forever (M11); if it
		// isn't newline-terminated yet, wait and retry on the next poll.
		if skip := skipOversizedLine(path, st.offset+consumed); skip > 0 {
			consumed += skip
		}
	}
	if consumed > 0 {
		st.offset += consumed
		tailsDirty = true // persist the advance so a restart resumes here (4b)
	}
	if len(texts) > 0 {
		registry.SetHealth(c.ID, "ok")
		for _, t := range texts {
			Emit("reply", c.ID, c.Name, t)
			dispatchPluginEvent("reply.out", c, map[string]any{"text": t})
		}
	} else if consumed > 0 {
		// File advanced but produced no visible text: the agent is thinking or
		// running tools — i.e. working.
		registry.SetHealth(c.ID, "working")
		EmitTyping(c.ID, c.Name)
	}
}

// assistantText returns the concatenated visible text of a Claude Code JSONL
// line if it is an assistant message, or "" otherwise. Thinking and tool_use
// blocks are deliberately ignored.
func assistantText(line []byte) string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return ""
	}
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
		return ""
	}
	var parts []string
	for _, b := range entry.Message.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// maxJSONLLine bounds how large a single session-JSONL line the tail will
// buffer, so a giant tool result can't blow up memory; a line past it is
// skipped rather than re-read forever (M11).
const maxJSONLLine = 8 * 1024 * 1024

// skipOversizedLine returns the byte length (including the terminating newline)
// of the single line beginning at OFFSET, or 0 if that line is not yet
// newline-terminated. Used to step the tail past a JSONL line too large for the
// scanner buffer instead of wedging on it.
func skipOversizedLine(path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return 0
	}
	// Scan forward in fixed 64 KB chunks, counting bytes to the next newline and
	// retaining nothing. The line is oversized by definition (it already blew the
	// scanner cap), so buffering it whole — as bufio.ReadBytes would — is the very
	// OOM this is meant to avoid (#4).
	buf := make([]byte, 64*1024)
	var n int64
	for {
		read, err := f.Read(buf)
		for i := 0; i < read; i++ {
			n++
			if buf[i] == '\n' {
				return n // bytes up to and including the terminating newline
			}
		}
		if err != nil {
			return 0 // no terminating newline yet: wait for the writer to finish it
		}
	}
}

func sessionIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

package main

// identity.go — which session file IS this contact? The resolution ladder
// (pane launch arg → rollover chain → stored pin → newest-file fallback),
// decided by file CONTENT, never mtimes — every rung here was earned by a
// live failure on 2026-07-06.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Locating Claude Code session files
// ---------------------------------------------------------------------------

var nonProjectChar = regexp.MustCompile(`[^A-Za-z0-9-]`)

// projectDir returns the ~/.claude/projects subdirectory Claude Code uses for
// sessions rooted at DIR (path components joined by hyphens).
func projectDir(dir string) string {
	home, _ := os.UserHomeDir()
	// Claude Code encodes the *resolved* path, so resolve symlinks (e.g. the
	// macOS /tmp -> /private/tmp link) to match where it actually writes.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if resolved, err = filepath.Abs(dir); err != nil {
			resolved = dir
		}
	}
	encoded := nonProjectChar.ReplaceAllString(resolved, "-")
	return filepath.Join(home, ".claude", "projects", encoded)
}

// currentSessionFile returns the most recently modified .jsonl in DIR's
// project directory — the live conversation — or "" if none.
func currentSessionFile(dir string) string {
	entries, err := os.ReadDir(projectDir(dir))
	if err != nil {
		return ""
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newest = filepath.Join(projectDir(dir), e.Name())
		}
	}
	return newest
}

// sessionFileFor returns the JSONL to tail for c. With a SessionID it pins on
// that file — `claude --resume` preserves the id (verified empirically), so two
// agents rehomed from the same directory never tail each other's conversation
// (H2). Insurance against a hypothetical future Claude Code that forks the id on
// resume: if the pinned file is gone or has stopped growing for a long time,
// fall back to the newest .jsonl in the directory that no *other* live contact
// is already tailing (never adopt a sibling's conversation — H2).
func sessionFileFor(c *Contact) string {
	// Identity resolution, in authority order — each rung earned by a failure
	// tonight: (1) the pane's launch id (`claude --resume <id>` — but it goes
	// stale when compaction ROLLS the session id), (2) the rollover CHAIN:
	// message-uuid continuity proves a successor file continues this exact
	// conversation (content, never mtimes — the marvin incident), (3) the
	// stored pin, already chain-healed, (4) newest-file only when the pinned
	// file is MISSING entirely.
	dir := projectDir(c.Directory)
	base := paneSessionID(c)
	if base == "" {
		base = c.SessionID
	}
	if base == "" {
		return currentSessionFile(c.Directory)
	}
	// A prior chain-heal (or manual surgery) stored a verified session while
	// the pane still shows its launch id forever. When they disagree, decide by
	// CONTENT, never mtime — an mtime race here once demoted a healed pin back
	// to its dead mirror file and silenced a thread for an hour (2026-07-06,
	// afternoon): one coincidental write to the mirror made fileNewer prefer
	// the launch id, and SetSession below then overwrote the pin, making the
	// downgrade permanent. Follow the launch id only when its file STRICTLY
	// continues the pin (holds the pin's tail and the pin doesn't hold its own
	// — "never follow anything that could follow you back"); a diverged mirror
	// pair keeps the pin, the last verified truth.
	if c.SessionID != "" && c.SessionID != base {
		base = resolveLaunchVsPin(dir, c, c.SessionID, base)
	}
	path := filepath.Join(dir, base+".jsonl")
	// When the resolved file has gone quiet, check (throttled) whether the
	// session rolled forward under a new id. A chainForce nudge (a hook event
	// arrived for a session id nobody claims — the roll just happened, H8)
	// bypasses both the two-minute quiet requirement and the once-a-minute
	// throttle so the adoption lands on this very pass.
	force := chainForce[c.ID]
	if force {
		delete(chainForce, c.ID)
	}
	info, statErr := os.Stat(path)
	if force || statErr != nil || time.Since(info.ModTime()) > 2*time.Minute {
		if force || time.Since(chainChecked[c.ID]) > time.Minute {
			chainChecked[c.ID] = time.Now()
			if tip := sessionChainTip(dir, base); tip != base {
				audit("session-follow", c.Name+" "+base[:8]+" -> "+tip[:8], "daemon")
				base = tip
				path = filepath.Join(dir, base+".jsonl")
			}
		}
	}
	if base != c.SessionID {
		registry.SetSession(c.ID, base)
	}
	if _, err := os.Stat(path); err != nil {
		if alt := currentSessionFile(c.Directory); alt != "" && alt != path &&
			!registry.SessionHeldByOther(sessionIDFromPath(alt), c.ID) {
			return alt
		}
	}
	return path
}

// chainChecked throttles rollover-chain scans per contact. Reconcile
// goroutine only.
var chainChecked = map[string]time.Time{}

// chainForce marks contacts whose rollover chain must be re-scanned on the
// very next resolve, bypassing sessionFileFor's throttles. Set by
// drainPendingHooks when a hook event names a session id no contact claims
// yet (H8). Reconcile goroutine only.
var chainForce = map[string]bool{}

// lastResolve caches resolveLaunchVsPin verdicts per contact so the content
// scans (transcripts reach hundreds of MB) stay off the 2s poll path.
// Reconcile goroutine only.
var lastResolve = map[string]resolveVerdict{}

type resolveVerdict struct {
	pin, pane, winner string
	at                time.Time
}

// resolveLaunchVsPin decides, by content, whether to tail the stored pin or
// the pane's launch id when they name different sessions. The launch id wins
// only when its file strictly continues the pin — it contains the pin's final
// record uuid while the pin's file does not contain its own (a true roll
// carries history exactly one way; a mirror pair matches both ways or
// neither). Everything else keeps the pin: launch args go stale, surgery and
// chain-heals are deliberate. The verdict is cached and refreshed at most
// every 5 minutes per unchanged (pin, pane) pair; a real roll is a once-ever
// event and the chain-check path handles it within its own throttle anyway.
func resolveLaunchVsPin(dir string, c *Contact, pin, pane string) string {
	if v, ok := lastResolve[c.ID]; ok && v.pin == pin && v.pane == pane &&
		time.Since(v.at) < 5*time.Minute {
		return v.winner
	}
	pinFile := filepath.Join(dir, pin+".jsonl")
	paneFile := filepath.Join(dir, pane+".jsonl")
	winner := pin
	pinTail := lastRecordUUID(pinFile)
	if pinTail == "" {
		winner = pane // pin's file is gone or empty: the launch id is all we have
	} else {
		paneTail := lastRecordUUID(paneFile)
		paneContinuesPin := fileContainsUUID(paneFile, pinTail)
		pinContinuesPane := paneTail != "" && fileContainsUUID(pinFile, paneTail)
		if paneContinuesPin && !pinContinuesPane {
			winner = pane
		}
	}
	if v, ok := lastResolve[c.ID]; !ok || v.winner != winner {
		audit("session-resolve", fmt.Sprintf("%s pin=%.8s pane=%.8s -> %.8s",
			c.Name, pin, pane, winner), "daemon")
	}
	lastResolve[c.ID] = resolveVerdict{pin: pin, pane: pane, winner: winner, at: time.Now()}
	return winner
}

var uuidRecRe = regexp.MustCompile(`"uuid":"([0-9a-f-]{36})"`)

// sessionChainTip follows clear/compaction rollovers from id to the current
// tip. The test is DIRECTIONAL by construction: a true continuation contains
// its predecessor's final record uuid (the roll copies history forward); an
// ancestor can never contain its descendant's future. The first head-uuid
// heuristic matched in BOTH directions on mirror-superset files and made the
// pin oscillate, eating replies — never follow anything that could also
// follow you back.
func sessionChainTip(dir, id string) string {
	for range [5]struct{}{} {
		child := sessionChildOf(dir, id)
		if child == "" {
			return id
		}
		id = child
	}
	return id
}

// sessionChildOf finds the file that continues session id, if any: the one
// containing id's final record uuid.
func sessionChildOf(dir, id string) string {
	tail := lastRecordUUID(filepath.Join(dir, id+".jsonl"))
	if tail == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		cand, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || cand == id {
			continue
		}
		if fileContainsUUID(filepath.Join(dir, e.Name()), tail) {
			return cand
		}
	}
	return ""
}

// lastRecordUUID returns the uuid of the final record in a transcript,
// scanning only the tail (files reach hundreds of MB).
func lastRecordUUID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - 64*1024
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, 0); err != nil {
		return ""
	}
	b, _ := io.ReadAll(f)
	ms := uuidRecRe.FindAllSubmatch(b, -1)
	if len(ms) == 0 {
		return ""
	}
	return string(ms[len(ms)-1][1])
}

// fileContainsUUID streams (transcripts reach hundreds of MB) looking for the
// record that owns uuid.
func fileContainsUUID(path, uuid string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	needle := []byte(`"uuid":"` + uuid + `"`)
	buf := make([]byte, 1<<20)
	keep := 0
	for {
		n, err := f.Read(buf[keep:])
		if n > 0 {
			if bytes.Contains(buf[:keep+n], needle) {
				return true
			}
			k := len(needle) - 1
			if keep+n > k {
				copy(buf, buf[keep+n-k:keep+n])
				keep = k
			} else {
				keep += n
			}
		}
		if err != nil {
			return false
		}
	}
}

// paneSessionID reads the session id straight off the contact's pane: every
// managed window runs `claude --resume <id>` as the pane process, so its
// command line is ground truth for who actually lives there. Returns "" when
// unavailable (dead window, no @-target, non-claude pane).
func paneSessionID(c *Contact) string {
	if !strings.HasPrefix(c.TmuxTarget, "@") {
		return ""
	}
	pid, err := tmux("display-message", "-p", "-t", c.TmuxTarget, "#{pane_pid}")
	if err != nil {
		return ""
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strings.TrimSpace(pid)).Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	for i, a := range f {
		if a == "--resume" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

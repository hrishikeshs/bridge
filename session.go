package main

// session.go — the session adapter: everything that touches a real Claude
// Code session. Inbound via tmux send-keys, outbound via session-JSONL
// tailing (text blocks only — thinking and tool internals never leave the
// machine), permission prompts via the Notification hook. The daemon core
// (serve.go) stays transport-agnostic behind the seams assigned here.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// init wires the daemon's three session seams to real tmux operations. In a
// CLI process (connect/send/hook) these assignments are harmless: the daemon
// never runs there, so the seams are never called.
var sessionCmdImpls map[string]func(*cliCtx) error

func init() {
	deliverToSession = tmuxDeliver
	capturePrompt = tmuxCapturePane
	sendKey = tmuxSendKey
	sessionCmdImpls = map[string]func(*cliCtx) error{
		"connect": runConnect,
		"attach":  runAttach,
		"send":    runSend,
		"hook":    runHook,
		"expose":  runExpose,
	}
}

// ---------------------------------------------------------------------------
// tmux seams (run inside the daemon)
// ---------------------------------------------------------------------------

func tmux(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("tmux", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// tmuxAlive reports whether TARGET names a live window. TARGET is normally a
// window id ("@N"), which is immune to tmux's target grammar — a numeric or
// relative window *name* can never misroute to it (C2). A legacy "bridge:<name>"
// target, written by daemons before the window-id migration, is resolved by
// window name so on-disk contacts keep working until they next reconnect.
func tmuxAlive(target string) bool {
	if target == "" {
		return false
	}
	if strings.HasPrefix(target, "@") {
		// Window ids are unique server-wide; check membership directly.
		out, err := tmux("list-windows", "-a", "-F", "#{window_id}")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == target {
				return true
			}
		}
		return false
	}
	sess, win, found := strings.Cut(target, ":")
	if !found {
		_, err := tmux("has-session", "-t", target)
		return err == nil
	}
	out, err := tmux("list-windows", "-t", sess, "-F", "#{window_name}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if line == win {
			return true
		}
	}
	return false
}

// tmuxWindowID returns the window id ("@N") of the window named NAME in the
// shared "bridge" session, or "" if there is none. Used to capture the routing
// target at creation and to migrate a legacy name-based target on revive.
func tmuxWindowID(name string) string {
	out, err := tmux("list-windows", "-t", "bridge", "-F", "#{window_id} #{window_name}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		id, wname, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && wname == name {
			return id
		}
	}
	return ""
}

// tmuxDeliver types TEXT into the agent's terminal as one literal line and
// presses Enter. TEXT arrives already prefixed and newline-flattened.
func tmuxDeliver(c *Contact, text string) error {
	if !tmuxAlive(c.TmuxTarget) {
		registry.SetOffline(c.ID)
		return fmt.Errorf("session %s is not running", c.Name)
	}
	if _, err := tmux("send-keys", "-t", c.TmuxTarget, "-l", "--", text); err != nil {
		return err
	}
	_, err := tmux("send-keys", "-t", c.TmuxTarget, "Enter")
	return err
}

// tmuxCapturePane returns one snapshot of the agent's visible terminal for the
// attention card body. Empty string on failure — the daemon falls back to the
// hook message text.
func tmuxCapturePane(c *Contact) string {
	out, err := tmux("capture-pane", "-p", "-t", c.TmuxTarget)
	if err != nil {
		return ""
	}
	return out
}

// tmuxSendKey delivers a whitelisted approval key. esc sends Escape and takes
// no Enter; every other key is typed and confirmed.
func tmuxSendKey(c *Contact, key string) error {
	if !tmuxAlive(c.TmuxTarget) {
		return fmt.Errorf("session %s is not running", c.Name)
	}
	if key == "esc" {
		_, err := tmux("send-keys", "-t", c.TmuxTarget, "Escape")
		return err
	}
	if _, err := tmux("send-keys", "-t", c.TmuxTarget, "-l", "--", key); err != nil {
		return err
	}
	_, err := tmux("send-keys", "-t", c.TmuxTarget, "Enter")
	return err
}

// ---------------------------------------------------------------------------
// Session manager: reply tailing + liveness (runs inside the daemon)
// ---------------------------------------------------------------------------

type tailState struct {
	path   string
	offset int64
}

var tails = map[string]*tailState{}

// startSessionManager launches the reconcile loop that keeps a JSONL tail
// running for every live contact and retires contacts whose tmux session has
// ended. Called once from runServe.
func startSessionManager() {
	go func() {
		for {
			for _, c := range registry.Roster() {
				alive := c.TmuxTarget != "" && tmuxAlive(c.TmuxTarget)
				switch {
				case c.Status == "live" && !alive:
					// Its tmux window ended: retire it.
					registry.SetOffline(c.ID)
					Emit("attention-clear", c.ID, c.Name, "")
					delete(tails, c.ID)
				case c.Status != "live" && alive:
					// Its window outlived a daemon restart: revive it so a
					// restart never orphans a running agent. Migrate a legacy
					// name-based target to the grammar-immune window id here.
					target := c.TmuxTarget
					if !strings.HasPrefix(target, "@") {
						if id := tmuxWindowID(c.Name); id != "" {
							target = id
						}
					}
					revived := registry.Connect(c.Name, c.Directory, c.SessionID, target)
					Emit("connected", revived.ID, revived.Name, "")
					flushMailbox(revived)
				case c.Status == "live" && alive:
					pollReplies(c)
					verifyPrompt(c)
				}
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

// promptStrikes counts consecutive reconcile ticks where a hook-attested open
// prompt was NOT visible on the contact's screen. Only the reconcile goroutine
// touches it.
var promptStrikes = map[string]int{}

// verifyPrompt re-checks an open permission prompt against the actual pane. A
// prompt answered at the desk leaves no hook event behind, so without this the
// "needs your approval" state goes stale and false urgency spreads across the
// phone (found live: a night of desk-answered prompts left banners on every
// thread). Two consecutive misses (~4s) clear it — one miss could just be the
// dialog still painting.
func verifyPrompt(c *Contact) {
	if !c.PromptOpen {
		delete(promptStrikes, c.ID)
		return
	}
	if looksLikePrompt(capturePrompt(c)) {
		delete(promptStrikes, c.ID)
		return
	}
	promptStrikes[c.ID]++
	if promptStrikes[c.ID] >= 2 {
		delete(promptStrikes, c.ID)
		registry.SetPrompt(c.ID, false)
		Emit("attention-clear", c.ID, c.Name, "")
	}
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
	if st == nil || st.path != path {
		size := fileSize(path)
		st = &tailState{path: path, offset: size}
		tails[c.ID] = st
		if base := sessionIDFromPath(path); base != "" && base != c.SessionID {
			registry.SetSession(c.ID, base)
		}
		return // first sight of this file: skip its backlog
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
	st.offset += consumed
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

// maxJSONLLine bounds how large a single session-JSONL line the tail will
// buffer, so a giant tool result can't blow up memory; a line past it is
// skipped rather than re-read forever (M11).
const maxJSONLLine = 8 * 1024 * 1024

// sessionFileFor returns the JSONL to tail for c. With a SessionID it pins on
// that file — `claude --resume` preserves the id (verified empirically), so two
// agents rehomed from the same directory never tail each other's conversation
// (H2). Insurance against a hypothetical future Claude Code that forks the id on
// resume: if the pinned file is gone or has stopped growing for a long time,
// fall back to the newest .jsonl in the directory that no *other* live contact
// is already tailing (never adopt a sibling's conversation — H2).
func sessionFileFor(c *Contact) string {
	// THE PANE IS THE AGENT. A managed window runs `claude --resume <id>` as
	// its pane process, so the command line outranks both the registry pin and
	// any newest-file guess. This rule exists because a staleness heuristic
	// once adopted a sibling's shadow transcript in a shared directory and
	// relayed one agent's words under another's name (the marvin incident,
	// 2026-07-06) — idle is normal; identity is not inferred from mtimes.
	if pane := paneSessionID(c); pane != "" {
		if pane != c.SessionID {
			registry.SetSession(c.ID, pane) // heal the pin to ground truth
			audit("session-heal", c.Name+" -> "+pane[:8], "daemon")
		}
		return filepath.Join(projectDir(c.Directory), pane+".jsonl")
	}
	if c.SessionID == "" {
		return currentSessionFile(c.Directory)
	}
	pinned := filepath.Join(projectDir(c.Directory), c.SessionID+".jsonl")
	// No pane to consult (window mid-respawn, foreign host). Only a MISSING
	// pinned file justifies the newest-file fallback; a quiet file is just an
	// idle agent.
	if _, err := os.Stat(pinned); err == nil {
		return pinned
	}
	if alt := currentSessionFile(c.Directory); alt != "" && alt != pinned &&
		!registry.SessionHeldByOther(sessionIDFromPath(alt), c.ID) {
		return alt
	}
	return pinned
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

// ---------------------------------------------------------------------------
// CLI verbs (run in the agent's own process, talk to the daemon)
// ---------------------------------------------------------------------------

// cliCtx carries parsed flags/args to a session CLI verb.
type cliCtx struct {
	args []string
	name string // --name (connect)
	to   string // --to (send)
}

// nameConnectRe validates a user-supplied --name: it must start with a letter
// and then contain only letters, digits, '-' and '_' (max 31 chars). This
// rejects all-digit, relative ('+'/'-'), whitespace and special names that tmux
// would otherwise misresolve via its target grammar (C2).
var nameConnectRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,30}$`)

// runConnect rehomes the calling agent: it finds this session's conversation,
// creates a daemon-managed tmux window, registers the contact to settle its
// final address and immutable id, then launches `claude --resume` in that window
// with the id baked into its environment. --name is optional; when omitted an
// adjective-animal address is generated. All agents share one "bridge" tmux
// session so `bridge attach` groups them as tabs.
func runConnect(ctx *cliCtx) error {
	name := ctx.name
	if name != "" && !nameConnectRe.MatchString(name) {
		return fmt.Errorf("invalid --name %q: start with a letter, then letters/digits/-/_ (max 31 chars); numeric, relative (+/-), whitespace and special names are rejected", name)
	}
	if err := ensureDaemon(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// The calling agent identifies ITSELF: Claude Code exports the session id
	// into every shell it runs. This makes connect deterministic in shared
	// project directories — several agents can live in one directory and each
	// rehomes its own conversation, never a sibling's.
	//
	// When the env id is set but its file is NOT under cwd's project dir, the
	// agent is simply standing in the wrong directory — ERROR AND SAY SO.
	// Never fall back to newest-file here: that silently registers whichever
	// stranger's session happens to live in cwd under the caller's name
	// (learned live — an agent cd'd into the repo to read the docs and spent
	// four connect attempts accidentally becoming a 4KB scratch session).
	// The newest-file heuristic survives only for CC versions without the var.
	sessionID := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if sessionID != "" {
		if _, err := os.Stat(filepath.Join(projectDir(cwd), sessionID+".jsonl")); err != nil {
			return fmt.Errorf("your session (%s) does not live under %s — cd to the directory you normally work in (your project dir) and run connect again", sessionID[:8], cwd)
		}
	} else {
		sessionFile := currentSessionFile(cwd)
		if sessionFile == "" {
			return fmt.Errorf("no Claude Code session found for %s — run this from inside a session", cwd)
		}
		sessionID = sessionIDFromPath(sessionFile)
	}

	// One roster read drives both auto-naming and reconnect reuse.
	contacts := liveContacts()
	taken := map[string]bool{}
	for _, c := range contacts {
		if c.Status == "live" && c.Name != "" {
			taken[c.Name] = true
		}
	}
	if name == "" {
		name = generateName(taken)
	}

	// Reconnect reuse: reuse a window only if THIS identity (name+directory)
	// already owns a live one, keyed by its stored window id — never by matching a
	// name, which could belong to a different live agent and hijack its pane (#2).
	reuse := ""
	for _, c := range contacts {
		if c.Status == "live" && c.Name == name && c.Directory == cwd &&
			strings.HasPrefix(c.TmuxTarget, "@") {
			reuse = c.TmuxTarget
			break
		}
	}
	// A fresh connect settles its final (possibly suffixed) address BEFORE the
	// window is born, so the window is created under its final name and is never
	// renamed out from under another agent later (#2).
	if reuse == "" {
		name = uniqueName(name, taken)
	}

	target, created, err := ensureWindow(name, cwd, reuse)
	if err != nil {
		return err
	}

	var reg struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := daemonRequest("POST", "/local/connect", map[string]string{
		"name": name, "directory": cwd,
		"session_id": sessionID, "tmux_target": target,
	}, &reg); err != nil {
		return err
	}
	// The daemon is the final authority on uniqueness; if a connect raced ours and
	// took the name in between, it appends one more suffix. Rename our OWN freshly
	// created window to match — safe now that ensureWindow never adopts another
	// agent's window (#2). A reused window belongs to this same contact, so the
	// daemon returns its existing name and this branch does not fire.
	if created && reg.Name != "" && reg.Name != name {
		_, _ = tmux("rename-window", "-t", target, reg.Name)
		name = reg.Name
	}

	// Launch claude with the immutable contact id in its environment: `bridge
	// send` self-identifies by this id, so a suffixed display name can never make
	// it post as — or be resolved to — another agent (#6). The id is known only
	// after registration, so the window was created empty and claude is started
	// now. A reused window already has claude running with this same id baked in.
	if created {
		if err := launchClaude(target, cwd, sessionID, reg.ID); err != nil {
			return err
		}
	}

	if err := installHook(); err != nil {
		fmt.Printf("(note: could not install the permission hook: %v)\n", err)
	}
	fmt.Printf(`I've moved into a managed tmux session — same memory, now running
headless. I'm not gone; I just don't live in a terminal window anymore.

Reach me however's closest — and both channels work at the same time:

  • bridge attach %-8s talk to me in a terminal
      (Ctrl-b d to detach and leave me running; I keep going headless)
  • bridge pair          text me from your phone

Type at your desk, text from the couch — same me, same conversation.

This window is now a retired copy — quit it whenever; I'm no longer in it.
`, name)
	return nil
}

// ensureWindow returns the tmux window id ("@N") to host the agent and whether it
// created a fresh one. On reconnect — reuse is this contact's own still-live
// window id — it returns that window with created=false (claude is already
// running there). Otherwise it creates a fresh window running only a shell and
// returns created=true; claude is launched later by launchClaude, once
// registration has minted the immutable id to bake into its environment (#6). A
// new connect never adopts an existing window by name: that window could be a
// different live agent's, and typing into it would misroute exactly like C2 (#2).
// All agents share one "bridge" session so `bridge attach` groups them as tabs.
func ensureWindow(name, cwd, reuse string) (string, bool, error) {
	if reuse != "" && tmuxAlive(reuse) {
		return reuse, false, nil
	}
	var out string
	var err error
	if _, e := tmux("has-session", "-t", "bridge"); e != nil {
		out, err = tmux("new-session", "-d", "-s", "bridge", "-n", name, "-c", cwd,
			"-P", "-F", "#{window_id}")
	} else {
		out, err = tmux("new-window", "-t", "bridge", "-n", name, "-c", cwd,
			"-P", "-F", "#{window_id}")
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to rehome into tmux (is tmux installed?): %w", err)
	}
	return strings.TrimSpace(out), true, nil
}

// launchClaude starts `claude --resume` in the (already created, still empty)
// window, replacing its shell, with BRIDGE_CONTACT set to the immutable contact
// id. respawn-pane -k delivers the environment straight to the new process, so
// there is no shell-timing race and the id is present the moment claude — and
// thus any `bridge send` it runs — starts (#6).
func launchClaude(target, cwd, sessionID, contactID string) error {
	if _, err := tmux("respawn-pane", "-k", "-t", target, "-c", cwd,
		"-e", "BRIDGE_CONTACT="+contactID, "claude", "--resume", sessionID); err != nil {
		return fmt.Errorf("failed to start claude in the managed window: %w", err)
	}
	return nil
}

// uniqueName returns name with the smallest numeric suffix absent from taken
// (name, name-2, name-3, ...). It mirrors the daemon's own suffixing so a fresh
// window is usually born with its final address; the daemon still has the last
// word if a concurrent connect races it.
func uniqueName(name string, taken map[string]bool) string {
	final := name
	for n := 2; taken[final]; n++ {
		final = fmt.Sprintf("%s-%d", name, n)
	}
	return final
}

// liveContact is the subset of a roster entry the connect CLI needs: enough to
// avoid name collisions and to recognize this identity's own window on reconnect.
type liveContact struct {
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	TmuxTarget string `json:"tmux_target"`
	Status     string `json:"status"`
}

// liveContacts fetches the daemon roster. The daemon still enforces true
// uniqueness among live contacts; the CLI uses this only for a best-effort first
// pass at naming and for reconnect reuse.
func liveContacts() []liveContact {
	var resp struct {
		Contacts []liveContact `json:"contacts"`
	}
	if err := daemonRequest("GET", "/local/contacts", nil, &resp); err != nil {
		return nil
	}
	return resp.Contacts
}

// runAttach hands the terminal to the grouped "bridge" tmux session — all
// agents as windows (tabs). With a name, it selects that agent's window first.
func runAttach(ctx *cliCtx) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	if len(ctx.args) >= 1 {
		_, _ = tmux("select-window", "-t", "bridge:"+ctx.args[0])
	}
	return syscall.Exec(bin, []string{"tmux", "attach", "-t", "bridge"}, os.Environ())
}

// runSend delivers a message from this agent — to the phone by default, or to
// another agent with --to. The sender is taken from BRIDGE_CONTACT.
func runSend(ctx *cliCtx) error {
	if len(ctx.args) < 1 {
		return fmt.Errorf("bridge send <text> [--to <name>]")
	}
	from := os.Getenv("BRIDGE_CONTACT")
	if from == "" {
		return fmt.Errorf("bridge send must run inside a bridge-managed session")
	}
	body := map[string]string{"contact": from, "text": strings.Join(ctx.args, " ")}
	if ctx.to != "" {
		body["to"] = ctx.to
	}
	return daemonRequest("POST", "/local/send", body, nil)
}

// runHook is the Claude Code Notification-hook shim: it reads the hook JSON on
// stdin and forwards the essentials to the daemon.
func runHook(ctx *cliCtx) error {
	var payload struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		Title     string `json:"title"`
		HookEvent string `json:"hook_event_name"`
	}
	if json.NewDecoder(os.Stdin).Decode(&payload) != nil {
		return nil // never break the session on a malformed hook
	}
	kind := "notification"
	if strings.Contains(strings.ToLower(payload.Message+payload.Title), "idle") {
		kind = "idle_prompt"
	}
	_ = daemonRequest("POST", "/local/event", map[string]string{
		"session_id": payload.SessionID,
		"message":    payload.Message,
		"kind":       kind,
	}, nil)
	return nil
}

// runExpose publishes the daemon to the tailnet via `tailscale serve`.
func runExpose(ctx *cliCtx) error {
	port := daemonPort()
	cli, err := tailscaleCLI()
	if err != nil {
		return err
	}
	out, err := exec.Command(cli, "serve", "--bg", fmt.Sprint(port)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale serve failed: %s", strings.TrimSpace(string(out)))
	}
	fmt.Printf("bridge is live on your tailnet. Open the URL above on your phone, then run: bridge pair\n")
	return nil
}

// ---------------------------------------------------------------------------
// helpers for the CLI verbs
// ---------------------------------------------------------------------------

// ensureDaemon starts `bridge serve` detached if no daemon is running.
func ensureDaemon() error {
	if _, err := readLockfile(); err == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 40; i++ {
		if _, err := readLockfile(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not come up")
}

func daemonPort() int {
	lf, err := readLockfile()
	if err != nil {
		return 8378
	}
	return lf.Port
}

// installHook adds the bridge Notification hook to ~/.claude/settings.json if
// absent. Hooks hot-reload, so a running session picks it up on its next fire.
func installHook() error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude", "settings.json")
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			// Never overwrite settings we can't parse: a single stray comma
			// would otherwise cost the user their permissions, env, model and
			// any other hooks (M1). Abort, leaving the file untouched.
			return fmt.Errorf("refusing to edit unparseable %s: %w", path, err)
		}
	}
	exe, _ := os.Executable()
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	if _, exists := hooks["Notification"]; exists {
		return nil // respect an existing configuration; don't stomp it
	}
	hooks["Notification"] = []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": exe + " hook"}},
	}}
	settings["hooks"] = hooks
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func tailscaleCLI() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	app := "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if _, err := os.Stat(app); err == nil {
		return app, nil
	}
	return "", fmt.Errorf("tailscale CLI not found — install from https://tailscale.com/download")
}

// looksLikePrompt reports whether a captured pane is showing a Claude Code
// permission dialog right now — the gate that keeps routine idle/waiting
// notifications from raising a false attention card.
func looksLikePrompt(pane string) bool {
	if pane == "" {
		return false
	}
	low := strings.ToLower(pane)
	// The permission dialog always offers numbered choices and an Esc-cancel
	// line; requiring both avoids matching ordinary numbered output.
	hasChoices := strings.Contains(pane, "1.") && strings.Contains(pane, "2.")
	hasProceed := strings.Contains(low, "do you want") ||
		strings.Contains(low, "esc to cancel") ||
		strings.Contains(low, "no, and tell") ||
		strings.Contains(low, "yes, and")
	return hasChoices && hasProceed
}

// timeNowUnix returns the current unix time in seconds.
func timeNowUnix() int64 { return time.Now().Unix() }

// firstPromptLine pulls the most informative line out of a permission-dialog
// capture for a push body: the tool call (Bash(...)), the "don't ask again
// for: X" command family, or the command line just above the approval ask.
func firstPromptLine(pane string) string {
	lines := strings.Split(pane, "\n")
	// 1. a tool invocation like Bash(git push) / Write(file.go)
	for _, ln := range lines {
		t := strings.TrimSpace(strings.TrimLeft(ln, "⏺ "))
		if toolCallRe.MatchString(t) {
			return t
		}
	}
	// 2. the "don't ask again for: <cmd>" hint names the command family
	for _, ln := range lines {
		if i := strings.Index(strings.ToLower(ln), "don't ask again for:"); i >= 0 {
			if cmd := strings.TrimSpace(ln[i+len("don't ask again for:"):]); cmd != "" {
				return strings.TrimSuffix(cmd, " *")
			}
		}
	}
	// 3. the first meaningful line above the approval ask is the command
	for i, ln := range lines {
		low := strings.ToLower(ln)
		if strings.Contains(low, "requires approval") || strings.Contains(low, "do you want to proceed") {
			for j := i - 1; j >= 0; j-- {
				if t := strings.TrimSpace(lines[j]); t != "" {
					return t
				}
			}
		}
	}
	return "wants your approval — tap to review"
}

var toolCallRe = regexp.MustCompile(`^[A-Z][A-Za-z]*\(.+\)`)

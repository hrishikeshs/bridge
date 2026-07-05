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

// tmuxAlive reports whether the named tmux session exists.
func tmuxAlive(target string) bool {
	_, err := tmux("has-session", "-t", target)
	return err == nil
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
				if c.Status != "live" {
					continue
				}
				if !tmuxAlive(c.TmuxTarget) {
					registry.SetOffline(c.ID)
					Emit("attention-clear", c.ID, c.Name, "")
					delete(tails, c.ID)
					continue
				}
				pollReplies(c)
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

// pollReplies relays new visible output for one contact. It re-resolves the
// session JSONL each pass (the path changes on --resume) and, on a switch,
// starts at end-of-file so replayed history is not re-sent to the phone.
func pollReplies(c *Contact) {
	path := currentSessionFile(c.Directory)
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
	var consumed int64
	var texts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		consumed += int64(len(line)) + 1 // +1 for the newline
		if t := assistantText(line); t != "" {
			texts = append(texts, t)
		}
	}
	st.offset += consumed
	if len(texts) > 0 {
		registry.SetHealth(c.ID, "ok")
		for _, t := range texts {
			Emit("reply", c.ID, c.Name, t)
		}
	} else {
		// File grew but produced no visible text: the agent is thinking or
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
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	encoded := nonProjectChar.ReplaceAllString(abs, "-")
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

// runConnect rehomes the calling agent: it finds this session's conversation,
// spawns a daemon-managed tmux running `claude --resume` on it, registers the
// contact, installs the Notification hook, and prints the handoff.
func runConnect(ctx *cliCtx) error {
	name := ctx.name
	if name == "" {
		return fmt.Errorf("bridge connect needs --name <address>")
	}
	if err := ensureDaemon(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	sessionFile := currentSessionFile(cwd)
	if sessionFile == "" {
		return fmt.Errorf("no Claude Code session found for %s — run this from inside a session", cwd)
	}
	sessionID := sessionIDFromPath(sessionFile)
	target := "bridge-" + name

	if !tmuxAlive(target) {
		if _, err := tmux("new-session", "-d", "-s", target, "-c", cwd,
			"-e", "BRIDGE_CONTACT="+name,
			"claude", "--resume", sessionID); err != nil {
			return fmt.Errorf("failed to rehome into tmux (is tmux installed?): %w", err)
		}
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
	if err := installHook(); err != nil {
		fmt.Printf("(note: could not install the permission hook: %v)\n", err)
	}
	fmt.Printf(`I've moved into a managed terminal — same memory, now reachable from your phone.

  • This session (the one you're reading) is now a retired copy. Quit it.
  • bridge attach %s    keep a terminal on me
  • bridge pair         put me on your phone

See you on the other side.
`, name)
	return nil
}

// runAttach hands the terminal to a managed session (exec tmux attach).
func runAttach(ctx *cliCtx) error {
	if len(ctx.args) < 1 {
		return fmt.Errorf("bridge attach <name>")
	}
	target := "bridge-" + ctx.args[0]
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	return syscall.Exec(bin, []string{"tmux", "attach", "-t", target}, os.Environ())
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
		_ = json.Unmarshal(data, &settings)
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

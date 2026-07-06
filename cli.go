package main

// cli.go — the session CLI verbs (connect/attach/send/hook/expose), which
// run in the agent's own process and talk to the daemon over the lockfile-
// token local API. Also the daemon bootstrap and hook installation they use.

import (
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

// sessionCmdImpls maps CLI verb names to their implementations; main.go's
// cobra stubs dispatch through it.
var sessionCmdImpls map[string]func(*cliCtx) error

func init() {
	sessionCmdImpls = map[string]func(*cliCtx) error{
		"connect": runConnect,
		"attach":  runAttach,
		"send":    runSend,
		"status":  runStatus,
		"hook":    runHook,
		"expose":  runExpose,
	}
}

// cliCtx carries parsed flags/args to a session CLI verb.
type cliCtx struct {
	args  []string
	name  string // --name (connect)
	to    string // --to (send)
	clear bool   // --clear (status)
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
	var resp struct {
		Queued   bool   `json:"queued"`
		UserAway string `json:"user_away"`
	}
	if err := daemonRequest("POST", "/local/send", body, &resp); err != nil {
		return err
	}
	if resp.Queued {
		fmt.Printf("queued — %s is offline right now; the daemon delivers it when they're back\n", ctx.to)
	}
	// AIM auto-responder: a send to the phone (no --to) may come back carrying
	// the human's away line. Surface it as a single line the moment this agent
	// reached out, so it reads like a status message in its own transcript.
	// stripControl scrubs it (delivery.go): my-status is printed straight into
	// a terminal, so control bytes must never survive to drive the TUI.
	if resp.UserAway != "" {
		fmt.Printf("away message from Hrishi: %s\n", strings.TrimSpace(stripControl(resp.UserAway)))
	}
	return nil
}

// runStatus sets, clears, or prints the calling agent's away/status line — the
// AIM status the phone shows beside its name. The agent identifies ITSELF via
// BRIDGE_CONTACT, exactly like runSend (an arbitrary contact string would be an
// identity-forgery vector the daemon rejects, H9). `bridge status <text>` sets
// it, `--clear` (or an empty text) clears it, and a bare `bridge status` prints
// the current one.
func runStatus(ctx *cliCtx) error {
	from := os.Getenv("BRIDGE_CONTACT")
	if from == "" {
		return fmt.Errorf("bridge status must run inside a bridge-managed session")
	}
	// Bare `bridge status`: report the current line rather than clobbering it.
	if !ctx.clear && len(ctx.args) == 0 {
		if away := currentAway(from); away != "" {
			fmt.Printf("status: %s\n", away)
		} else {
			fmt.Println("no status set — `bridge status <text>` to set one")
		}
		return nil
	}
	text := ""
	if !ctx.clear {
		text = strings.Join(ctx.args, " ")
	}
	if err := daemonRequest("POST", "/local/status", map[string]string{"contact": from, "text": text}, nil); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		fmt.Println("status cleared")
	} else {
		fmt.Printf("status set: %s\n", text)
	}
	return nil
}

// currentAway fetches this agent's away line from the daemon roster, matching on
// the immutable contact id (BRIDGE_CONTACT). Empty when unset, unknown, or the
// daemon is unreachable — a bare read never errors out the caller.
func currentAway(id string) string {
	var resp struct {
		Contacts []struct {
			ID   string `json:"id"`
			Away string `json:"away"`
		} `json:"contacts"`
	}
	if err := daemonRequest("GET", "/local/contacts", nil, &resp); err != nil {
		return ""
	}
	for _, c := range resp.Contacts {
		if c.ID == id {
			return c.Away
		}
	}
	return ""
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

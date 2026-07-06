package main

// tmux.go — every touch of a real tmux server: exec plumbing, liveness,
// window lookup, and the three delivery primitives (type a line, capture the
// pane, press a key). Nothing above this layer runs tmux directly.

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// init wires the daemon's three session seams to the real tmux operations.
// In a CLI process (connect/send/hook) these assignments are harmless: the
// daemon never runs there, so the seams are never called.
func init() {
	deliverToSession = tmuxDeliver
	capturePrompt = tmuxCapturePane
	sendKey = tmuxSendKey
}

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
		// Don't mark offline here: one failed tmux exec reads exactly like a
		// dead window, and a single-strike flap used to reset the reply tail
		// to EOF (H6). The reconcile loop owns liveness — it retires a contact
		// only after consecutive dead ticks; this delivery just fails safely
		// and the mail stays queued.
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

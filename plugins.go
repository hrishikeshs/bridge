package main

// Plugin runtime — docs/plugins.md is the contract; this file is the engine.
//
// A plugin is ONE self-describing executable in ~/.bridge/plugins/:
// `<exe> manifest` declares the events it wants, `<exe> event` receives an
// envelope on stdin and prints action JSON on stdout. Plugins run with the
// daemon's uid — the same trust boundary as the daemon itself — so the
// runtime's job is not sandboxing (it can't, honestly) but LOUDNESS: every
// install, run, action, refusal, and failure is audited. Events are
// best-effort signals, never a durable queue.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pluginExecTimeout = 10 * time.Second
	pluginStdoutCap   = 64 * 1024
	pluginStderrCap   = 1024
	pluginQueueCap    = 32
	pluginActionCap   = 8
	pluginValueCap    = 200 // set-field value length
	pluginTickEvery   = 60 * time.Second
)

var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// pluginKnownEvents is the v1 vocabulary. A manifest naming anything else is
// audited and that name ignored (forward compatibility with future events).
var pluginKnownEvents = map[string]bool{
	"message.in": true, "reply.out": true, "permission.prompt": true,
	"agent.connect": true, "agent.idle": true, "tick": true,
}

type pluginContact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Status    string `json:"status"`
	Health    string `json:"health"`
}

type pluginEnvelope struct {
	Event   string         `json:"event"`
	TS      string         `json:"ts"`
	Contact *pluginContact `json:"contact,omitempty"`
	Data    map[string]any `json:"data"`
}

type plugin struct {
	name   string
	path   string
	events map[string]bool
	queue  chan pluginEnvelope
	done   chan struct{} // closed to retire the worker when the plugin vanishes
}

var (
	pluginMu       sync.Mutex
	pluginSet      = map[string]*plugin{} // loaded plugins by name
	pluginDirStamp time.Time              // plugins dir mtime at last scan
	pluginSeen     = map[string]bool{}    // ever-announced names, persisted
	pluginOff      atomic.Bool            // lockdown kill switch
)

func pluginsDir() string     { return filepath.Join(bridgeDir(), "plugins") }
func pluginSeenPath() string { return filepath.Join(bridgeDir(), "plugins-seen.json") }

// initPlugins loads the announced-set, does the first scan, and starts the
// tick heartbeat. Called once from runServe.
func initPlugins() {
	if data, err := os.ReadFile(pluginSeenPath()); err == nil {
		var names []string
		if json.Unmarshal(data, &names) == nil {
			for _, n := range names {
				pluginSeen[n] = true
			}
		}
	}
	pluginMu.Lock()
	rescanPlugins()
	pluginMu.Unlock()
	go pluginTickLoop()
}

func savePluginSeen() { // caller holds pluginMu
	names := make([]string, 0, len(pluginSeen))
	for n := range pluginSeen {
		names = append(names, n)
	}
	data, _ := json.Marshal(names)
	_ = os.WriteFile(pluginSeenPath(), data, 0o600)
}

// maybeRescanPlugins reloads the plugin set when the directory mtime moved —
// lazily, at most one stat per dispatch, so installs are picked up without a
// daemon restart and the hot path stays cheap.
func maybeRescanPlugins() {
	info, err := os.Stat(pluginsDir())
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if err != nil {
		if len(pluginSet) > 0 { // directory removed: retire everything
			pluginDirStamp = time.Time{}
			rescanPlugins()
		}
		return
	}
	if !info.ModTime().Equal(pluginDirStamp) {
		rescanPlugins()
	}
}

// rescanPlugins rebuilds pluginSet from disk. Caller holds pluginMu.
func rescanPlugins() {
	next := map[string]*plugin{}
	defer func() {
		// Retire workers for plugins that vanished; adopt the new set.
		for name, p := range pluginSet {
			if next[name] == nil || next[name] != p {
				close(p.done)
			}
		}
		pluginSet = next
	}()

	dir := pluginsDir()
	info, err := os.Stat(dir)
	if err != nil {
		return // no plugins dir = no plugins; not an error
	}
	pluginDirStamp = info.ModTime()
	if info.Mode().Perm()&0o077 != 0 {
		audit("plugin-dir-refused", fmt.Sprintf("%s is %o, need 0700", dir, info.Mode().Perm()), "plugin")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // *.d state dirs and anything else
		}
		fi, err := e.Info()
		if err != nil || fi.Mode().Perm()&0o100 == 0 {
			continue // not executable: not a plugin
		}
		path := filepath.Join(dir, e.Name())
		if fi.Mode().Perm()&0o022 != 0 {
			audit("plugin-refused", path+" is group/world-writable", "plugin")
			continue
		}
		m, err := pluginManifest(path)
		if err != nil {
			audit("plugin-refused", path+": "+err.Error(), "plugin")
			continue
		}
		if !pluginNameRe.MatchString(m.Name) {
			audit("plugin-refused", path+": bad name "+m.Name, "plugin")
			continue
		}
		if next[m.Name] != nil {
			audit("plugin-refused", path+": duplicate name "+m.Name, "plugin")
			continue
		}
		events := map[string]bool{}
		for _, ev := range m.Events {
			if pluginKnownEvents[ev] {
				events[ev] = true
			} else {
				audit("plugin-unknown-event", m.Name+" wants "+ev, "plugin")
			}
		}
		// Keep the running instance (queue, worker, in-flight work) when the
		// same plugin is still there; otherwise start fresh.
		if prev := pluginSet[m.Name]; prev != nil && prev.path == path {
			prev.events = events
			next[m.Name] = prev
			continue
		}
		p := &plugin{name: m.Name, path: path, events: events,
			queue: make(chan pluginEnvelope, pluginQueueCap), done: make(chan struct{})}
		next[m.Name] = p
		go p.worker()
		if !pluginSeen[m.Name] {
			pluginSeen[m.Name] = true
			savePluginSeen()
			evs := make([]string, 0, len(events))
			for ev := range events {
				evs = append(evs, ev)
			}
			detail := m.Name + " installed — listens to: " + strings.Join(evs, ", ")
			audit("plugin-installed", detail, "plugin")
			Emit("plugin", "", m.Name, detail) // recorded in history; installs are loud
		}
	}
}

type manifest struct {
	Name   string   `json:"name"`
	Events []string `json:"events"`
}

// pluginManifest asks the executable to describe itself.
func pluginManifest(path string) (manifest, error) {
	var m manifest
	ctx, cancel := context.WithTimeout(context.Background(), pluginExecTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "manifest").Output()
	if err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	if len(out) > pluginStdoutCap {
		out = out[:pluginStdoutCap]
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &m); err != nil {
		return m, fmt.Errorf("manifest parse: %w", err)
	}
	return m, nil
}

// dispatchPluginEvent fans an event out to every plugin that asked for it.
// Contact may be nil (tick). Non-blocking: a full queue drops the oldest
// waiting event (with an audit line) — a slow plugin delays only itself.
func dispatchPluginEvent(event string, c *Contact, data map[string]any) {
	if pluginOff.Load() {
		return
	}
	maybeRescanPlugins()
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if len(pluginSet) == 0 {
		return
	}
	env := pluginEnvelope{Event: event, TS: nowUTC(), Data: data}
	if c != nil {
		env.Contact = &pluginContact{ID: c.ID, Name: c.Name, Directory: c.Directory,
			Status: c.Status, Health: c.Health}
	}
	if env.Data == nil {
		env.Data = map[string]any{}
	}
	for _, p := range pluginSet {
		if !p.events[event] {
			continue
		}
		select {
		case p.queue <- env:
		default:
			select { // full: shed the oldest, then this one fits
			case <-p.queue:
				audit("plugin-queue-full", p.name+" dropped oldest event", "plugin")
			default:
			}
			select {
			case p.queue <- env:
			default:
			}
		}
	}
}

// worker serializes a plugin's invocations: one process at a time, ever.
func (p *plugin) worker() {
	for {
		select {
		case <-p.done:
			return
		case env := <-p.queue:
			p.run(env)
		}
	}
}

// boundedBuf keeps the first cap bytes and silently drops the rest.
type boundedBuf struct {
	cap int
	buf bytes.Buffer
}

func (b *boundedBuf) Write(p []byte) (int, error) {
	if room := b.cap - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil // report full write so the child never sees EPIPE
}

// run invokes the plugin for one event and applies whatever actions it prints.
// Actions from a run that exited non-zero are discarded: a crashing plugin
// should not half-act.
func (p *plugin) run(env pluginEnvelope) {
	home := filepath.Join(pluginsDir(), p.name+".d")
	_ = os.MkdirAll(home, 0o700)

	payload, err := json.Marshal(env)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.path, "event")
	cmd.Env = append(os.Environ(),
		"BRIDGE_EVENT="+env.Event, "BRIDGE_PLUGIN_HOME="+home)
	cmd.Stdin = bytes.NewReader(payload)
	stdout := &boundedBuf{cap: pluginStdoutCap}
	stderr := &boundedBuf{cap: pluginStderrCap}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	if err := cmd.Run(); err != nil {
		what := err.Error()
		if ctx.Err() == context.DeadlineExceeded {
			what = "timeout after " + pluginExecTimeout.String()
		}
		if s := strings.TrimSpace(stderr.buf.String()); s != "" {
			what += " | " + s
		}
		audit("plugin-error", p.name+" ("+env.Event+"): "+what, "plugin")
		return
	}

	actions := parsePluginActions(stdout.buf.Bytes())
	if len(actions) > pluginActionCap {
		audit("plugin-action-capped", fmt.Sprintf("%s printed %d actions, applying %d",
			p.name, len(actions), pluginActionCap), "plugin")
		actions = actions[:pluginActionCap]
	}
	nudgeIndex := 0 // per-batch stagger index; only LIVE nudge deliveries consume one
	for _, a := range actions {
		applyPluginAction(p.name, a, &nudgeIndex)
	}
	audit("plugin-run", fmt.Sprintf("%s %s: %d action(s)", p.name, env.Event, len(actions)), "plugin")
}

// parsePluginActions accepts one JSON object per line, or a single JSON array.
// Garbage is dropped with an audit line — a plugin cannot crash the daemon
// with its stdout.
func parsePluginActions(out []byte) []json.RawMessage {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	if out[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(out, &arr); err != nil {
			audit("plugin-bad-output", "unparseable action array", "plugin")
			return nil
		}
		return arr
	}
	var actions []json.RawMessage
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			audit("plugin-bad-output", "unparseable action line", "plugin")
			continue
		}
		actions = append(actions, json.RawMessage(line))
	}
	return actions
}

// applyPluginAction executes one action from the v1 vocabulary (docs/plugins.md).
// Every apply and every refusal is audited with the plugin's name. nudgeIndex is
// the caller's per-batch counter: a "nudge" delivered to a LIVE agent staggers on
// fanoutOffset(*nudgeIndex) and bumps it, so a plugin that prods many agents in
// one tick doesn't fire N Claude API calls at one instant (the thundering-herd
// fix, matched to fanoutRoom). Only live nudges consume an index — offline ones
// queue durably with no herd, and no other action touches it.
func applyPluginAction(pname string, raw json.RawMessage, nudgeIndex *int) {
	var a struct {
		Action  string `json:"action"`
		Contact string `json:"contact"`
		Text    string `json:"text"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Key     string `json:"key"`
		Value   string `json:"value"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		audit("plugin-bad-output", pname+": malformed action", "plugin")
		return
	}
	switch a.Action {
	case "nudge":
		c := registry.Resolve(a.Contact)
		if c == nil || strings.TrimSpace(a.Text) == "" {
			audit("plugin-action-refused", pname+" nudge: unknown contact or empty text", "plugin")
			return
		}
		// Route through the same hold-and-batch as every other inbound: a nudge
		// now waits out an open permission dialog (no blind Enter) and can never
		// overtake queued mail — the ordering invariant the coalescer promises
		// (review H3). Emitted:true keeps it out of the phone feed: a nudge
		// prods the agent, it isn't a user-facing message (unchanged behavior).
		m := MailMessage{From: "plugin:" + pname, Via: "plugin", Text: a.Text, TS: nowUTC(), Emitted: true}
		if c.Status == "live" {
			// Stagger this live nudge against the others in the same batch so a
			// storm of nudges doesn't burst the crew's API calls at one instant
			// (offset 0 == feature off == plain holdInbound).
			holdInboundStaggered(c, m, fanoutOffset(*nudgeIndex))
			*nudgeIndex++
			audit("plugin-action", pname+" nudge -> "+c.Name, "plugin")
		} else {
			registry.Queue(c.ID, m)
			audit("plugin-action", pname+" nudge queued for "+c.Name, "plugin")
		}
	case "notify":
		if strings.TrimSpace(a.Title) == "" && strings.TrimSpace(a.Body) == "" {
			audit("plugin-action-refused", pname+" notify: empty", "plugin")
			return
		}
		notifyPush(a.Title, a.Body, "plugin:"+pname, a.Contact)
		audit("plugin-action", pname+" notify: "+a.Title, "plugin")
	case "emit":
		c := registry.Resolve(a.Contact)
		if c == nil || strings.TrimSpace(a.Text) == "" {
			audit("plugin-action-refused", pname+" emit: unknown contact or empty text", "plugin")
			return
		}
		Emit("plugin", c.ID, pname, a.Text)
		audit("plugin-action", pname+" emit -> "+c.Name, "plugin")
	case "set-field":
		c := registry.Resolve(a.Contact)
		if c == nil || !pluginNameRe.MatchString(a.Key) || len(a.Value) > pluginValueCap {
			audit("plugin-action-refused", pname+" set-field: bad contact/key/value", "plugin")
			return
		}
		if !registry.SetField(c.ID, a.Key, a.Value) {
			audit("plugin-action-refused", pname+" set-field: field cap reached on "+c.Name, "plugin")
			return
		}
		audit("plugin-action", pname+" set-field "+a.Key+" on "+c.Name, "plugin")
	default:
		audit("plugin-action-refused", pname+": unknown action "+a.Action, "plugin")
	}
}

// pluginTickLoop is the 60s heartbeat: a tick envelope carrying the roster,
// with no single contact attached.
func pluginTickLoop() {
	t := time.NewTicker(pluginTickEvery)
	defer t.Stop()
	for range t.C {
		if pluginOff.Load() {
			return
		}
		roster := []map[string]string{}
		for _, c := range registry.Roster() {
			roster = append(roster, map[string]string{
				"id": c.ID, "name": c.Name, "directory": c.Directory,
				"status": c.Status, "health": c.Health,
			})
		}
		dispatchPluginEvent("tick", nil, map[string]any{"contacts": roster})
	}
}

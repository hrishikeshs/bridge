package main

// codex.go — a protocol-v2 remote client backed by the official Codex App
// Server. `bridge codex` owns the stdio JSON-RPC connection; the daemon remains
// provider-neutral and sees only SemanticCommand/SemanticEvent values.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

type appRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type appServerClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	write  sync.Mutex
	nextID atomic.Int64
	mu     sync.Mutex
	waits  map[string]chan appRPCMessage
	events chan appRPCMessage
	done   chan error
}

func startAppServer(ctx context.Context, executable string) (*appServerClient, error) {
	if executable == "" {
		executable = "codex"
	}
	cmd := exec.CommandContext(ctx, executable, "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	c := &appServerClient{
		cmd: cmd, stdin: stdin, waits: map[string]chan appRPCMessage{},
		events: make(chan appRPCMessage, 128), done: make(chan error, 1),
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.readLoop(stdout)
	return c, nil
}

func (c *appServerClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var msg appRPCMessage
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		// Messages with both method and id are server-initiated requests. Plain
		// responses have an id but no method; notifications have method only.
		if len(msg.ID) > 0 && msg.Method == "" {
			key := string(msg.ID)
			c.mu.Lock()
			ch := c.waits[key]
			delete(c.waits, key)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
				close(ch)
			}
			continue
		}
		queueAppServerEvent(c.events, msg)
	}
	err := sc.Err()
	waitErr := c.cmd.Wait()
	if err == nil {
		err = waitErr
	}
	c.done <- err
	close(c.events)
}

func droppableAppServerNotification(msg appRPCMessage) bool {
	return len(msg.ID) == 0 && strings.HasSuffix(msg.Method, "/delta")
}

func queueAppServerEvent(events chan appRPCMessage, msg appRPCMessage) {
	if droppableAppServerNotification(msg) {
		select {
		case events <- msg:
		default:
			// The completed item contains the full message, so a delta may be
			// discarded under pressure without losing user-visible content.
		}
		return
	}
	// Server requests and lifecycle notifications are lossless. Dropping
	// item/completed loses the reply; dropping turn/completed leaves the bridge
	// believing a finished turn is still active.
	events <- msg
}

func (c *appServerClient) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.write.Lock()
	defer c.write.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *appServerClient) request(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan appRPCMessage, 1)
	c.mu.Lock()
	c.waits[key] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.waits, key)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waits, key)
		c.mu.Unlock()
		return ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("codex app-server %s: %s (%d)", method, msg.Error.Message, msg.Error.Code)
		}
		if out != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

func (c *appServerClient) notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *appServerClient) respond(requestID string, result any) error {
	var id any
	if json.Unmarshal([]byte(requestID), &id) != nil {
		id = requestID
	}
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *appServerClient) respondError(requestID string, code int, message string) error {
	var id any
	if json.Unmarshal([]byte(requestID), &id) != nil {
		id = requestID
	}
	return c.send(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

type codexBridge struct {
	ctx      context.Context
	rpc      *appServerClient
	threadID string
	name     string
	cwd      string

	mu              sync.Mutex
	lease           string
	contact         string
	contactName     string
	heartbeatEvery  time.Duration
	turnID          string
	messageBuf      map[string]string
	pendingApproval *SemanticEvent
	executed        map[string]bool

	registerMu sync.Mutex
	eventMu    sync.Mutex
	eventQueue []SemanticEvent
	eventWake  chan struct{}
	retryDelay time.Duration
}

type codexPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

func runCodexBridge(ctx context.Context, name, threadID, model, executable string) error {
	if name != "" && !nameConnectRe.MatchString(name) {
		return fmt.Errorf("invalid --name %q: start with a letter, then letters/digits/-/_ (max 31 chars)", name)
	}
	if err := ensureDaemon(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	rpc, err := startAppServer(ctx, executable)
	if err != nil {
		return fmt.Errorf("start Codex App Server: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var initialized map[string]any
	if err := rpc.request(requestCtx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "bridge", "title": "Bridge", "version": version},
	}, &initialized); err != nil {
		return err
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return err
	}

	if threadID != "" {
		var resumed struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := rpc.request(requestCtx, "thread/resume", map[string]any{"threadId": threadID}, &resumed); err != nil {
			return err
		}
		if resumed.Thread.ID != "" {
			threadID = resumed.Thread.ID
		}
	} else {
		params := map[string]any{"cwd": cwd}
		if model != "" {
			params["model"] = model
		}
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := rpc.request(requestCtx, "thread/start", params, &started); err != nil {
			return err
		}
		threadID = started.Thread.ID
	}
	if threadID == "" {
		return fmt.Errorf("Codex App Server returned no thread id")
	}
	if name == "" {
		name = generateName(map[string]bool{})
	}

	b := &codexBridge{
		ctx: ctx, rpc: rpc, threadID: threadID, name: name, cwd: cwd,
		messageBuf: map[string]string{}, executed: map[string]bool{},
		eventWake: make(chan struct{}, 1), retryDelay: time.Second,
	}
	if _, err := b.register(""); err != nil {
		return err
	}
	_, _, contactName := b.route()
	fmt.Printf("%s is connected through Codex App Server (thread %s).\n", contactName, shortID(threadID))

	go b.postLoop()
	go b.heartbeat()
	go b.commandLoop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-rpc.done:
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				return fmt.Errorf("Codex App Server exited")
			}
			return fmt.Errorf("Codex App Server exited: %w", err)
		case msg, ok := <-rpc.events:
			if !ok {
				err := <-rpc.done
				if err == nil {
					return fmt.Errorf("Codex App Server exited")
				}
				return fmt.Errorf("Codex App Server exited: %w", err)
			}
			b.handleRPC(msg)
		}
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

var errCodexLeaseReplaced = errors.New("Codex bridge lease replaced")

// register obtains a protocol-v2 lease. expectedLease serializes recovery: if
// another goroutine already replaced the failed lease, the late caller does no
// work. This is the same hello used at startup, so daemon restarts preserve the
// contact identity through ConnectRemote's name+directory adoption rule.
func (b *codexBridge) register(expectedLease string) (bool, error) {
	b.registerMu.Lock()
	defer b.registerMu.Unlock()

	b.mu.Lock()
	if expectedLease != "" && b.lease != expectedLease {
		b.mu.Unlock()
		return false, nil
	}
	b.mu.Unlock()

	var hello struct {
		Agents   []struct{ ID, Name string } `json:"agents"`
		Lease    string                      `json:"lease"`
		TTLS     int                         `json:"ttl_s"`
		Protocol int                         `json:"protocol"`
	}
	requestCtx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
	defer cancel()
	if err := semanticDaemonRequest(requestCtx, http.MethodPost, "/local/transport/v2/hello", map[string]any{
		"transport": "codex", "protocol": 2, "provider": "codex",
		"agents": []map[string]string{{"name": b.name, "directory": b.cwd, "session_id": b.threadID}},
	}, &hello); err != nil {
		return false, err
	}
	if hello.Lease == "" || len(hello.Agents) != 1 || hello.Protocol != 2 {
		return false, fmt.Errorf("bridge daemon returned an incomplete v2 lease")
	}
	if hello.TTLS < 3 {
		hello.TTLS = 30
	}
	b.mu.Lock()
	b.lease = hello.Lease
	b.contact = hello.Agents[0].ID
	b.contactName = hello.Agents[0].Name
	b.heartbeatEvery = time.Duration(hello.TTLS) * time.Second / 3
	b.mu.Unlock()
	b.enqueuePendingApproval()
	return true, nil
}

func (b *codexBridge) route() (lease, contact, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lease, b.contact, b.contactName
}

func (b *codexBridge) recoverLease(expiredLease string) error {
	recovered, err := b.register(expiredLease)
	if err != nil {
		return err
	}
	if recovered {
		fmt.Fprintln(os.Stderr, "bridge codex: reconnected to the Bridge daemon")
	}
	return nil
}

func (b *codexBridge) retryWait() bool {
	delay := b.retryDelay
	if delay <= 0 {
		delay = time.Second
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (b *codexBridge) heartbeat() {
	for b.ctx.Err() == nil {
		if err := b.attest(); err != nil && b.ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "bridge codex heartbeat:", err)
		}
		b.mu.Lock()
		every := b.heartbeatEvery
		b.mu.Unlock()
		if every <= 0 {
			every = 10 * time.Second
		}
		t := time.NewTimer(every)
		select {
		case <-b.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (b *codexBridge) attest() error {
	b.mu.Lock()
	lease, contact := b.lease, b.contact
	promptOpen := b.pendingApproval != nil
	ready := b.turnID == "" && !promptOpen
	b.mu.Unlock()
	err := semanticDaemonRequest(b.ctx, http.MethodPost, "/local/transport/attest", map[string]any{
		"lease":  lease,
		"states": []map[string]any{{"id": contact, "ready": ready, "prompt_open": promptOpen}},
	}, nil)
	if daemonLeaseGone(err) {
		return b.recoverLease(lease)
	}
	return err
}

func (b *codexBridge) commandLoop() {
	for b.ctx.Err() == nil {
		lease, _, _ := b.route()
		var resp struct {
			Commands []SemanticCommand `json:"commands"`
		}
		path := "/local/transport/v2/commands?lease=" + url.QueryEscape(lease) + "&wait=25"
		if err := semanticDaemonRequest(b.ctx, http.MethodGet, path, nil, &resp); err != nil {
			if daemonLeaseGone(err) {
				err = b.recoverLease(lease)
			}
			if err != nil && b.ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "bridge codex commands:", err)
			}
			if !b.retryWait() {
				return
			}
			continue
		}
		for _, command := range resp.Commands {
			if err := b.processCommand(lease, command); err != nil {
				if errors.Is(err, errCodexLeaseReplaced) {
					break
				}
				fmt.Fprintln(os.Stderr, "bridge codex:", err)
				continue // no ack: daemon retains/redelivers the command
			}
		}
	}
}

func (b *codexBridge) processCommand(lease string, command SemanticCommand) error {
	key := lease + ":" + command.ID
	b.mu.Lock()
	alreadyExecuted := b.executed[key]
	b.mu.Unlock()
	if !alreadyExecuted {
		if err := b.execute(command); err != nil {
			return err
		}
		b.mu.Lock()
		b.executed[key] = true
		b.mu.Unlock()
	}
	for b.ctx.Err() == nil {
		err := semanticDaemonRequest(b.ctx, http.MethodPost, "/local/transport/v2/ack", map[string]any{
			"lease": lease, "ids": []string{command.ID},
		}, nil)
		if err == nil {
			b.mu.Lock()
			delete(b.executed, key)
			b.mu.Unlock()
			return nil
		}
		if daemonLeaseGone(err) {
			if recoverErr := b.recoverLease(lease); recoverErr != nil {
				return recoverErr
			}
			return errCodexLeaseReplaced
		}
		if !b.retryWait() {
			return b.ctx.Err()
		}
	}
	return b.ctx.Err()
}

func (b *codexBridge) execute(command SemanticCommand) error {
	ctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancel()
	switch command.Type {
	case SemanticCommandInput:
		b.mu.Lock()
		turnID := b.turnID
		b.mu.Unlock()
		input := []map[string]string{{"type": "text", "text": command.Text}}
		if turnID != "" {
			return b.rpc.request(ctx, "turn/steer", map[string]any{
				"threadId": b.threadID, "expectedTurnId": turnID, "input": input,
			}, nil)
		}
		var started struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := b.rpc.request(ctx, "turn/start", map[string]any{"threadId": b.threadID, "input": input}, &started); err != nil {
			return err
		}
		b.mu.Lock()
		b.turnID = started.Turn.ID
		b.mu.Unlock()
		return nil
	case SemanticCommandInterrupt:
		b.mu.Lock()
		turnID := b.turnID
		b.mu.Unlock()
		if turnID == "" {
			return nil
		}
		return b.rpc.request(ctx, "turn/interrupt", map[string]string{"threadId": b.threadID, "turnId": turnID}, nil)
	case SemanticCommandApproval:
		result, err := codexApprovalResponse(command)
		if err != nil {
			return err
		}
		return b.rpc.respond(command.RequestID, result)
	case SemanticCommandCompact:
		return b.rpc.request(ctx, "thread/compact/start", map[string]string{"threadId": b.threadID}, nil)
	default:
		return fmt.Errorf("unknown semantic command %q", command.Type)
	}
}

// codexApprovalResponse selects the response schema for the request method.
// Command and file approvals use a named decision. Permission requests instead
// expect the granted subset and scope: accept grants exactly what Codex asked
// for, while decline/cancel grants an empty profile (Codex's documented deny).
func codexApprovalResponse(command SemanticCommand) (any, error) {
	switch command.ApprovalKind {
	case "", "command", "file_change":
		return map[string]string{"decision": command.Decision}, nil
	case "permissions":
		permissions := map[string]any{}
		scope := "turn"
		switch command.Decision {
		case "accept", "acceptForSession":
			if len(command.Permissions) == 0 {
				return nil, fmt.Errorf("permission approval %s has no requested profile", command.RequestID)
			}
			if err := json.Unmarshal(command.Permissions, &permissions); err != nil {
				return nil, fmt.Errorf("decode requested permission profile: %w", err)
			}
			if command.Decision == "acceptForSession" {
				scope = "session"
			}
		case "decline", "cancel":
			// An empty GrantedPermissionProfile is the app-server denial shape.
		default:
			return nil, fmt.Errorf("unsupported permission decision %q", command.Decision)
		}
		return map[string]any{"permissions": permissions, "scope": scope}, nil
	default:
		return nil, fmt.Errorf("unsupported approval kind %q", command.ApprovalKind)
	}
}

func (b *codexBridge) handleRPC(msg appRPCMessage) {
	if len(msg.ID) > 0 && msg.Method != "" {
		b.handleServerRequest(msg)
		return
	}
	var p struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		ItemID    string          `json:"itemId"`
		Delta     string          `json:"delta"`
		RequestID json.RawMessage `json:"requestId"`
		Turn      struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		Item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	switch msg.Method {
	case "turn/started":
		b.mu.Lock()
		b.turnID = p.Turn.ID
		b.mu.Unlock()
		b.enqueueEvent(SemanticEvent{Type: SemanticEventStatus, Status: "working"})
	case "item/agentMessage/delta":
		b.mu.Lock()
		b.messageBuf[p.ItemID] += p.Delta
		b.mu.Unlock()
	case "turn/plan/updated":
		var plan struct {
			Explanation string          `json:"explanation"`
			Plan        []codexPlanStep `json:"plan"`
		}
		if json.Unmarshal(msg.Params, &plan) == nil {
			b.enqueueEvent(SemanticEvent{Type: SemanticEventPlan, Text: formatCodexPlan(plan.Explanation, plan.Plan)})
		}
	case "item/completed":
		itemID := p.Item.ID
		b.mu.Lock()
		message := b.messageBuf[itemID]
		delete(b.messageBuf, itemID)
		b.mu.Unlock()
		if p.Item.Type == "agentMessage" {
			if message == "" {
				message = p.Item.Text
			}
			b.enqueueEvent(SemanticEvent{Type: SemanticEventAgentMessage, Text: message})
		} else if p.Item.Type == "plan" {
			b.enqueueEvent(SemanticEvent{Type: SemanticEventPlan, Text: p.Item.Text})
		}
	case "turn/completed":
		b.mu.Lock()
		b.turnID = ""
		b.mu.Unlock()
		status := p.Turn.Status
		if status == "" {
			status = "completed"
		}
		b.enqueueEvent(SemanticEvent{Type: SemanticEventStatus, Status: status})
	case "serverRequest/resolved":
		requestID := string(p.RequestID)
		b.mu.Lock()
		if b.pendingApproval != nil && b.pendingApproval.RequestID == requestID {
			b.pendingApproval = nil
		}
		b.mu.Unlock()
		b.enqueueEvent(SemanticEvent{Type: SemanticEventApprovalResolved, RequestID: requestID})
	}
}

func (b *codexBridge) handleServerRequest(msg appRPCMessage) {
	if msg.Method != "item/commandExecution/requestApproval" &&
		msg.Method != "item/fileChange/requestApproval" &&
		msg.Method != "item/permissions/requestApproval" {
		// Different approval methods have different response schemas. Return a
		// protocol error instead of fabricating a shape that could grant access.
		_ = b.rpc.respondError(string(msg.ID), -32601, "unsupported approval request")
		return
	}
	var p struct {
		Reason             string          `json:"reason"`
		Command            any             `json:"command"`
		Cwd                string          `json:"cwd"`
		AvailableDecisions []string        `json:"availableDecisions"`
		Permissions        json.RawMessage `json:"permissions"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	command := ""
	switch v := p.Command.(type) {
	case string:
		command = v
	case []any:
		parts := make([]string, 0, len(v))
		for _, part := range v {
			parts = append(parts, fmt.Sprint(part))
		}
		command = strings.Join(parts, " ")
	}
	kind := "command"
	switch msg.Method {
	case "item/fileChange/requestApproval":
		kind = "file_change"
	case "item/permissions/requestApproval":
		kind = "permissions"
	}
	event := SemanticEvent{
		Type: SemanticEventApprovalRequested, RequestID: string(msg.ID), ApprovalKind: kind,
		Reason: p.Reason, Command: command, Cwd: p.Cwd, AvailableDecisions: p.AvailableDecisions,
		Permissions: p.Permissions,
	}
	b.mu.Lock()
	b.pendingApproval = &event
	b.mu.Unlock()
	b.enqueueEvent(event)
}

func formatCodexPlan(explanation string, steps []codexPlanStep) string {
	var lines []string
	if explanation = strings.TrimSpace(explanation); explanation != "" {
		lines = append(lines, explanation)
	}
	for _, step := range steps {
		lines = append(lines, fmt.Sprintf("[%s] %s", step.Status, step.Step))
	}
	return strings.Join(lines, "\n")
}

func (b *codexBridge) enqueueEvent(event SemanticEvent) {
	b.eventMu.Lock()
	b.eventQueue = append(b.eventQueue, event)
	b.eventMu.Unlock()
	select {
	case b.eventWake <- struct{}{}:
	default:
	}
}

// enqueuePendingApproval replays an unresolved app-server request after a
// daemon re-hello. If its original event is already waiting for delivery, the
// queue check keeps the replay idempotent.
func (b *codexBridge) enqueuePendingApproval() {
	b.mu.Lock()
	if b.pendingApproval == nil {
		b.mu.Unlock()
		return
	}
	event := *b.pendingApproval
	b.mu.Unlock()

	b.eventMu.Lock()
	for _, queued := range b.eventQueue {
		if queued.Type == SemanticEventApprovalRequested && queued.RequestID == event.RequestID {
			b.eventMu.Unlock()
			return
		}
	}
	b.eventQueue = append(b.eventQueue, event)
	b.eventMu.Unlock()
	select {
	case b.eventWake <- struct{}{}:
	default:
	}
}

// postLoop provides ordered, retryable delivery for user-visible semantic
// events. It is deliberately separate from app-server stdout consumption: a
// daemon outage can delay delivery, but cannot back up the RPC reader until it
// starts dropping completion notifications.
func (b *codexBridge) postLoop() {
	for b.ctx.Err() == nil {
		b.eventMu.Lock()
		if len(b.eventQueue) == 0 {
			b.eventMu.Unlock()
			select {
			case <-b.ctx.Done():
				return
			case <-b.eventWake:
				continue
			}
		}
		event := b.eventQueue[0]
		b.eventMu.Unlock()

		lease, contact, _ := b.route()
		event.Contact = contact
		err := semanticDaemonRequest(b.ctx, http.MethodPost, "/local/transport/v2/events", map[string]any{
			"lease": lease, "events": []SemanticEvent{event},
		}, nil)
		if err == nil {
			b.eventMu.Lock()
			b.eventQueue = b.eventQueue[1:]
			b.eventMu.Unlock()
			continue
		}
		if daemonLeaseGone(err) {
			err = b.recoverLease(lease)
		}
		if err != nil && b.ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "bridge codex events:", err)
		}
		if !b.retryWait() {
			return
		}
	}
}

type daemonHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *daemonHTTPError) Error() string {
	return fmt.Sprintf("bridge daemon returned %s: %s", e.Status, e.Body)
}

func daemonLeaseGone(err error) bool {
	var responseErr *daemonHTTPError
	return errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusGone
}

func semanticDaemonRequest(ctx context.Context, method, path string, body, out any) error {
	lf, err := readLockfile()
	if err != nil {
		return fmt.Errorf("bridge daemon not running: %w", err)
	}
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", lf.Port, path), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+lf.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &daemonHTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(data)),
		}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func codexCmd() *cobra.Command {
	var name, threadID, model, executable string
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Host a Codex thread through Bridge's semantic transport",
		Long: `Host a Codex thread through the official Codex App Server.

This is additive to the Claude/tmux connect workflow. It starts a local
app-server process, creates a thread (or resumes --thread), and translates
Bridge messages, interrupts, streamed output, plans, and approvals without
scraping a terminal or reading raw reasoning. Stop it with Ctrl-C.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runCodexBridge(ctx, name, threadID, model, executable)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the address this Codex thread answers to")
	cmd.Flags().StringVar(&threadID, "thread", "", "resume this Codex thread id instead of creating one")
	cmd.Flags().StringVar(&model, "model", "", "model for a newly created thread (Codex default when empty)")
	cmd.Flags().StringVar(&executable, "codex", "codex", "Codex CLI executable")
	return cmd
}

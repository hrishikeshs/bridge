package main

// codex.go — a protocol-v2 remote client backed by the official Codex App
// Server. `bridge codex` owns the stdio JSON-RPC connection; the daemon remains
// provider-neutral and sees only SemanticCommand/SemanticEvent values.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		if len(msg.ID) > 0 && msg.Method != "" {
			// Approval requests must never be dropped: losing one leaves the turn
			// blocked behind a prompt that no Bridge client can see.
			c.events <- msg
		} else {
			select {
			case c.events <- msg:
			default:
				// Never deadlock app-server's stdout on high-volume deltas.
			}
		}
	}
	err := sc.Err()
	waitErr := c.cmd.Wait()
	if err == nil {
		err = waitErr
	}
	c.done <- err
	close(c.events)
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
	lease    string
	contact  string
	threadID string

	mu         sync.Mutex
	turnID     string
	messageBuf map[string]string
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

	var hello struct {
		Agents []struct{ ID, Name string } `json:"agents"`
		Lease  string                      `json:"lease"`
		TTLS   int                         `json:"ttl_s"`
	}
	err = semanticDaemonRequest(ctx, http.MethodPost, "/local/transport/v2/hello", map[string]any{
		"transport": "codex", "protocol": 2, "provider": "codex",
		"agents": []map[string]string{{"name": name, "directory": cwd, "session_id": threadID}},
	}, &hello)
	if err != nil {
		return err
	}
	if hello.Lease == "" || len(hello.Agents) != 1 {
		return fmt.Errorf("bridge daemon returned an incomplete v2 lease")
	}
	b := &codexBridge{
		ctx: ctx, rpc: rpc, lease: hello.Lease, contact: hello.Agents[0].ID,
		threadID: threadID, messageBuf: map[string]string{},
	}
	if hello.TTLS < 3 {
		hello.TTLS = 30
	}
	fmt.Printf("%s is connected through Codex App Server (thread %s).\n", hello.Agents[0].Name, shortID(threadID))

	go b.heartbeat(time.Duration(hello.TTLS) * time.Second / 3)
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

func (b *codexBridge) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		b.attest()
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *codexBridge) attest() {
	b.mu.Lock()
	ready := b.turnID == ""
	b.mu.Unlock()
	_ = semanticDaemonRequest(b.ctx, http.MethodPost, "/local/transport/attest", map[string]any{
		"lease":  b.lease,
		"states": []map[string]any{{"id": b.contact, "ready": ready, "prompt_open": false}},
	}, nil)
}

func (b *codexBridge) commandLoop() {
	for b.ctx.Err() == nil {
		var resp struct {
			Commands []SemanticCommand `json:"commands"`
		}
		path := "/local/transport/v2/commands?lease=" + b.lease + "&wait=25"
		if err := semanticDaemonRequest(b.ctx, http.MethodGet, path, nil, &resp); err != nil {
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		for _, command := range resp.Commands {
			if err := b.execute(command); err != nil {
				fmt.Fprintln(os.Stderr, "bridge codex:", err)
				continue // no ack: daemon retains/redelivers under the v1 contract
			}
			_ = semanticDaemonRequest(b.ctx, http.MethodPost, "/local/transport/v2/ack", map[string]any{
				"lease": b.lease, "ids": []string{command.ID},
			}, nil)
		}
	}
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
		return b.rpc.respond(command.RequestID, map[string]string{"decision": command.Decision})
	default:
		return fmt.Errorf("unknown semantic command %q", command.Type)
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
		b.post(SemanticEvent{Type: SemanticEventStatus, Status: "working"})
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
			b.post(SemanticEvent{Type: SemanticEventPlan, Text: formatCodexPlan(plan.Explanation, plan.Plan)})
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
			b.post(SemanticEvent{Type: SemanticEventAgentMessage, Text: message})
		} else if p.Item.Type == "plan" {
			b.post(SemanticEvent{Type: SemanticEventPlan, Text: p.Item.Text})
		}
	case "turn/completed":
		b.mu.Lock()
		b.turnID = ""
		b.mu.Unlock()
		status := p.Turn.Status
		if status == "" {
			status = "completed"
		}
		b.post(SemanticEvent{Type: SemanticEventStatus, Status: status})
	case "serverRequest/resolved":
		requestID := string(p.RequestID)
		b.post(SemanticEvent{Type: SemanticEventApprovalResolved, RequestID: requestID})
	}
}

func (b *codexBridge) handleServerRequest(msg appRPCMessage) {
	if msg.Method != "item/commandExecution/requestApproval" && msg.Method != "item/fileChange/requestApproval" {
		// Different approval methods have different response schemas. Return a
		// protocol error instead of fabricating a shape that could grant access.
		_ = b.rpc.respondError(string(msg.ID), -32601, "unsupported approval request")
		return
	}
	var p struct {
		Reason             string   `json:"reason"`
		Command            any      `json:"command"`
		Cwd                string   `json:"cwd"`
		AvailableDecisions []string `json:"availableDecisions"`
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
	if msg.Method == "item/fileChange/requestApproval" {
		kind = "file_change"
	}
	b.post(SemanticEvent{
		Type: SemanticEventApprovalRequested, RequestID: string(msg.ID), ApprovalKind: kind,
		Reason: p.Reason, Command: command, Cwd: p.Cwd, AvailableDecisions: p.AvailableDecisions,
	})
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

func (b *codexBridge) post(event SemanticEvent) {
	event.Contact = b.contact
	_ = semanticDaemonRequest(b.ctx, http.MethodPost, "/local/transport/v2/events", map[string]any{
		"lease": b.lease, "events": []SemanticEvent{event},
	}, nil)
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
		return fmt.Errorf("bridge daemon returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

type channelWriteCloser struct{ writes chan []byte }

func (w *channelWriteCloser) Write(p []byte) (int, error) {
	copyOfP := append([]byte(nil), p...)
	w.writes <- copyOfP
	return len(p), nil
}

func (*channelWriteCloser) Close() error { return nil }

func installTestDaemon(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := ensureBridgeDir(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLockfile(port, "test-token"); err != nil {
		t.Fatal(err)
	}
	return server
}

func TestAppServerMessagesDeclareJSONRPC2(t *testing.T) {
	w := &bufferWriteCloser{}
	c := &appServerClient{stdin: w}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(w.Bytes()), &message); err != nil {
		t.Fatal(err)
	}
	if message["jsonrpc"] != "2.0" || message["method"] != "initialized" {
		t.Fatalf("unexpected JSON-RPC notification: %#v", message)
	}
}

func TestLegacyApprovalKeysMapOnlyForSemanticContacts(t *testing.T) {
	cases := map[string]string{
		"1": "accept", "y": "accept", "2": "acceptForSession",
		"3": "decline", "n": "decline", "esc": "cancel",
	}
	for key, want := range cases {
		got, ok := semanticDecisionForLegacyKey(key)
		if !ok || got != want {
			t.Errorf("semanticDecisionForLegacyKey(%q) = %q, %v; want %q, true", key, got, ok, want)
		}
	}
	if _, ok := semanticDecisionForLegacyKey("q"); ok {
		t.Fatal("unexpected semantic mapping for an unapproved key")
	}
}

func TestSemanticApprovalTextRemainsCompatibleWithPWAButtons(t *testing.T) {
	text := formatSemanticApproval(SemanticEvent{Command: "go test ./...", Cwd: "/project"}, "Run tests?")
	for _, option := range []string{"1. Yes", "2. Yes, and allow for this session", "3. No"} {
		if !strings.Contains(text, option) {
			t.Errorf("approval text missing %q: %s", option, text)
		}
	}
}

func TestCodexPlanUsesCurrentTurnPlanShape(t *testing.T) {
	got := formatCodexPlan("Implementation", []codexPlanStep{
		{Step: "Add adapter", Status: "inProgress"},
		{Step: "Run tests", Status: "pending"},
	})
	want := "Implementation\n[inProgress] Add adapter\n[pending] Run tests"
	if got != want {
		t.Fatalf("formatCodexPlan() = %q, want %q", got, want)
	}
}

func TestQueuePressureDropsOnlyDeltas(t *testing.T) {
	events := make(chan appRPCMessage, 1)
	events <- appRPCMessage{Method: "already/queued"}

	queueAppServerEvent(events, appRPCMessage{Method: "item/agentMessage/delta"})
	if len(events) != 1 {
		t.Fatalf("delta changed a saturated queue length to %d", len(events))
	}

	done := make(chan struct{})
	go func() {
		queueAppServerEvent(events, appRPCMessage{Method: "turn/completed"})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("lifecycle notification was dropped instead of applying backpressure")
	case <-time.After(10 * time.Millisecond):
	}
	<-events
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lifecycle notification did not enter the queue after capacity returned")
	}
	if got := <-events; got.Method != "turn/completed" {
		t.Fatalf("queued method = %q, want turn/completed", got.Method)
	}
}

func TestCompletedItemTextWinsAfterPartialDeltas(t *testing.T) {
	b := &codexBridge{
		messageBuf: map[string]string{"item-1": "only the prefix"},
		eventWake:  make(chan struct{}, 1),
	}
	b.handleRPC(appRPCMessage{
		Method: "item/completed",
		Params: json.RawMessage(`{"item":{"id":"item-1","type":"agentMessage","text":"the complete authoritative answer"}}`),
	})
	if len(b.eventQueue) != 1 {
		t.Fatalf("queued events = %d, want 1", len(b.eventQueue))
	}
	if got := b.eventQueue[0].Text; got != "the complete authoritative answer" {
		t.Fatalf("completed reply = %q, want authoritative item.text", got)
	}
}

func TestSemanticPromptSurvivesTerminalVerification(t *testing.T) {
	const contactID = "structured-prompt-contact"
	semanticApprovalMu.Lock()
	semanticApprovals[contactID] = semanticApproval{RequestID: "7", Kind: "command"}
	semanticApprovalMu.Unlock()
	promptStrikes[contactID] = 1
	t.Cleanup(func() {
		clearSemanticApproval(contactID)
		delete(promptStrikes, contactID)
	})

	verifyPrompt(&Contact{ID: contactID, PromptOpen: true, Transport: "remote"})
	if _, struck := promptStrikes[contactID]; struck {
		t.Fatal("structured approval accumulated a terminal-capture miss")
	}
	if !semanticApprovalPending(contactID) {
		t.Fatal("structured approval was cleared by terminal verification")
	}
}

func TestStaleResolutionCannotClearNewerApproval(t *testing.T) {
	const contactID = "approval-race-contact"
	semanticApprovalMu.Lock()
	semanticApprovals[contactID] = semanticApproval{RequestID: "request-b", Kind: "command"}
	semanticApprovalMu.Unlock()
	t.Cleanup(func() { clearSemanticApproval(contactID) })

	if resolveSemanticApproval(contactID, "request-a") {
		t.Fatal("stale request A resolved newer request B")
	}
	semanticApprovalMu.Lock()
	pending := semanticApprovals[contactID]
	semanticApprovalMu.Unlock()
	if pending.RequestID != "request-b" {
		t.Fatalf("pending request = %q, want request-b", pending.RequestID)
	}
	if !resolveSemanticApproval(contactID, "request-b") {
		t.Fatal("matching request B did not resolve")
	}
}

func TestV2HelloPathForcesV2Negotiation(t *testing.T) {
	if got := negotiatedTransportProtocol("/local/transport/v2/hello", 0); got != 2 {
		t.Fatalf("v2 hello negotiated protocol %d, want 2", got)
	}
	if got := negotiatedTransportProtocol("/local/transport/hello", 0); got != 1 {
		t.Fatalf("legacy hello negotiated protocol %d, want 1", got)
	}
	if got := negotiatedTransportProtocol("/local/transport/hello", 2); got != 2 {
		t.Fatalf("explicit v2 request negotiated protocol %d, want 2", got)
	}
}

func TestPermissionApprovalUsesRequestedProfileAndEmptyDenial(t *testing.T) {
	requested := json.RawMessage(`{"network":{"enabled":true}}`)
	approved, err := codexApprovalResponse(SemanticCommand{
		RequestID: "9", ApprovalKind: "permissions", Decision: "acceptForSession", Permissions: requested,
	})
	if err != nil {
		t.Fatal(err)
	}
	approvedMap := approved.(map[string]any)
	if approvedMap["scope"] != "session" {
		t.Fatalf("approval scope = %#v, want session", approvedMap["scope"])
	}
	permissions := approvedMap["permissions"].(map[string]any)
	network := permissions["network"].(map[string]any)
	if network["enabled"] != true {
		t.Fatalf("granted profile = %#v, want requested network permission", permissions)
	}

	denied, err := codexApprovalResponse(SemanticCommand{
		RequestID: "10", ApprovalKind: "permissions", Decision: "decline", Permissions: requested,
	})
	if err != nil {
		t.Fatal(err)
	}
	deniedPermissions := denied.(map[string]any)["permissions"].(map[string]any)
	if len(deniedPermissions) != 0 {
		t.Fatalf("denial granted permissions: %#v", deniedPermissions)
	}
}

func TestPermissionsRequestIsSurfacedInsteadOfRejected(t *testing.T) {
	b := &codexBridge{eventWake: make(chan struct{}, 1)}
	b.handleServerRequest(appRPCMessage{
		ID:     json.RawMessage(`17`),
		Method: "item/permissions/requestApproval",
		Params: json.RawMessage(`{"reason":"Need the network","cwd":"/project","permissions":{"network":{"enabled":true}}}`),
	})
	if len(b.eventQueue) != 1 {
		t.Fatalf("queued events = %d, want 1", len(b.eventQueue))
	}
	event := b.eventQueue[0]
	if event.ApprovalKind != "permissions" || event.RequestID != "17" {
		t.Fatalf("unexpected permission event: %#v", event)
	}
	if b.pendingApproval == nil || b.pendingApproval.RequestID != "17" {
		t.Fatal("permission request was not retained for daemon recovery")
	}
}

func TestCompactCommandCallsDedicatedAppServerMethod(t *testing.T) {
	w := &channelWriteCloser{writes: make(chan []byte, 1)}
	rpc := &appServerClient{stdin: w, waits: map[string]chan appRPCMessage{}}
	b := &codexBridge{ctx: context.Background(), rpc: rpc, threadID: "thread-42"}
	done := make(chan error, 1)
	go func() { done <- b.execute(SemanticCommand{Type: SemanticCommandCompact}) }()

	var request map[string]any
	select {
	case data := <-w.writes:
		if err := json.Unmarshal(bytes.TrimSpace(data), &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("compact command did not call app-server")
	}
	if request["method"] != "thread/compact/start" {
		t.Fatalf("compact method = %#v, want thread/compact/start", request["method"])
	}
	params := request["params"].(map[string]any)
	if params["threadId"] != "thread-42" {
		t.Fatalf("compact params = %#v", params)
	}
	rpc.mu.Lock()
	response := rpc.waits["1"]
	rpc.mu.Unlock()
	response <- appRPCMessage{Result: json.RawMessage(`{}`)}
	close(response)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSteerCarriesStableClientUserMessageID(t *testing.T) {
	w := &channelWriteCloser{writes: make(chan []byte, 1)}
	rpc := &appServerClient{stdin: w, waits: map[string]chan appRPCMessage{}}
	b := &codexBridge{ctx: context.Background(), rpc: rpc, threadID: "thread-42", turnID: "turn-active"}
	done := make(chan error, 1)
	go func() {
		done <- b.execute(SemanticCommand{
			Type: SemanticCommandInput, Text: "follow-up", DeliveryID: "group-follow-up",
		})
	}()

	var request map[string]any
	select {
	case data := <-w.writes:
		if err := json.Unmarshal(bytes.TrimSpace(data), &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("steer did not call app-server")
	}
	if request["method"] != "turn/steer" {
		t.Fatalf("method = %#v, want turn/steer", request["method"])
	}
	params := request["params"].(map[string]any)
	if params["clientUserMessageId"] != "group-follow-up" {
		t.Fatalf("clientUserMessageId = %#v, want group-follow-up", params["clientUserMessageId"])
	}
	rpc.mu.Lock()
	response := rpc.waits["1"]
	rpc.mu.Unlock()
	response <- appRPCMessage{Result: json.RawMessage(`{}`)}
	close(response)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodexBridgeReHellosAfterLeaseExpiry(t *testing.T) {
	var hellos atomic.Int32
	installTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/local/transport/attest":
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":"lease-expired"}`))
		case "/local/transport/v2/hello":
			hellos.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agents":[{"id":"contact-new","name":"wise-deer"}],"lease":"lease-new","ttl_s":30,"protocol":2}`))
		default:
			t.Fatalf("unexpected daemon path %s", r.URL.Path)
		}
	}))

	pending := SemanticEvent{Type: SemanticEventApprovalRequested, RequestID: "23", ApprovalKind: "command"}
	b := &codexBridge{
		ctx: context.Background(), name: "wise-deer", cwd: "/project", threadID: "thread-1",
		lease: "lease-old", contact: "contact-old", pendingApproval: &pending,
		eventWake: make(chan struct{}, 1), retryDelay: time.Millisecond,
	}
	if err := b.attest(); err != nil {
		t.Fatal(err)
	}
	lease, contact, _ := b.route()
	if lease != "lease-new" || contact != "contact-new" {
		t.Fatalf("recovered route = %q/%q, want lease-new/contact-new", lease, contact)
	}
	if hellos.Load() != 1 {
		t.Fatalf("hello count = %d, want 1", hellos.Load())
	}
	if len(b.eventQueue) != 1 || b.eventQueue[0].RequestID != "23" {
		t.Fatalf("pending approval was not replayed after re-hello: %#v", b.eventQueue)
	}
}

func TestSemanticCommandIdentitySurvivesLeaseRedelivery(t *testing.T) {
	var hellos atomic.Int32
	var acks atomic.Int32
	installTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/local/transport/v2/ack":
			acks.Add(1)
			var req struct {
				Lease string `json:"lease"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Lease == "lease-old" {
				w.WriteHeader(http.StatusGone)
				_, _ = w.Write([]byte(`{"error":"lease-expired"}`))
				return
			}
			if req.Lease != "lease-new" {
				t.Errorf("ack lease = %q, want lease-old or lease-new", req.Lease)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/local/transport/v2/hello":
			hellos.Add(1)
			_, _ = w.Write([]byte(`{"agents":[{"id":"contact","name":"wise-deer"}],"lease":"lease-new","ttl_s":30,"protocol":2}`))
		default:
			t.Errorf("unexpected daemon path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	writes := &channelWriteCloser{writes: make(chan []byte, 2)}
	rpc := &appServerClient{stdin: writes, waits: map[string]chan appRPCMessage{}}
	b := &codexBridge{
		ctx: context.Background(), rpc: rpc, threadID: "thread-1", name: "wise-deer", cwd: "/project",
		lease: "lease-old", contact: "contact", contactName: "wise-deer",
		messageBuf: map[string]string{}, executed: map[string]bool{},
		eventWake: make(chan struct{}, 1), retryDelay: time.Millisecond,
	}
	first := SemanticCommand{
		ID: "wire-old", Type: SemanticCommandInput, Text: "one user message", DeliveryID: "group-stable",
	}
	done := make(chan error, 1)
	go func() { done <- b.processCommand("lease-old", first) }()

	var request map[string]any
	select {
	case data := <-writes.writes:
		if err := json.Unmarshal(bytes.TrimSpace(data), &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first delivery did not reach app-server")
	}
	params := request["params"].(map[string]any)
	if params["clientUserMessageId"] != "group-stable" {
		t.Fatalf("clientUserMessageId = %#v, want group-stable", params["clientUserMessageId"])
	}
	rpc.mu.Lock()
	response := rpc.waits["1"]
	rpc.mu.Unlock()
	response <- appRPCMessage{Result: json.RawMessage(`{"turn":{"id":"turn-1"}}`)}
	close(response)
	if err := <-done; !errors.Is(err, errCodexLeaseReplaced) {
		t.Fatalf("first processCommand error = %v, want lease replacement", err)
	}

	second := first
	second.ID = "wire-new"
	if err := b.processCommand("lease-new", second); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-writes.writes:
		t.Fatalf("redelivery executed a second app-server request: %s", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
	if hellos.Load() != 1 || acks.Load() != 2 {
		t.Fatalf("hello/ack counts = %d/%d, want 1/2", hellos.Load(), acks.Load())
	}
	b.mu.Lock()
	stillExecuted := b.executed["group-stable"]
	b.mu.Unlock()
	if stillExecuted {
		t.Fatal("stable execution receipt remained after fresh-lease ack")
	}
}

func TestMailboxClaimFreezesSemanticGroupAcrossRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := &Registry{
		contacts: map[string]*Contact{}, mailbox: map[string][]MailMessage{}, flushing: map[string]bool{},
	}
	const contactID = "mailbox-identity-contact"
	r.Queue(contactID, MailMessage{From: "Hrishi", Via: "phone", Text: "one"})
	r.Queue(contactID, MailMessage{From: "Hrishi", Via: "phone", Text: "two"})
	first := r.ClaimMailboxGroup(contactID)
	if len(first) != 2 || first[0].DeliveryID == "" || first[1].DeliveryID != first[0].DeliveryID {
		t.Fatalf("first claimed group = %#v", first)
	}
	persistedData, err := os.ReadFile(bridgePath("contacts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted registryFile
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatal(err)
	}
	persistedGroup := persisted.Mailbox[contactID]
	if len(persistedGroup) != 2 || persistedGroup[0].DeliveryID != first[0].DeliveryID ||
		persistedGroup[1].DeliveryID != first[0].DeliveryID {
		t.Fatalf("claimed identity was not persisted: %#v", persistedGroup)
	}

	r.Queue(contactID, MailMessage{From: "Hrishi", Via: "phone", Text: "three"})
	retry := r.ClaimMailboxGroup(contactID)
	if len(retry) != 2 || retry[0].DeliveryID != first[0].DeliveryID {
		t.Fatalf("retry group expanded or changed identity: %#v", retry)
	}
	r.DropMailbox(contactID, retry)
	next := r.ClaimMailboxGroup(contactID)
	if len(next) != 1 || next[0].Text != "three" || next[0].DeliveryID == first[0].DeliveryID {
		t.Fatalf("new mail did not form a fresh group: %#v", next)
	}
}

func TestSemanticEventDedupSurvivesDaemonHistoryReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := ensureBridgeDir(); err != nil {
		t.Fatal(err)
	}

	oldRegistry := registry
	registry = &Registry{
		contacts: map[string]*Contact{}, mailbox: map[string][]MailMessage{}, flushing: map[string]bool{},
	}
	const contactID = "event-dedup-contact"
	registry.contacts[contactID] = &Contact{ID: contactID, Name: "wise-deer", Status: "live", Health: "working"}

	eventsMu.Lock()
	oldEvents, oldCounter, oldAppends := events, eventCounter, historyAppends
	events, eventCounter, historyAppends = nil, 0, 0
	eventsMu.Unlock()
	t.Cleanup(func() {
		registry = oldRegistry
		eventsMu.Lock()
		events, eventCounter, historyAppends = oldEvents, oldCounter, oldAppends
		eventsMu.Unlock()
	})

	c := registry.Resolve(contactID)
	event := SemanticEvent{ID: "semantic-event-1", Type: SemanticEventAgentMessage, Text: "only once"}
	if !applySemanticEvent(c, event) {
		t.Fatal("first semantic event was not accepted")
	}
	eventsMu.Lock()
	events, eventCounter, historyAppends = nil, 0, 0
	eventsMu.Unlock()
	loadHistory() // simulate a fresh daemon restoring its durable receipt
	if !applySemanticEvent(c, event) {
		t.Fatal("retried semantic event was not accepted as an idempotent duplicate")
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	count := 0
	for _, stored := range events {
		if stored.SourceID == contactID+":"+event.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("durable semantic event count = %d, want 1", count)
	}
}

func TestSemanticEventPostingRetriesTransientFailure(t *testing.T) {
	var posts atomic.Int32
	postedIDs := make(chan string, 2)
	installTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/local/transport/v2/events" {
			t.Errorf("unexpected daemon path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Events []SemanticEvent `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Events) != 1 {
			t.Errorf("decode semantic event post: %v (%d events)", err, len(req.Events))
		} else {
			postedIDs <- req.Events[0].ID
		}
		if posts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"try-again"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"accepted":1}`))
	}))

	ctx, cancel := context.WithCancel(context.Background())
	b := &codexBridge{
		ctx: ctx, lease: "lease", contact: "contact", eventWake: make(chan struct{}, 1),
		retryDelay: 2 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() {
		b.postLoop()
		close(done)
	}()
	b.enqueueEvent(SemanticEvent{Type: SemanticEventAgentMessage, Text: "the final answer"})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.eventMu.Lock()
		empty := len(b.eventQueue) == 0
		b.eventMu.Unlock()
		if empty && posts.Load() >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	b.eventMu.Lock()
	remaining := len(b.eventQueue)
	b.eventMu.Unlock()
	if remaining != 0 || posts.Load() != 2 {
		t.Fatalf("post retries=%d remaining=%d, want 2/0", posts.Load(), remaining)
	}
	firstID, secondID := <-postedIDs, <-postedIDs
	if firstID == "" || secondID != firstID {
		t.Fatalf("retried event ids = %q/%q, want one stable non-empty id", firstID, secondID)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("post loop did not stop")
	}
}

func TestV2RemoteDeliveryParksSemanticCommand(t *testing.T) {
	const contactID = "semantic-contact"
	const token = "semantic-lease"
	remoteMu.Lock()
	remoteLeases[token] = &remoteLease{
		token: token, protocol: 2, lastSeen: time.Now(),
		agents: map[string]bool{contactID: true}, states: map[string]remoteState{},
	}
	remoteLeaseByContact[contactID] = token
	remoteMu.Unlock()
	defer func() {
		remoteMu.Lock()
		remoteDeleteLeaseLocked(token)
		remoteMu.Unlock()
	}()

	c := &Contact{ID: contactID, Name: "semantic-agent", Transport: "remote"}
	done := make(chan error, 1)
	go func() { done <- (remoteTransport{}).Deliver(c, "hello") }()

	deadline := time.Now().Add(time.Second)
	var parked *parkedDelivery
	for parked == nil && time.Now().Before(deadline) {
		remoteMu.Lock()
		if lease := remoteLeases[token]; lease != nil && len(lease.outbox) > 0 {
			parked = lease.outbox[0]
		}
		remoteMu.Unlock()
		if parked == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if parked == nil || parked.Command == nil {
		t.Fatal("v2 delivery did not park a semantic command")
	}
	if parked.Command.Type != SemanticCommandInput || parked.Command.Text != "hello" || parked.Text != "" {
		t.Fatalf("unexpected parked v2 delivery: %#v", parked)
	}

	remoteMu.Lock()
	parked.acked = true
	remoteRemoveParkedLocked(remoteLeases[token], parked.ID)
	remoteMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSemanticCompactParksTypedCommand(t *testing.T) {
	const contactID = "compact-contact"
	const token = "compact-lease"
	remoteMu.Lock()
	remoteLeases[token] = &remoteLease{
		token: token, protocol: 2, lastSeen: time.Now(),
		agents: map[string]bool{contactID: true}, states: map[string]remoteState{},
	}
	remoteLeaseByContact[contactID] = token
	remoteMu.Unlock()
	defer func() {
		remoteMu.Lock()
		remoteDeleteLeaseLocked(token)
		remoteMu.Unlock()
	}()

	c := &Contact{ID: contactID, Name: "compact-agent", Transport: "remote"}
	done := make(chan error, 1)
	go func() { done <- deliverCompact(c) }()

	deadline := time.Now().Add(time.Second)
	var parked *parkedDelivery
	for parked == nil && time.Now().Before(deadline) {
		remoteMu.Lock()
		if lease := remoteLeases[token]; lease != nil && len(lease.outbox) > 0 {
			parked = lease.outbox[0]
		}
		remoteMu.Unlock()
		if parked == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if parked == nil || parked.Command == nil || parked.Command.Type != SemanticCommandCompact {
		t.Fatalf("compact did not park a typed command: %#v", parked)
	}
	if parked.Command.Text != "" || parked.Text != "" {
		t.Fatalf("compact leaked literal input: %#v", parked)
	}

	remoteMu.Lock()
	parked.acked = true
	remoteRemoveParkedLocked(remoteLeases[token], parked.ID)
	remoteMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestV1MailboxDeliveryDoesNotExposeSemanticIdentity(t *testing.T) {
	const contactID = "v1-delivery-contact"
	const token = "v1-delivery-lease"
	remoteMu.Lock()
	remoteLeases[token] = &remoteLease{
		token: token, protocol: 1, lastSeen: time.Now(),
		agents: map[string]bool{contactID: true}, states: map[string]remoteState{},
	}
	remoteLeaseByContact[contactID] = token
	remoteMu.Unlock()
	defer func() {
		remoteMu.Lock()
		remoteDeleteLeaseLocked(token)
		remoteMu.Unlock()
	}()

	c := &Contact{ID: contactID, Name: "v1-agent", Transport: "remote"}
	done := make(chan error, 1)
	go func() { done <- deliverMailboxGroup(c, "hello", "durable-semantic-id") }()

	deadline := time.Now().Add(time.Second)
	var parked *parkedDelivery
	for parked == nil && time.Now().Before(deadline) {
		remoteMu.Lock()
		if lease := remoteLeases[token]; lease != nil && len(lease.outbox) > 0 {
			parked = lease.outbox[0]
		}
		remoteMu.Unlock()
		if parked == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if parked == nil || parked.Text != "hello" || parked.Command != nil {
		t.Fatalf("v1 parked delivery changed shape: %#v", parked)
	}

	remoteMu.Lock()
	parked.acked = true
	remoteRemoveParkedLocked(remoteLeases[token], parked.ID)
	remoteMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestV2AckRejectsLostLeaseWithoutChangingV1(t *testing.T) {
	body := []byte(`{"lease":"missing","ids":["delivery"]}`)
	v2 := httptest.NewRecorder()
	handleTransportAck(v2, httptest.NewRequest(http.MethodPost, "/local/transport/v2/ack", bytes.NewReader(body)))
	if v2.Code != http.StatusGone {
		t.Fatalf("v2 ack status = %d, want 410", v2.Code)
	}

	v1 := httptest.NewRecorder()
	handleTransportAck(v1, httptest.NewRequest(http.MethodPost, "/local/transport/ack", bytes.NewReader(body)))
	if v1.Code != http.StatusOK {
		t.Fatalf("v1 ack status = %d, want unchanged 200", v1.Code)
	}
}

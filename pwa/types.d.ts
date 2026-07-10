// Ambient types for the bridge PWA — a hand-written mirror of the daemon's
// /api/status JSON (httpapi.go handleStatus) and the SSE event shapes, plus the
// client-only shapes (the live `state` object and the outbox message).
//
// NO BUILD STEP: these power `// @ts-check` + editor/CI `tsc --noEmit` only.
// Nothing here is imported at runtime and nothing ships — the .js files are
// served verbatim from //go:embed. This file has no import/export on purpose, so
// the interfaces are GLOBAL and usable in JSDoc as `@param {Contact}` etc.
//
// When the daemon's JSON changes, update this file; `tsc` then flags every PWA
// read that drifted (the "hold_reason wired in Go, never read on the phone" class).

interface Contact {
  id: string;
  name: string;
  directory?: string;
  /** Always sent by the daemon for a real contact; absent on the client-built
      synthetic #crew row (renderList pushes {id,name,room:true} into the list). */
  status?: 'live' | 'offline';
  /** Client-only marker: renderList tags the synthetic room row so it renders via
      makeRoomRow. Never sent by the daemon. */
  room?: boolean;
  /** 'offline' rides along on both disconnect paths; status is the canonical
      presence signal, health mirrors it there (registry.go:23). */
  health?: 'ok' | 'working' | 'prompt' | 'offline';
  away?: string;
  /** The daemon's PromptOpen — a permission dialog is open on the agent's
      screen — packed by handleStatus into json:"attention" (httpapi.go). ONE
      field, one truth: there is no separate prompt_open key on this wire (that
      json tag exists only on /local/contacts, which the PWA never calls). This
      is the live authority pruneAttentions trusts over event history. */
  attention?: boolean;
  fields?: { status?: string; [k: string]: string | undefined };
  transport?: string;
  transport_flavor?: string;
  /** input+cache tokens ÷ model window, 0/omitted when unknown (context gauge). */
  context_pct?: number;
  /** Route-health L1: present ONLY when a route is genuinely stuck ('offline'
      = mail queued for a contact that is gone — the July-7 soft-wedge row). */
  hold_reason?: 'stale' | 'unconfirmed' | 'at-prompt' | 'busy' | 'stalled' | 'offline';
  /** Seconds since the route was last seen (route-health). Live remote rows:
      the lease's attest age. OFFLINE rows, best evidence first: the lingering
      stale lease's attest age (while it survives the lazy 10×TTL reap), else
      the Contact.LastSeen strike/connect stamp — so tmux offline rows carry an
      honest age too. Absent/0 = no evidence; the UI stays silent. */
  last_seen_s?: number;
}

interface Room {
  id: string;
  name: string;
}

/** A stored/streamed event (SSE /api/events + /api/history). Mirrors the Go
    Event struct (events.go) — it carries NO image/mstate fields (a phone photo
    is a 'sent' event whose text ends in "📷 photo"; those two live on the
    client-only PendingMsg/MessageLike shapes). */
interface BridgeEvent {
  /** Every STORED event carries id+ts; only the transient 'typing' frame
      (EmitTyping marshals just {type,agent,name}) arrives without them. */
  id?: number;
  type:
    | 'reply' | 'mention' | 'sent' | 'peer' | 'connected'
    | 'attention' | 'attention-clear' | 'approved' | 'compacted'
    | 'mystatus' | 'reaction' | 'status' | 'typing' | 'paper'
    | 'interrupted' | 'plugin';
  agent: string;
  name?: string;
  text?: string;
  ts?: string;
  /** reaction events: the target event id the emoji decorates. */
  target?: number;
  /** 'sent' events: the client-supplied id used to reconcile the outbox echo. */
  client_id?: string;
  /** 'sent' events: the quoted bubble this reply was composed against. */
  quote_name?: string;
  quote_excerpt?: string;
}

/** An outbox / local-echo message (client-only; persisted to localStorage). */
interface PendingMsg {
  clientId: string;
  agent: string;
  name: string;
  text: string;
  image?: string | null;
  quote?: Quote | null;
  ts: string;
  mstate: 'sending' | 'sent' | 'failed' | 'queued';
  inflight?: boolean;
}

/** {name, excerpt} — the bubble the composer is replying to. */
interface Quote {
  name?: string;
  excerpt?: string;
}

/** A message-like item for a row/preview: a stored event OR an outbox echo
    (newestMessage returns whichever is newer, so callers duck-type both). */
interface MessageLike {
  type?: BridgeEvent['type'];
  name?: string;
  text?: string;
  ts?: string;
  image?: string | null;
  mstate?: string;
}

/** The live client state object (app.js `export const state`). Every field the
    literal declares is mirrored here; if a module reads state.X, X lives here. */
interface State {
  contacts: Contact[];
  rooms: Room[];
  events: BridgeEvent[];
  attentions: Map<string, BridgeEvent>;
  reactions: Map<number, string[]>;
  myReactions: Map<number, Set<string>>;
  quote: Quote | null;
  /** #34 export: selection mode — taps toggle bubbles, gestures sleep. */
  selecting: boolean;
  /** #34 export: event ids checked for the PNG. */
  selectedIds: Set<number>;
  view: 'list' | 'thread';
  selected: string | null;
  focus: boolean;
  feedWindow: number;
  myStatus: string;
  lastEventId: number;
  lastSeen: { [contactId: string]: number };
  source: EventSource | null;
  typing: Map<string, number>;
  connected: boolean;
  pending: PendingMsg[];
  guidance: { agent: string; until: number } | null;
  lastContact: number | null;
  serverStarted: number | null;
  seenWake: number;
  wakeNote: { from?: number; to?: number; until: number } | null;
  wired: boolean;
  hydrated: boolean;
  /** Set true after the first /api/status; gates the "Mac was asleep" banner. */
  wakeSeenOnce?: boolean;
}

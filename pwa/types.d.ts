// Ambient types for the bridge PWA — a hand-written mirror of the daemon's
// /api/status JSON (httpapi.go handleStatus) and the SSE event shapes.
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
  status: 'live' | 'offline';
  health?: 'ok' | 'working' | 'prompt';
  prompt_open?: boolean;
  away?: string;
  fields?: { status?: string; [k: string]: string | undefined };
  transport?: string;
  transport_flavor?: string;
  /** input+cache tokens ÷ model window, 0/omitted when unknown (context gauge). */
  context_pct?: number;
  /** Route-health L1: present ONLY when a route is genuinely stuck. */
  hold_reason?: 'stale' | 'unconfirmed' | 'at-prompt' | 'busy' | 'stalled';
  /** Seconds since a remote route last attested (route-health, remote only). */
  last_seen_s?: number;
}

interface Room {
  id: string;
  name: string;
}

/** A stored/streamed event (SSE /api/events + /api/history). */
interface BridgeEvent {
  type:
    | 'reply' | 'mention' | 'sent' | 'peer' | 'connected'
    | 'attention' | 'compacted' | 'mystatus' | 'reaction' | 'status';
  agent: string;
  name?: string;
  text?: string;
  ts: string;
  image?: string;
  mstate?: string;
}

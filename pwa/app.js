// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — phone client.
   Talks to the daemon over REST + Server-Sent Events.

   This file is the native-ESM entry (index.html loads it with
   <script type="module">) AND the shared core. The peeled feature modules
   imported just below pull the helpers/state they need ($, state, api, …) back
   out of this core, and this core calls their entry points in turn. No build
   step ships: the browser loads app.js and each ./*.js import as plain
   same-origin files (see test/pwa/harness.js for how the tests bundle them). */

'use strict';

// Peeled feature modules (the ES-module split). Behaviour unchanged; they import
// shared helpers/state from this core, and this core calls back into them (a
// benign ESM cycle — every call is deferred to runtime, never eval-time).
import { armScreensaver, wakeScreensaver, isMomentEvent, initScreensaver } from './screensaver.js';
import { renderContextGauge } from './context-gauge.js';
// Appearance (theme / scenery / light drift): the module applies the saved
// look at import time (same startup point as before). The settings sheet's
// theme/wallpaper/palette setters moved with it into ./settings.js, so the spine
// now calls only applyPhase (the visibilitychange handler re-reads the light on
// wake); currentWallpaper is re-exported below for screensaver.js.
import { applyPhase } from './appearance.js';
// Pure text/time formatters (bubble markdown/linkify/thinking, bubble splitter,
// preview flattener, timestamp/date labels). Leaf module; we import the ones
// this core still calls directly.
import {
  richText, splitPleasing, plainPreview, firstLine, who,
  typingBubble, photoBox, appendStamp, localTime, dayLabel,
} from './textformat.js';
// Message & unread math (newest-message / activity / unread derivations off the
// event log + outbox). The list module imports the rest; the spine only calls
// markSeen (init / openThread / renderFeed). unreadCount rides into the list
// module (updateUnreadTotals lives there now).
import { markSeen, isDoubleTap, forwardText } from './messagemath.js';
// Notifications (browser Notification / ServiceWorker / PushManager + /api/push).
// The module wires its own enable-push listener at import time; the spine calls
// these four entry points back.
import {
  updatePushButton, requestNotifyPermission, maybeNotify, clearDeliveredNotifications,
} from './notifications.js';
// The settings sheet + Focus toggle. Self-contained (it wires its own listeners
// at import time and reaches nothing back out of the spine); a bare import pulls
// it into the graph so those side-effects run at the same startup point as before.
// settings.js imports renderList from this core (its toggleFocus re-renders the
// list) — the re-export below keeps that a core import, avoiding a settings↔list
// cycle. renderMyStatus lives in settings.js and the list module calls it.
import './settings.js';
// The conversation list (roster view). The spine calls renderList (every
// roster/status change) and updateUnreadTotals (the feed clears unread).
import { renderList, updateUnreadTotals } from './list.js';
// settings.js imports renderList from this core (its toggleFocus re-renders the
// list); re-export the binding so that import resolves here — keeping renderList
// a core import and avoiding a settings↔list cycle.
export { renderList };

export const $ = (id) => document.getElementById(id);   // exported: shared by the feature modules

// screensaver.js imports currentWallpaper from this core; re-export it from
// ./appearance.js (its new home) so that import stays byte-identical.
export { currentWallpaper } from './appearance.js';

/* ---------- appearance (theme / scenery / light drift) ----------
   Peeled into ./appearance.js (imported at the top of this file). That module
   applies the saved theme, wallpaper+fog, and palette phase at import time —
   the same startup point this block occupied — and exports the symbols the
   settings sheet + visibilitychange handler here still call (setTheme,
   setWallpaper, setPaletteSun, applyPhase, currentTheme/Wallpaper, THEMES, …). */

const HEALTH_LABELS = { working: 'working', prompt: 'waiting on you', offline: 'offline' };
const STATE_GLYPH = { sending: '🕐', sent: '✓', failed: '⚠️', queued: '📮' };

// The reaction whitelist — kept byte-identical to the daemon's reactionEmoji
// (httpapi.go), which 400s anything outside it.
const REACTIONS = ['👍', '❤️', '😂', '🎉', '👀', '🚀'];

// How many of a thread's newest events the feed paints on entry. A 400-message
// thread renders just this tail (instant); "↑ show earlier" widens the window a
// step at a time. Only the DOM is windowed — resolveAttentions still walks the
// FULL history, so an approval card resolved far below the fold still collapses
// (perf round, 2026-07-06).
const FEED_WINDOW = 60;

// Exported: the peeled feature modules read live app state through this object
// (screensaver: state.view; context-gauge: state.contacts/selected/view).
/** @type {State} */
export const state = {
  contacts: [],          // roster from /api/status
  rooms: [],             // shared threads from /api/status (v1: just #crew)
  events: [],            // chronological event list
  attentions: new Map(), // contact id -> latest unresolved attention event
  reactions: new Map(),  // target event id -> array of emoji (folded from 'reaction' events)
  myReactions: new Map(),// target event id -> Set of emoji THIS phone sent this session
  quote: null,           // {name, excerpt} — the bubble the composer is replying to
  view: 'list',          // 'list' (conversation list) | 'thread' (one contact)
  selected: null,        // contact id of the open thread, or null on the list
  focus: localStorage.getItem('focus') === '1', // list filter: only recent chats
  feedWindow: FEED_WINDOW, // newest events the open thread renders (grows on "show earlier")
  myStatus: '',          // the human's away line, delivered to agents on reach-out
  lastEventId: 0,
  lastSeen: JSON.parse(localStorage.getItem('lastSeen') || '{}'),
  source: null,          // EventSource
  typing: new Map(),     // contact id -> expiry ms; fed by transient events
  connected: false,      // SSE open / last request reached the bridge
  pending: [],           // local echoes + outbox (see loadPending)
  guidance: null,        // {agent, until} — a No was tapped; next message is the "do this instead"
  lastContact: null,     // ms of the last successful reach to the daemon (presence truth)
  serverStarted: null,   // daemon start unix (a change = it restarted)
  seenWake: 0,           // newest wake_to the phone has already surfaced
  wakeNote: null,        // {from,to,until} — transient "Mac was asleep X–Y" banner
  wired: false,          // one-time listeners/intervals installed (init re-runs)
  hydrated: false,       // eventCache replayed into state once (init re-runs)
};

/* Outbox: unsent/undelivered messages, persisted so they survive an app
   restart. A restored "sending" message is unconfirmed, so it reverts to
   "queued" until a flush retries it. */
loadPending();

/* ---------- rooms (party line) ----------
   A room is a shared thread everyone sees; v1 ships exactly one, #crew. It is
   not a contact — it comes from /api/status `rooms` and lives in its own
   "room:" id namespace, so every helper keyed on a string id (unreadCount,
   newestMessage, lastActivityMs, the lastSeen cursor) just works, while the
   contact-shaped lookups (health, presence, panes) are guarded to skip it. */
/** @param {string | null} id @returns {boolean} */
export function isRoomId(id) { return typeof id === 'string' && id.startsWith('room:'); }   // exported: context-gauge.js
/** @param {string | null} id @returns {Room | null} */
function roomById(id) { return state.rooms.find((r) => r.id === id) || null; }

// A thread target is legitimate if it's a known contact OR a known room. A
// notification tap deep-links with contact="room:crew", so navigation must
// validate against both (never trust an id from outside to pick a target).
/** @param {string | null} id @returns {boolean} */
function isValidTarget(id) {
  return state.contacts.some((c) => c.id === id) || state.rooms.some((r) => r.id === id);
}

// Display name for a thread id — a contact's name or a room's ("#crew"). Used
// wherever contactName() alone would return undefined for a room.
/** @param {string | null} id @returns {string | undefined} */
export function threadName(id) {   // exported: context-gauge.js
  if (isRoomId(id)) { const r = roomById(id); return (r && r.name) || '#crew'; }
  return contactName(id);
}

let reconnectDelay = 1000;   // capped exponential backoff for SSE
let reconnectTimer = null;

setInterval(() => {                 // expire stale typing bubbles
  const now = Date.now();
  let changed = false;
  for (const [id, until] of state.typing) {
    if (until < now) { state.typing.delete(id); changed = true; }
  }
  if (changed) { renderFeed(); renderList(); }
}, 2000);

/* ---------- bootstrap ---------- */

async function init() {
  // Replay the local cache into state before the first network round-trip, so a
  // warm open paints instantly and loadHistory only has to fetch the delta since
  // the cached cursor. Guarded (once): init re-runs on pairing and the offline
  // retry loop, but the cache must fold in exactly once.
  hydrateCache();
  const res = await fetch('/api/status').catch(() => null);
  if (!res) return showOffline();          // unreachable: show cached feed
  if (res.status === 401) return showPairing();
  const data = await res.json();
  state.contacts = data.contacts || [];
  state.rooms = data.rooms || [];
  state.myStatus = data.my_status || '';
  noteServerClocks(data);
  setConnected(true);
  showApp();
  await loadHistory();
  // With the roster and history loaded, land on the view named by the URL:
  // the list, or — cold-open from a notification tap (/?contact=<id> or
  // #/c/<id>) — straight into that contact's thread. restoreView validates
  // the id against the roster (a crafted id must not select a real target).
  restoreView();
  connectEvents();
  // One-time wiring, guarded: init() re-runs (showOffline retry loop, the
  // pair button), and unguarded it stacked a duplicate interval and listener
  // set per run — N× polling and N× handlers after a flaky morning.
  if (!state.wired) {
    state.wired = true;
    if ('serviceWorker' in navigator) {
      navigator.serviceWorker.register('/sw.js').catch(() => {});
      // Notification tap (app already open): the SW posts the contact to
      // open. Validate it against the live roster before selecting — never
      // trust a value from outside to pick the target agent.
      navigator.serviceWorker.addEventListener('message', (e) => {
        if (e.data && e.data.type === 'open-contact') selectContactIfValid(e.data.contact);
      });
    }
    setInterval(refreshStatus, 30000);
    // Stream liveness watchdog (H12): heartbeats are real events now, so a
    // healthy stream proves itself every 25s. 70s of silence (three missed
    // beats) on a non-closed source means the socket is a zombie — readyState
    // still says OPEN, nothing flows, onerror never fires. Replace it.
    setInterval(() => {
      if (!state.source || state.source.readyState === EventSource.CLOSED) return;
      if (Date.now() - (state.lastContact || 0) <= 70000) return;
      state.source.close();
      setConnected(false);
      scheduleReconnect();
    }, 15000);
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) {
        // A phone reopened hours later shouldn't still be wearing noon — read
        // the light for the current time before anything else repaints.
        applyPhase();
        // Waking from a sleep/background gap: a source that heard nothing for
        // 35s+ may be a zombie whose readyState still lies OPEN — recycle it
        // rather than trusting connectEvents' early-return (H12).
        if (state.source && Date.now() - (state.lastContact || 0) > 35000) {
          state.source.close();
        }
        refreshStatus(); connectEvents(); flushOutbox();
        clearDeliveredNotifications();   // you're looking at the app now
        // Returning to the foreground on an open thread clears its unread.
        if (state.view === 'thread' && state.selected) markSeen(state.selected);
        renderList();
        wakeScreensaver();   // #19(b): come back to a lit UI, re-arm the timer
      }
    });
    window.addEventListener('online', () => { connectEvents(); flushOutbox(); });
    window.addEventListener('offline', () => setConnected(false));
    initScreensaver();   // #19(b): wire the document-level idle detection (see ./screensaver.js)
  }
  clearDeliveredNotifications();
  updatePushButton();          // iOS only prompts on a tap, so offer a button
}

/* Server unreachable (laptop asleep, daemon restarting): show the last
   cached conversation read-only, and retry until the bridge is back. The cache
   was already folded in at the top of init(); hydrateCache is idempotent (its
   guard, and ingest's id guard, skip everything already seen), so this call is
   a no-op that keeps the read-only-offline path honest on its own. */
function showOffline() {
  hydrateCache();
  showApp();
  setConnected(false);
  setTimeout(init, 5000);
}

/* Persist enough to reopen warm AND to fetch only the delta next time:
   - events.slice(-300): the tail we replay into the feed offline.
   - reactions: the folded badge map. Reactions (and status) events FOLD into
     maps and never enter state.events, so they live ONLY in server history.
     Once loadHistory stops replaying from since=0, a reload would lose every
     badge older than the delta unless the map itself rides the cache — this
     field is that lifeline. (Status/away needs no caching: it re-arrives on the
     first /api/status fetch.)
   - lastEventId: the SSE/history cursor. It can exceed the last cached event's
     id — a reaction/status folded and advanced the cursor without being stored
     — so it is cached explicitly rather than re-derived from events. */
function cacheEvents() {
  try {
    localStorage.setItem('eventCache', JSON.stringify({
      contacts: state.contacts,
      rooms: state.rooms,
      events: state.events.slice(-300),
      reactions: [...state.reactions],
      lastEventId: state.lastEventId,
    }));
  } catch (e) { /* storage full — cache is best-effort */ }
}

/* Fold the persisted cache back into state, exactly once. Called at the top of
   init() (which re-runs) and again from showOffline(); the hydrated guard makes
   the second call a no-op. Ingesting with the SAME outbox reconcile loadHistory
   does keeps a cached 'sent' from re-delivering its local echo. */
function hydrateCache() {
  if (state.hydrated) return;
  state.hydrated = true;
  const cached = JSON.parse(localStorage.getItem('eventCache') || 'null');
  if (!cached) return;                       // cold start: nothing cached
  state.contacts = cached.contacts || [];
  state.rooms = cached.rooms || [];
  (cached.events || []).forEach((e) => {
    // Same reconcile as loadHistory's replay (H10): a cached 'sent' the phone
    // never saw live must still drop its outbox echo, or flushOutbox re-sends it.
    if (ingest(e) && e.type === 'sent') dropPendingEcho(e.agent, e.text, e.client_id);
  });
  // Reactions folded out of state.events at cache time — restore the badge map
  // from the cached Map entries so pre-delta reactions survive the reload.
  if (cached.reactions) state.reactions = new Map(cached.reactions);
  // The cursor can sit past the newest cached event (a folded reaction/status
  // advanced it), so trust the cached cursor over the ids ingest just replayed.
  state.lastEventId = Math.max(state.lastEventId, cached.lastEventId || 0);
}

function showPairing() {
  $('pair-screen').classList.remove('hidden');
  $('app').classList.add('hidden');
}

function showApp() {
  $('pair-screen').classList.add('hidden');
  $('app').classList.remove('hidden');
  showList();   // default view; init()/restoreView() may then open a thread
}

/* ---------- pairing ---------- */

$('pair-btn').addEventListener('click', async () => {
  const code = /** @type {HTMLInputElement} */ ($('pair-code')).value.trim();
  const res = await fetch('/api/pair', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code, device: navigator.userAgent.slice(0, 120) }),
  }).catch(() => null);
  if (res && res.ok) {
    $('pair-error').classList.add('hidden');
    init();
  } else {
    $('pair-error').classList.remove('hidden');
  }
});

/* ---------- data ---------- */

async function refreshStatus() {
  const res = await fetch('/api/status').catch(() => null);
  // A revoked or missing device token is an auth problem, not a dead Mac:
  // route back to pairing instead of showing "unreachable" forever.
  if (res && res.status === 401) return showPairing();
  if (!res || !res.ok) return setConnected(false);
  const data = await res.json();
  const wasOffline = new Map(state.contacts.map((c) => [c.id, c.status === 'offline']));
  state.contacts = data.contacts || [];
  state.rooms = data.rooms || [];
  state.myStatus = data.my_status || '';
  noteServerClocks(data);
  setConnected(true);
  // A contact that just came back to life can receive its queued messages.
  const revived = state.contacts.some(
    (c) => c.status !== 'offline' && wasOffline.get(c.id));
  if (revived) flushOutbox();
  pruneAttentions();
  renderList();
  if (state.view === 'thread') updateThreadHeader();
  updateAttentionBanner();
}

async function loadHistory() {
  // Fetch only what's newer than the cached cursor — a warm open pulls the
  // delta, not all 500 events. A cold start (no cache) leaves lastEventId at 0,
  // so this naturally becomes since=0, the full history.
  const res = await fetch('/api/history?since=' + state.lastEventId).catch(() => null);
  if (!res || !res.ok) return;
  const data = await res.json();
  (data.events || []).forEach((e) => {
    // Reconcile the outbox against the replay exactly like the live path
    // (H10): a 'sent' we never saw live (SSE died mid-send) must still drop
    // its local echo, or the next flushOutbox re-delivers it — and after a
    // daemon restart the client_id ring is empty, so the retry types into
    // the pane a second time.
    if (ingest(e) && e.type === 'sent') dropPendingEcho(e.agent, e.text, e.client_id);
  });
  pruneAttentions();   // history replay can resurrect orphaned attentions
  cacheEvents();
  renderFeed();
  // A replay landing on an already-open thread (an offline-retry re-init) can
  // backfill content under the fold after renderFeed's synchronous stick; hold
  // the bottom as it settles — guarded, so only for a reader already at the end.
  if (state.view === 'thread' && state.selected) pinFeedBottom();
}

// Roster truth beats history replay. An attention whose clear never got
// emitted (a desk-answered prompt from before the daemon re-verified them)
// survives in history forever; the daemon's live PromptOpen flag is the
// authority on who needs approval RIGHT NOW. Prune anything the roster
// disowns so a banner can never outlive its prompt.
function pruneAttentions() {
  let changed = false;
  for (const id of [...state.attentions.keys()]) {
    const c = state.contacts.find((x) => x.id === id);
    if (!c || !c.attention) { state.attentions.delete(id); changed = true; }
  }
  if (changed) { updateAttentionBanner(); renderList(); }
}

function connectEvents() {
  if (state.source && state.source.readyState !== EventSource.CLOSED) return;
  clearTimeout(reconnectTimer);
  const source = new EventSource('/api/events?since=' + state.lastEventId);
  state.source = source;
  state.lastContact = Date.now();   // fresh socket gets its full liveness grace
  // Server heartbeat (every 25s, a named event so old clients ignore it):
  // proof the socket is genuinely alive, consumed by the H12 watchdog.
  source.addEventListener('hb', () => {
    state.lastContact = Date.now();
    setConnected(true);
  });
  source.onopen = () => {
    reconnectDelay = 1000;   // healthy link — reset the backoff
    state.lastContact = Date.now();
    setConnected(true);
    refreshStatus();         // roster may have changed while we were away
    flushOutbox();           // drain anything queued during the outage
  };
  // Drive reconnection ourselves (capped backoff) rather than leaning on the
  // browser's opaque retry, so we can refresh status and flush on each try.
  source.onerror = () => {
    setConnected(false);
    source.close();
    scheduleReconnect();
  };
  source.onmessage = (msg) => {
    state.lastContact = Date.now();   // any frame proves the Mac is reachable
    const event = JSON.parse(msg.data);
    if (event.type === 'typing') {          // transient: never stored
      state.typing.set(event.agent, Date.now() + 6000);
      renderFeed();
      renderList();          // surface "typing…" in the contact's row preview
      return;
    }
    const added = ingest(event);
    // Drop the local echo only once the server's own 'sent' event has been
    // accepted for render — otherwise a deduped event would remove the echo
    // and leave no bubble at all.
    if (added && event.type === 'sent') dropPendingEcho(event.agent, event.text, event.client_id);
    cacheEvents();
    renderFeed();
    renderList();
    if (state.view === 'thread') updateThreadHeader();
    updateAttentionBanner();
    maybeNotify(event);
    // #19(b): a new attention card or message must never be eaten by the
    // screensaver — wake the UI (also re-arms the idle countdown).
    if (added && isMomentEvent(event)) wakeScreensaver();
  };
}

function scheduleReconnect() {
  clearTimeout(reconnectTimer);
  reconnectTimer = setTimeout(connectEvents, reconnectDelay);
  reconnectDelay = Math.min(reconnectDelay * 2, 15000);
}

/** @param {BridgeEvent} event @returns {boolean} — true when the event was pushed to the feed */
function ingest(event) {
  if (event.id <= state.lastEventId) return false;
  state.lastEventId = event.id;
  // Away statuses are roster state, not thread messages: fold a 'status' into
  // its contact and a 'mystatus' into state.myStatus, then stop before the feed.
  // They are durable (Emit, so a reconnect's history replay reaches here too),
  // but must render like a transient roster update — never a bubble (as 'typing'
  // does in connectEvents). The live onmessage path re-renders the list and
  // thread header right after ingest, so this alone keeps the phone in sync.
  if (event.type === 'status') {
    const c = state.contacts.find((x) => x.id === event.agent);
    if (c) c.away = event.text;
    return false;
  }
  if (event.type === 'mystatus') {
    state.myStatus = event.text;
    return false;
  }
  // Reactions are decorations, not thread bubbles: fold the emoji onto the
  // target event's badge list and stop before the feed (like 'status'). Durable
  // (Emit), so a reconnect's history replay rebuilds the map here too.
  if (event.type === 'reaction') {
    const arr = state.reactions.get(event.target) || [];
    if (!arr.includes(event.text)) { arr.push(event.text); state.reactions.set(event.target, arr); }
    return false;
  }
  state.events.push(event);
  if (event.type === 'attention') {
    state.attentions.set(event.agent, event);
  } else if (event.type === 'attention-clear' || event.type === 'approved') {
    state.attentions.delete(event.agent);
  }
  if (event.type === 'reply' || event.type === 'mention') {
    state.typing.delete(event.agent);   // the reply arrived; stop the dots
  }
  return true;
}

/** @param {boolean} on */
function setConnected(on) {
  state.connected = !!on;
  // Both view headers carry a connection dot; keep them in lockstep.
  document.querySelectorAll('.dot').forEach((d) => d.classList.toggle('on', !!on));
  updateBanner();
}

/* Slim status banner under the header. Hidden when the bridge is reachable;
   distinguishes "phone has no network" from "phone online, Mac asleep". */
function updateAttentionBanner() {
  const el = $('attn-banner');
  if (!el) return;
  // The banner lives in the thread view; on the list, the row accent + preview
  // override carry the same signal, so keep it hidden there.
  if (state.view !== 'thread') { el.classList.add('hidden'); return; }
  // the first unresolved attention that isn't the contact you're looking at
  let target = null;
  for (const [id, ev] of state.attentions) {
    if (id !== state.selected) { target = { id, name: ev.name }; break; }
  }
  if (target) {
    el.textContent = '🔔 ' + (target.name || 'an agent') + ' needs your approval →';
    el.classList.remove('hidden');
    el.onclick = () => navigateToThread(target.id);
  } else {
    el.classList.add('hidden');
  }
}

/* Record the daemon's clocks from a /api/status payload (round 4 presence
   truth). Marks the reach time, notices a restart (started changed), and
   surfaces a fresh sleep window once as a transient banner. */
/** @param {any} data — the /api/status JSON (res.json() is untyped) */
function noteServerClocks(data) {
  if (!data) return;
  state.lastContact = Date.now();
  if (data.started) {
    if (state.serverStarted && data.started !== state.serverStarted) {
      // The daemon restarted since we last looked — reset the SSE cursor so we
      // re-sync from history rather than assuming continuity.
      state.serverStarted = data.started;
    } else if (!state.serverStarted) {
      state.serverStarted = data.started;
    }
  }
  if (data.wake_to && data.wake_to > state.seenWake) {
    state.seenWake = data.wake_to;
    // Don't cry wolf on the very first load (no prior contact to have missed).
    if (state.lastContact && state.wakeSeenOnce) {
      state.wakeNote = { from: data.wake_from, to: data.wake_to, until: Date.now() + 8000 };
      setTimeout(() => { state.wakeNote = null; updateBanner(); }, 8200);
    }
  }
  state.wakeSeenOnce = true;
}

// "HH:MM" in the phone's locale from a unix-seconds timestamp.
/** @param {number} [sec] @returns {string} */
function clockUnix(sec) {
  const d = new Date((sec || 0) * 1000);
  // isNaN(Date) coerces via Number(d) at runtime; the cast is inert (a comment)
  // and keeps that behaviour byte-identical — no d.getTime() logic change.
  return isNaN(/** @type {?} */ (d)) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function updateBanner() {
  const banner = $('conn-banner');
  const wn = state.wakeNote;
  if (wn && wn.until > Date.now()) {
    // Just reconnected after the Mac slept — say so, briefly, over any state.
    banner.textContent = '😴 Mac was asleep ' + clockUnix(wn.from) + '–' + clockUnix(wn.to) + ' — back now';
    banner.className = 'banner';
    return;
  }
  if (state.connected) {
    banner.className = 'banner hidden';
  } else if (!navigator.onLine) {
    banner.textContent = '📴 You’re offline';
    banner.className = 'banner offline';
  } else if (state.lastContact) {
    // Reachability, not just "unreachable": name when we last had the Mac, so a
    // sleeping/asleep laptop reads as a gap since HH:MM rather than a dead app.
    banner.textContent = '⚠️ Mac unreachable since ' + clockUnix(state.lastContact / 1000) + ' — retrying…';
    banner.className = 'banner';
  } else {
    banner.textContent = '⚠️ Mac unreachable — retrying…';
    banner.className = 'banner';
  }
}

/* ---------- message & unread math ----------
   Peeled into ./messagemath.js (imported at the top of this file): newestMessage,
   lastActivityMs, myLastSentMs, lastMessageMs, unreadCount, markSeen. The spine
   still calls markSeen (init / openThread / renderFeed) and unreadCount
   (updateUnreadTotals), imported back at the top; the list module imports the
   rest. */

/* ---------- navigation: list ↔ thread ----------

   Two screens, iMessage-style. history.pushState/popstate drives it so the
   platform back gesture (iOS swipe-back, Android back button) just works:
   the list sits at #/, a thread at #/c/<contactId>. openThread/showList do
   the DOM swap; navigateToThread pushes history; routeFromLocation replays
   the URL (refresh, back/forward). */

function showList() {
  state.view = 'list';
  state.selected = null;
  document.documentElement.setAttribute('data-view', 'list');   // scenery on
  $('list-view').classList.remove('hidden');
  $('thread-view').classList.add('hidden');
  renderList();
  updateAttentionBanner();   // hidden on the list — rows carry the signal
  armScreensaver();          // #19(b): the list is the only view with scenery
}

/** @param {string} id */
function openThread(id) {
  state.view = 'thread';
  state.selected = id;
  document.documentElement.setAttribute('data-view', 'thread');  // scenery off
  $('thread-view').classList.remove('hidden');
  $('list-view').classList.add('hidden');
  if (!document.hidden) markSeen(id);
  clearDeliveredNotifications();
  updateThreadHeader();
  updateAttentionBanner();
  // Every entry lands at the newest message, so the window resets to the tail —
  // a thread you scrolled way back in last time reopens light, not wherever its
  // "show earlier" left off.
  state.feedWindow = FEED_WINDOW;
  renderFeed();
  // Entering a thread always lands on the newest message. renderFeed's sticky
  // logic only holds the bottom once you're already there; on entry the feed
  // may carry the previous thread's scroll position — and thumbs shouldn't
  // pay for history they didn't ask to read (field report, 2026-07-06).
  // pinFeedBottom re-pins as late content (a photo's <img>, a font swap)
  // inflates the feed under the fold, so the landing holds instead of drifting.
  pinFeedBottom(true);
  restoreDraft();
  state.quote = null;   // a quote is a reply to THIS thread; don't carry it across
  renderQuoteChip();
  wakeScreensaver();    // #19(b): a thread has no scenery — cancel the idle timer
}

// Enter a thread and push it onto history, so back pops to the list.
/** @param {string} id */
export function navigateToThread(id) {   // exported: list.js rows navigate through it
  if (state.view === 'thread' && state.selected === id) { openThread(id); return; }
  history.pushState({ view: 'thread', id }, '', '#/c/' + encodeURIComponent(id));
  openThread(id);
}

// Land on the view named by the URL — used by back/forward (popstate).
function routeFromLocation() {
  const m = (location.hash || '').match(/^#\/c\/(.+)$/);
  const id = m ? decodeURIComponent(m[1]) : null;
  if (id && isValidTarget(id)) openThread(id);
  else showList();
}

// Establish the history baseline once the roster is known: a list entry always
// sits under any thread, so back (button or swipe) returns to the list rather
// than leaving the app — even on a cold-open deep-link straight to a thread.
// The id (from #/c/<id> or the legacy ?contact= param) is honoured only if it
// is in the live roster.
function restoreView() {
  const m = (location.hash || '').match(/^#\/c\/(.+)$/);
  const param = new URLSearchParams(location.search).get('contact');
  const id = m ? decodeURIComponent(m[1]) : param;
  history.replaceState({ view: 'list' }, '', '#/');
  if (id && isValidTarget(id)) navigateToThread(id);
  else showList();
}

window.addEventListener('popstate', routeFromLocation);

// Notification deep-link (SW postMessage, app already open). Validate the id
// against the roster so an unknown/crafted value can't select a real target.
/** @param {string} id */
function selectContactIfValid(id) {
  if (id && isValidTarget(id)) navigateToThread(id);
}

$('back-btn').addEventListener('click', () => history.back());

/* Interrupt: one tap → the daemon sends the agent a bare Escape (stop
   mid-thought). Held burst messages are then delivered immediately, so
   "stop — do this instead" arrives in the right order. */
$('stop-btn').addEventListener('click', async () => {
  if (!state.selected) return;
  const btn = /** @type {HTMLButtonElement} */ ($('stop-btn'));
  btn.disabled = true;
  await api('/api/interrupt', { agent: state.selected });
  btn.disabled = false;
});

/* ---------- feature #19(b): idle screensaver ----------
   Peeled into ./screensaver.js (imported at the top of this file). This core
   calls armScreensaver / wakeScreensaver / isMomentEvent / initScreensaver; the
   module reads state.view + currentWallpaper() to decide eligibility. */

/* ---------- settings sheet ----------
   Peeled into ./settings.js (imported at the top of this file): the gear's
   slide-up sheet (theme / wallpaper / palette-sun / notifications / my-status)
   and the list-header Focus toggle, plus all their event listeners. That module
   imports the appearance setters, the notification entry points, and $/state/
   renderList from here; renderMyStatus lives there and the list module calls it.
   The spine imports nothing back — the sheet is self-contained. */

/* ---------- conversation list ----------
   Peeled into ./list.js (imported at the top of this file): listSort, applyFocus
   (Focus filter), renderList, the contact/#crew rows + their previews, the
   avatar monogram/colour, the #18 transport badge, and updateUnreadTotals. That
   module imports the message/unread math, the preview flattener + list time,
   renderMyStatus, and $/state/isRoomId/navigateToThread/routeHold from here; the
   spine calls renderList and updateUnreadTotals back. routeHold + humanizeAgo
   stay here (threadStatusText also uses routeHold). */

// Route Health L1 (daemon-derived, docs/route-health.md): a human phrase for a
// route whose mail is queued+blocked, or "" when healthy (hold_reason absent —
// also the case on an older daemon, so the UI degrades cleanly). 'stale' means
// the remote client stopped attesting and carries the last-seen age; the others
// mean mail is genuinely held. Composed per context: previewFor prefixes "⚠ ",
// threadStatusText joins it with " · ".
/** @param {Contact} contact @returns {string} */
export function routeHold(contact) {   // exported: list.js previewFor uses it (humanizeAgo stays here too)
  const r = contact.hold_reason;
  if (!r) return '';
  if (r === 'stale') return 'stale — last seen ' + humanizeAgo(contact.last_seen_s);
  const lbl = { 'at-prompt': 'at a prompt', busy: 'busy',
                unconfirmed: 'unconfirmed', stalled: 'stuck',
                offline: 'mail waiting' }[r] || r;   // offline = queued for someone who's gone (review C7)
  return 'held — ' + lbl;
}

// Compact age string ("45s" / "12m" / "3h" / "2d") from a seconds count.
/** @param {number} [s] @returns {string} */
function humanizeAgo(s) {
  s = Math.max(0, s | 0);
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

export function updateThreadHeader() {   // exported: context-gauge.js doCompact() re-renders through it
  // A room has no presence, health, or pane: fixed header "#crew · everyone",
  // and no interrupt button (there is nothing to Escape).
  if (isRoomId(state.selected)) {
    const r = roomById(state.selected);
    $('thread-name').textContent = (r && r.name) || '#crew';
    $('thread-status').textContent = 'everyone';
    $('stop-btn').classList.add('hidden');
    renderContextGauge(null);   // ── context gauge ── rooms have no bar
    return;
  }
  const c = state.contacts.find((x) => x.id === state.selected);
  $('thread-name').textContent = (c && c.name) || 'contact';
  $('thread-status').textContent = threadStatusText(c);
  // Interrupt is offered whenever the contact is live — health lags the
  // roster refresh, and "stop mid-thought" can't wait for it to catch up.
  $('stop-btn').classList.toggle('hidden', !c || c.status === 'offline');
  renderContextGauge(c);   // ── context gauge ── ≥70% only; enabled iff idle
}

/** @param {Contact | undefined} contact @returns {string} */
function threadStatusText(contact) {
  if (!contact) return '';
  let s = 'live';
  if (contact.status === 'offline') {
    s = 'offline';
  } else {
    const h = HEALTH_LABELS[contact.health];
    if (h) s += ' · ' + h;
  }
  // Route Health L1: the honest delivery state, after presence/health.
  const hold = routeHold(contact);
  if (hold) s += ' · ' + hold;
  // An offline row's honest clock (review C7): the daemon sends last_seen_s
  // from the lingering lease's true attest age when it has one — real evidence,
  // so show it; absent, stay silent rather than guessing off hello-time.
  if (contact.status === 'offline' && contact.last_seen_s) {
    s += ' · last seen ' + humanizeAgo(contact.last_seen_s);
  }
  // The away line rides under the name, after presence/health.
  if (contact.away) s += ' · 💬 ' + contact.away;
  return s;
}

/* ── context gauge ───────────────────────────────────────────────────────────
   Peeled into ./context-gauge.js (imported at the top of this file). This core
   calls renderContextGauge from updateThreadHeader; the module imports
   $, state, isRoomId, threadName, api and updateThreadHeader back from here to
   render the bar and run the compact confirm/POST flow. */
/* ── end context gauge ─────────────────────────────────────────────────────*/

/* ---------- feed ---------- */

function visibleEvents() {
  return state.events.filter((e) => e.agent === state.selected);
}

function visiblePending() {
  return state.pending.filter((m) => m.agent === state.selected);
}

function renderFeed() {
  if (state.view !== 'thread' || !state.selected) return;   // feed is hidden
  const feed = $('feed');
  const stick = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 60;
  feed.innerHTML = '';
  const all = visibleEvents();
  // resolveAttentions walks the FULL thread: a prompt's resolver (approve /
  // clear / a superseding attention) can land many events after it, past the
  // top edge of the window, and the card must still collapse.
  const resolutions = resolveAttentions(all);   // attn event id -> resolution
  // The DOM, though, only carries the newest slice — a 400-message thread paints
  // its tail instantly. Older events wait behind a "show earlier" row at the top.
  const events = all.slice(-state.feedWindow);
  if (events.length < all.length) feed.appendChild(showEarlierButton(all.length - events.length));
  let lastDay = '';   // day separators compute over the rendered window only
  for (const event of events) {
    const day = (event.ts || '').slice(0, 10);   // group by calendar day
    if (day && day !== lastDay) {
      lastDay = day;
      const sep = document.createElement('div');
      sep.className = 'day-sep';
      sep.textContent = dayLabel(event.ts);       // Today / Yesterday / date
      feed.appendChild(sep);
    }
    let res = resolutions.get(event.id);
    if (!res && event.type === 'attention' && !state.attentions.has(event.agent)) {
      // Orphaned: no clear event survived, but the attention is no longer
      // live. state.attentions is the authority here — event-stream truth,
      // pruned only against a FRESHLY fetched roster (pruneAttentions). The
      // old check read state.contacts directly, and that snapshot can be 30s
      // stale: a brand-new prompt rendered "✓ Resolved", buttons missing,
      // until the next poll (H11).
      res = { kind: 'cleared', ts: null };
    }
    feed.appendChild(renderEvent(event, res));
  }
  for (const msg of visiblePending()) feed.appendChild(renderPending(msg));
  const now = Date.now();
  for (const [id, until] of state.typing) {
    if (until < now) continue;
    if (state.selected !== id) continue;
    feed.appendChild(typingBubble(contactName(id) || 'contact'));
  }
  if (stick) feed.scrollTop = feed.scrollHeight;
  // New events arriving while the thread is open and visible clear its unread.
  if (!document.hidden) { markSeen(state.selected); updateUnreadTotals(); }
  const g = state.guidance;
  /** @type {HTMLInputElement} */ ($('msg-input')).placeholder = (g && g.agent === state.selected && g.until > Date.now())
    ? 'Tell ' + (threadName(state.selected) || 'the agent') + ' what to do differently…'
    : 'Message ' + (threadName(state.selected) || 'contact') + '…';
  /** @type {HTMLButtonElement} */ ($('send-btn')).disabled = false;
  // Photos are a 1:1 feature in v1 — the room upload path has no fan-out, so a
  // photo to #crew would stick queued. Disable the attach button in a room.
  /** @type {HTMLButtonElement} */ ($('attach-btn')).disabled = isRoomId(state.selected);
}

/* The "↑ show earlier" row at the top of a windowed feed. Widening the window
   re-renders taller, so we pin the viewport to the messages the reader was
   already looking at: record the height, grow, re-render, then push scrollTop
   down by exactly how much taller it got. The reader sits at the top when this
   fires (the button IS the top), so renderFeed's near-bottom stick can't trip,
   and pinFeedBottom never runs from here — the view holds where the eye is. */
/** @param {number} n @returns {HTMLElement} */
function showEarlierButton(n) {
  const btn = document.createElement('button');
  btn.className = 'show-earlier';
  btn.textContent = '↑ show earlier (' + n + ' more)';
  btn.onclick = () => {
    const feed = $('feed');
    const before = feed.scrollHeight;
    state.feedWindow += FEED_WINDOW;
    renderFeed();
    feed.scrollTop += feed.scrollHeight - before;
  };
  return btn;
}

/* Land the feed on its newest message — and KEEP it there as late content
   settles. The pin is easily undone a beat later: a photo bubble's <img>
   decodes after layout, a webfont swaps in — each grows scrollHeight below the
   fold and slides the true bottom out from under the landing. So we re-pin
   across the three windows where that inflation shows up: the next two frames
   (layout), each <img>'s load (decode), and one 300ms backstop.
   `land` forces the first pin — entering a thread always drops you at the
   bottom. Without it (a history replay onto an already-open thread) we only
   re-pin when already near the bottom, so we hold a reader parked at the end
   without yanking one who scrolled up. Every DEFERRED re-pin honours that
   near-bottom guard regardless — 60px, renderFeed's own stick threshold — so a
   settle that lands mid-scroll never steals the view (field report, 2026-07-06). */
/** @param {boolean} [land] */
function pinFeedBottom(land) {
  const feed = $('feed');
  const nearBottom = () =>
    feed.scrollHeight - feed.scrollTop - feed.clientHeight < 60;
  const pin = () => { feed.scrollTop = feed.scrollHeight; };
  const settle = () => { if (nearBottom()) pin(); };
  if (land) pin();
  requestAnimationFrame(() => requestAnimationFrame(land ? pin : settle));
  // One-shot per image still decoding: its height lands after this frame, and a
  // photo can grow the fold well past where we just parked.
  feed.querySelectorAll('img').forEach((img) => {
    if (!img.complete) img.addEventListener('load', settle, { once: true });
  });
  setTimeout(settle, 300);
}

/** @param {string | null} id @returns {string | undefined} */
function contactName(id) {
  const contact = state.contacts.find((c) => c.id === id);
  return contact && contact.name;
}

/** @param {BridgeEvent} event @param {{kind:string, ts:string|null, key?:string}} [resolution] @returns {Node} */
function renderEvent(event, resolution) {
  if (event.type === 'attention') return attentionCard(event, resolution);
  const el = document.createElement('div');
  if (event.type === 'sent') {
    el.className = 'msg sent';
    el.appendChild(who('you → ' + (event.name || '?')));
    if (event.quote_name || event.quote_excerpt) {
      el.appendChild(quoteInset(event.quote_name, event.quote_excerpt));
    }
    el.appendChild(richText(event.text));
    appendStamp(el, event.ts);
  } else if (event.type === 'reply') {
    return replyBubbles(event, 'msg reply', who(event.name || '?'));
  } else if (event.type === 'mention') {
    return replyBubbles(event, 'msg mention', who((event.name || 'contact') + ' · @mention'));
  } else if (event.type === 'peer') {
    // Agent-to-agent (switchboard) message in this thread: it wears its
    // AUTHOR — event.name is the sender, event.agent the thread it landed in.
    // In a room the bubble is just its author ("marvin"); a 1:1 peer keeps the
    // routing arrow ("marvin → recipient", the thread it was relayed into).
    const author = event.name || 'agent';
    const label = isRoomId(event.agent) ? author
      : author + ' → ' + (contactName(event.agent) || 'agent');
    return replyBubbles(event, 'msg peer', who(label));
  } else if (event.type === 'paper') {
    // The Bridge Herald: the daemon's morning edition, a small newspaper in
    // the #crew thread. Masthead in serif (the wordmark already spans the
    // coasts), ALL-CAPS lines become section heads, bullets become copy.
    el.className = 'msg paper';
    const mast = document.createElement('div');
    mast.className = 'paper-masthead';
    mast.textContent = event.name || 'The Bridge Herald';
    el.appendChild(mast);
    const body = document.createElement('div');
    body.className = 'paper-body';
    for (const ln of (event.text || '').split('\n')) {
      if (!ln.trim()) continue;
      const div = document.createElement('div');
      div.className = /^[A-Z][A-Z '&]+$/.test(ln.trim()) ? 'paper-section' : 'paper-line';
      div.textContent = ln;
      body.appendChild(div);
    }
    el.appendChild(body);
    appendStamp(el, event.ts);
  } else if (event.type === 'interrupted') {
    el.className = 'msg system';
    el.textContent = '⏹ interrupted' + (localTime(event.ts) ? ' · ' + localTime(event.ts) : '');
  } else {
    el.className = 'msg system';
    el.textContent = (event.name || '') + ' · ' + event.type +
      (event.text ? ' · ' + event.text : '');
  }
  return el;
}

/* Agents write essays; phones deserve conversation. A long reply renders as
   several consecutive bubbles split at pleasing boundaries — paragraphs,
   never inside code fences or [thinking] blocks — and iMessage-grouped:
   tight spacing, who-line only on the first, timestamp only on the last.
   Render-time only: one event on the wire stays one event, so history,
   pushes and dedup are untouched (and old walls of text improve
   retroactively). */
/** @param {BridgeEvent} event @param {string} cls @param {Node} whoEl @returns {Node} */
function replyBubbles(event, cls, whoEl) {
  const parts = splitPleasing(event.text || '');
  // Long-press metadata for the action sheet: the whole event is the reaction
  // target (its id), and the pressed part's own text seeds a quote.
  const meta = (text) => ({ id: event.id, name: event.name || 'agent', type: event.type, text });
  if (parts.length <= 1) {
    const el = document.createElement('div');
    el.className = cls;
    el.appendChild(whoEl);
    el.appendChild(richText(event.text));
    appendStamp(el, event.ts);
    attachBubbleActions(el, meta(event.text || ''));
    appendReactions(el, event.id);
    return el;
  }
  const frag = document.createDocumentFragment();
  parts.forEach((p, i) => {
    const el = document.createElement('div');
    el.className = cls + (i === 0 ? ' grp-first' : (i === parts.length - 1 ? ' grp-last' : ' grp-mid'));
    if (i === 0) el.appendChild(whoEl);
    el.appendChild(richText(p));
    // Reactions decorate the whole message: badge only the last bubble (its
    // bottom edge is the message's bottom edge).
    if (i === parts.length - 1) { appendStamp(el, event.ts); appendReactions(el, event.id); }
    attachBubbleActions(el, meta(p));
    frag.appendChild(el);
  });
  return frag;
}

/* Local echo for an outgoing message. Its delivery state (sending / sent /
   failed / queued) shows as a glyph in the who-line; failed messages get a
   retry button that re-sends with the same client_id (safe — the server
   dedups). Replaced by the server's own "sent" event when it arrives. */
/** @param {PendingMsg} msg @returns {HTMLElement} */
function renderPending(msg) {
  const el = document.createElement('div');
  el.className = 'msg sent pending ' + msg.mstate;
  const w = who('you → ' + msg.name);
  const badge = document.createElement('span');
  badge.className = 'mstate';
  badge.textContent = ' ' + (STATE_GLYPH[msg.mstate] || '');
  w.appendChild(badge);
  el.appendChild(w);
  if (msg.quote && (msg.quote.name || msg.quote.excerpt)) {
    el.appendChild(quoteInset(msg.quote.name, msg.quote.excerpt));
  }
  if (msg.image) el.appendChild(photoBox('data:image/jpeg;base64,' + msg.image));
  el.appendChild(richText(msg.text));
  if (msg.mstate === 'failed') {
    const retry = document.createElement('button');
    retry.className = 'retry';
    retry.textContent = 'retry';
    retry.onclick = () => deliver(msg);
    el.appendChild(retry);
  }
  appendStamp(el, msg.ts);
  return el;
}

/* ---------- attention cards ---------- */

/* Walk one contact's events in order and decide, for each `attention`, whether
   a LATER event resolved it — so a stale prompt collapses instead of lingering.
   Resolution is the FIRST of: an `approved` (a key sent from the phone → shown
   "Approved from phone"), an `attention-clear` (resolved at the desk / timed
   out / post-approval → "Resolved"), or a newer `attention` superseding it
   (also "Resolved"). The first resolver wins, so an approve followed by the
   daemon's own attention-clear still reads as "Approved from phone". An
   attention with no later resolver stays live (absent from the map).
   Returns Map(attentionEventId -> { kind: 'approved'|'cleared', ts }). */
/** @param {BridgeEvent[]} events @returns {Map<number, {kind:string, ts:string|null, key?:string}>} */
function resolveAttentions(events) {
  /** @type {Map<number, {kind:string, ts:string|null, key?:string}>} */
  const res = new Map();
  let open = null;   // the currently-unresolved attention, if any
  for (const e of events) {
    if (e.type === 'attention') {
      if (open) res.set(open.id, { kind: 'cleared', ts: e.ts });   // superseded
      open = e;
    } else if (e.type === 'approved') {
      // #17: keep the answered key (the 'approved' event's text — "1"/"3"/"esc",
      // per Emit in httpapi.go) so the collapsed card can show what was tapped
      // when re-expanded. Additive field; existing consumers ignore it.
      if (open) { res.set(open.id, { kind: 'approved', ts: e.ts, key: e.text }); open = null; }
    } else if (e.type === 'attention-clear') {
      if (open) { res.set(open.id, { kind: 'cleared', ts: e.ts }); open = null; }
    }
  }
  return res;   // `open` (if set) is the one live card — deliberately not added
}

/* An attention event. Live → the full tappable approval card. Resolved →
   collapsed: one dimmed prompt line + a resolution line, no buttons. */
/** @param {BridgeEvent} event @param {{kind:string, ts:string|null, key?:string}} [resolution] @returns {HTMLElement} */
function attentionCard(event, resolution) {
  const el = document.createElement('div');
  el.className = 'attention';
  if (resolution) {
    el.classList.add('resolved');
    el.appendChild(who((event.name || '?') + ' needed your attention'));
    const snippet = document.createElement('div');
    snippet.className = 'attn-snippet';
    snippet.textContent = firstLine(event.text);
    el.appendChild(snippet);
    const done = document.createElement('div');
    done.className = 'attn-resolved';
    const label = resolution.kind === 'approved'
      ? '✓ Approved from phone' : '✓ Resolved';
    done.textContent = resolution.ts ? label + ' · ' + localTime(resolution.ts) : label;
    el.appendChild(done);
    // #17: make the collapsed card tap-to-expand (read-only history).
    makeResolvedExpandable(el, event, resolution);
  } else {
    el.appendChild(who((event.name || '?') + ' needs your attention'));
    el.appendChild(promptExcerpt(event.text));
    el.appendChild(approveKeys(event));
  }
  return el;
}

/* ── feature #17: re-expand a resolved prompt ───────────────────────────────
   Turn the collapsed (already-resolved) card into a tap-to-expand chip: tapping
   reveals the original prompt snapshot (the 'attention' event's own text) and
   what was answered (the key from the 'approved' event, mapped back to its
   option label where possible), then tapping again re-collapses. Read-only —
   no re-approving. A resolved card carries no interactive children, so a single
   click handler on the whole card is unambiguous; the collapse/resolve
   machinery in attentionCard/resolveAttentions is left exactly as it was. */
/** @param {HTMLElement} card @param {BridgeEvent} event @param {{kind:string, ts:string|null, key?:string}} resolution */
function makeResolvedExpandable(card, event, resolution) {
  const detail = document.createElement('div');
  detail.className = 'attn-detail hidden';

  const snap = document.createElement('pre');
  snap.className = 'attn-detail-prompt';
  snap.textContent = event.text || '(no prompt text captured)';
  detail.appendChild(snap);

  const answer = document.createElement('div');
  answer.className = 'attn-detail-answer';
  answer.textContent = resolutionAnswer(event, resolution);
  detail.appendChild(answer);
  card.appendChild(detail);

  const hint = document.createElement('div');
  hint.className = 'attn-expand-hint';
  hint.textContent = 'tap to see details';
  card.appendChild(hint);

  card.classList.add('expandable');
  card.setAttribute('role', 'button');
  card.setAttribute('tabindex', '0');
  card.setAttribute('aria-expanded', 'false');
  const toggle = () => {
    const open = !detail.classList.toggle('hidden');   // toggle returns true when re-hidden
    card.setAttribute('aria-expanded', open ? 'true' : 'false');
    hint.textContent = open ? 'tap to hide' : 'tap to see details';
  };
  card.addEventListener('click', toggle);
  card.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
  });
}

// The human-readable "what was answered" line for a re-expanded card. An
// approved digit is mapped back to its option's full label when the snapshot
// still parses (so "3" reads "3. No, and tell Claude…"); 'esc' reads as a
// dismissal; a desk-answered / timed-out resolution carries no key.
/** @param {BridgeEvent} event @param {{kind:string, ts:string|null, key?:string}} resolution @returns {string} */
function resolutionAnswer(event, resolution) {
  if (resolution.kind === 'approved') {
    const key = resolution.key;
    if (key === 'esc') return 'You dismissed this (Esc)';
    if (key) {
      const opt = promptOptions(event.text).find((o) => o.key === key);
      return 'You answered: ' + (opt ? opt.label : key);
    }
    return 'Answered from phone';
  }
  return 'Resolved at the desk, or it timed out';
}
/* ── end #17 ────────────────────────────────────────────────────────────────*/

/** @param {string} [text] @returns {HTMLElement} */
function promptExcerpt(text) {
  const pre = document.createElement('pre');
  const lines = (text || '').split('\n');
  if (lines.length > 4) {
    const excerpt = lines.slice(-4).join('\n');
    pre.textContent = excerpt;
    const more = document.createElement('button');
    more.className = 'show-more';
    more.textContent = 'show full prompt';
    more.onclick = () => {   // a toggle, not a one-way door
      const expanded = more.textContent === 'collapse';
      pre.textContent = expanded ? excerpt : text;
      more.textContent = expanded ? 'show full prompt' : 'collapse';
    };
    const wrap = document.createElement('div');
    wrap.appendChild(more);
    wrap.appendChild(pre);
    return wrap;
  }
  pre.textContent = text || '(no prompt text captured)';
  return pre;
}

/* ── feature #16: labelled, stacked approval buttons ────────────────────────
   Parse the dialog's own numbered options out of the captured snapshot
   (protocol.md §5) and keep the FULL option text, not just the digit. Each
   option becomes { key, label, deny }:
     key   — the bare digit the dialog expects, sent verbatim to /api/approve
             (layout/labels change here; the key on the wire never does)
     label — the whole option line, digit and all ("2. No, and tell Claude
             what to do differently"), so the phone button reads like the TUI
     deny  — a "No…" choice: styled distinctly and wired to the guidance flow
   Fallback when nothing parses: the canonical 1=Yes / 3=No pair, matching the
   daemon's 1/3/esc contract (approveKeys always adds the esc button). */
/** @param {string} [text] @returns {{key:string, label:string, deny:boolean}[]} */
function promptOptions(text) {
  /** @type {{key:string, label:string, deny:boolean}[]} */
  const options = [];
  for (const line of (text || '').split('\n')) {
    const m = line.match(/^\s*(?:❯\s*)?([123])\.\s*(.+?)\s*$/);
    if (m) {
      const body = m[2].trim();
      options.push({ key: m[1], label: m[1] + '. ' + body, deny: /^no\b/i.test(body) });
    }
  }
  return options.length ? options : [
    { key: '1', label: '1. Yes', deny: false },
    { key: '3', label: '3. No', deny: true }];
}

// The live card's buttons: one full-width button per option, stacked
// vertically and labelled with the dialog's real text. Affirmative choices are
// primary; the deny option and the Esc/dismiss button carry secondary/danger
// styling so they never read as the default tap. The key each sends is
// unchanged — only the label and the layout do.
/** @param {BridgeEvent} event @returns {HTMLElement} */
function approveKeys(event) {
  const keys = document.createElement('div');
  keys.className = 'keys';
  for (const opt of promptOptions(event.text)) {
    const btn = document.createElement('button');
    btn.className = 'key-opt' + (opt.deny ? ' deny' : '');
    btn.textContent = opt.label;
    btn.onclick = () => {
      approve(event.agent, opt.key);
      // "No" in Claude Code means "no — and tell me what to do instead": the
      // agent opens a guidance input, and the next phone message lands straight
      // in it. Point the composer there instead of leaving the user wondering.
      if (opt.deny) offerGuidance(event.agent, event.name);
    };
    keys.appendChild(btn);
  }
  const esc = document.createElement('button');
  esc.className = 'key-opt esc';
  esc.textContent = '⎋ Esc';
  esc.onclick = () => approve(event.agent, 'esc');
  keys.appendChild(esc);
  return keys;
}
/* ── end #16 ────────────────────────────────────────────────────────────────*/

// After a No: the agent is waiting to hear what to do differently. Point the
// composer at that conversation and say so; the hint expires quietly.
/** @param {string} agentID @param {string} [name] */
function offerGuidance(agentID, name) {
  state.guidance = { agent: agentID, until: Date.now() + 2 * 60 * 1000 };
  input.placeholder = 'Tell ' + (name || 'the agent') + ' what to do differently…';
  input.focus();
}

/* ---------- composer ---------- */

const input = /** @type {HTMLTextAreaElement} */ ($('msg-input'));

function autogrow() {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 120) + 'px';
}

/** @returns {string} */
function draftKey() { return 'draft-' + state.selected; }

function restoreDraft() {
  input.value = localStorage.getItem(draftKey()) || '';
  autogrow();
}

input.addEventListener('input', () => {
  localStorage.setItem(draftKey(), input.value);
  autogrow();
});

/* iOS shows a Prev/Next/Done accessory bar above the keyboard whenever
   the page has more than one focusable element. While the composer is
   focused, take every other control out of the tab order (taps still
   work) so Safari sees a single input and drops the bar — the
   iMessage-clean keyboard. Restored on blur for desktop keyboard users. */
/** @param {boolean} on */
function setComposerFocusMode(on) {
  document.querySelectorAll('button, [href], input, [tabindex]').forEach((node) => {
    const el = /** @type {HTMLElement} */ (node);
    if (el === input) return;
    if (on) {
      // DOMStringMap coerces the RHS to a string at runtime; the cast is inert
      // (a comment), so the assignment stays byte-identical (no String() wrap).
      if (!el.dataset.tabSaved) el.dataset.tabSaved = /** @type {?} */ (el.tabIndex);
      el.tabIndex = -1;
    } else if (el.dataset.tabSaved !== undefined) {
      el.tabIndex = Number(el.dataset.tabSaved);
      delete el.dataset.tabSaved;
    }
  });
}
input.addEventListener('focus', () => setComposerFocusMode(true));
input.addEventListener('blur', () => setComposerFocusMode(false));

const coarsePointer = matchMedia('(pointer: coarse)').matches;
input.addEventListener('keydown', (e) => {
  // Desktop: Enter sends, Shift+Enter breaks. Touch: Enter always breaks
  // (fat-thumb protection) — the send button is the only trigger.
  if (e.key === 'Enter' && !e.shiftKey && !coarsePointer) {
    e.preventDefault();
    sendMessage();
  }
});

/* ---------- attachments ---------- */

let pendingImage = null; // base64 payload (no data: prefix)

$('attach-btn').addEventListener('click', () => $('attach-input').click());

$('attach-input').addEventListener('change', async (e) => {
  const target = /** @type {HTMLInputElement} */ (e.target);
  const file = target.files && target.files[0];
  target.value = '';
  if (!file) return;
  const dataUrl = await downscale(file).catch(() => null);
  if (!dataUrl) {
    // Never fail silently: a CSP block or an undecodable file used to end
    // here with no trace (found live 2026-07-06).
    alert('Couldn’t read that photo — try picking it again.');
    return;
  }
  pendingImage = dataUrl.split(',')[1];
  /** @type {HTMLImageElement} */ ($('attach-thumb')).src = dataUrl;
  $('attach-preview').classList.remove('hidden');
});

$('attach-remove').addEventListener('click', clearAttachment);

function clearAttachment() {
  pendingImage = null;
  /** @type {HTMLImageElement} */ ($('attach-thumb')).src = '';
  $('attach-preview').classList.add('hidden');
}

/* Re-encode client-side: caps dimensions at 2048px and always produces
   JPEG — smaller uploads and HEIC handled for free by the canvas. */
/** @param {File} file @returns {Promise<string>} */
function downscale(file) {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file);
    const img = new Image();
    img.onload = () => {
      URL.revokeObjectURL(url);
      const scale = Math.min(1, 2048 / Math.max(img.width, img.height));
      const canvas = document.createElement('canvas');
      canvas.width = Math.round(img.width * scale);
      canvas.height = Math.round(img.height * scale);
      canvas.getContext('2d').drawImage(img, 0, 0, canvas.width, canvas.height);
      resolve(canvas.toDataURL('image/jpeg', 0.85));
    };
    img.onerror = reject;
    img.src = url;
  });
}

function sendMessage() {
  const body = input.value.trim();
  const image = pendingImage;
  if ((!body && !image) || !state.selected) return;
  input.value = '';
  localStorage.removeItem(draftKey());
  clearAttachment();
  autogrow();
  requestNotifyPermission();
  // A quote rides on a text send only — the upload path carries none, so a
  // photo drops any pending quote rather than show an inset the agent won't get.
  const quote = image ? null : state.quote;
  /** @type {PendingMsg} */
  const msg = {
    clientId: crypto.randomUUID(),
    agent: state.selected,               // a contact id, or a room id ("room:crew")
    name: threadName(state.selected) || '?',
    text: body,
    image: image || null,
    quote: quote || null,
    ts: new Date().toISOString(),
    mstate: 'sending',
    inflight: false,
  };
  state.pending.push(msg);
  state.guidance = null;   // whatever this message was, the No has its answer
  state.quote = null;      // the chip is consumed; the quote now rides on the msg
  renderQuoteChip();
  savePending();
  renderFeed();
  if (!state.connected) {                // banner is up — don't wait on a dead link
    msg.mstate = 'queued';
    savePending();
    renderFeed();
    return;
  }
  deliver(msg);
}

$('send-btn').addEventListener('click', sendMessage);

/* ---------- quoting & reactions ----------

   Long-press (or right-click) a reply/peer/mention bubble to raise a small
   anchored sheet: Quote (seeds the composer chip) and a reaction row. The
   daemon carries a quote inline to the agent and echoes reactions back as
   'reaction' events the feed folds into badges. */

// The quoted bubble as it rides on an outbound message (a 'sent' event's
// quote_* fields, or a pending echo's msg.quote): author line + excerpt inset.
/** @param {string} [name] @param {string} [excerpt] @returns {HTMLElement} */
function quoteInset(name, excerpt) {
  const box = document.createElement('div');
  box.className = 'quote-inset';
  const n = document.createElement('span');
  n.className = 'quote-inset-name';
  n.textContent = name || '';
  const t = document.createElement('span');
  t.className = 'quote-inset-text';
  t.textContent = excerpt || '';
  box.appendChild(n);
  box.appendChild(t);
  return box;
}

// The reaction badge row for a target bubble, drawn from the folded map. No-op
// when nothing has reacted to this event id.
/** @param {HTMLElement} bubble @param {number} id */
function appendReactions(bubble, id) {
  const arr = state.reactions.get(id);
  if (!arr || !arr.length) return;
  const row = document.createElement('div');
  row.className = 'reactions';
  for (const emoji of arr) {
    const b = document.createElement('span');
    b.className = 'reaction';
    b.textContent = emoji;
    row.appendChild(b);
  }
  bubble.appendChild(row);
}

/* Gesture detection (feature #31): two gestures on a bubble, two surfaces.
   · LONG-PRESS (500ms hold, no drift) — the SELECT menu: Quote / Copy / Forward.
   · DOUBLE-TAP (two clean taps, same bubble, ≤350ms & ≤40px apart) — reactions.
   The sheet's overlay intercepts the trailing synthetic click either way, so it
   can't close the sheet or fire a link under the finger (the backdrop handler
   ignores clicks in the first 350ms after opening). Desktop mirrors both:
   right-click = menu, double-click = reactions. touch-action: manipulation on
   the bubbles (style.css) suppresses iOS's own double-tap zoom on the feed. */
let lpTimer = null;
let lpStart = null;
let actionOpenedAt = 0;
/** @type {{id:number, t:number, x:number, y:number} | null} */
let lastTap = null;   // the previous clean tap — the double-tap detector's memory

/** @typedef {{id:number, name:string, type?:string, text:string}} BubbleMeta */
/** @param {HTMLElement} el @param {BubbleMeta} meta */
function attachBubbleActions(el, meta) {
  el.addEventListener('touchstart', (e) => {
    const t = e.touches && e.touches[0];
    if (!t) return;
    lpStart = { x: t.clientX, y: t.clientY };
    // Freeze the press point for the sheet: it opens AT the finger, never at the
    // bubble's box — a tall bubble's top edge can sit a screenful above the
    // touch, which is how the menu used to fly to the top of the screen.
    const at = { x: t.clientX, y: t.clientY };
    clearTimeout(lpTimer);
    lpTimer = setTimeout(() => {
      lpStart = null;
      lastTap = null;   // the hold consumed this touch — it is not tap #1
      openActionSheet(meta, at, 'menu');
    }, 500);
  }, { passive: true });
  el.addEventListener('touchmove', (e) => {
    if (!lpStart) return;
    const t = e.touches && e.touches[0];
    if (t && Math.hypot(t.clientX - lpStart.x, t.clientY - lpStart.y) > 10) {
      clearTimeout(lpTimer); lpStart = null; lastTap = null;  // a scroll is neither press nor tap
    }
  }, { passive: true });
  el.addEventListener('touchend', () => {
    clearTimeout(lpTimer);
    if (!lpStart) return;             // long-press already fired, or drift cancelled
    const at = { x: lpStart.x, y: lpStart.y };
    lpStart = null;
    // A clean short tap. Second one on the SAME bubble completes a double-tap →
    // the reaction sheet. (Single taps keep doing what they always did: nothing.)
    const now = { id: meta.id, t: Date.now(), x: at.x, y: at.y };
    if (lastTap && lastTap.id === meta.id && isDoubleTap(lastTap, now)) {
      lastTap = null;
      openActionSheet(meta, at, 'react');
      return;
    }
    lastTap = now;
  }, { passive: true });
  // Desktop mirrors: right-click = select menu, double-click = reactions.
  el.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    openActionSheet(meta, { x: e.clientX, y: e.clientY }, 'menu');
  });
  el.addEventListener('dblclick', (e) => {
    openActionSheet(meta, { x: e.clientX, y: e.clientY }, 'react');
  });
}

/* The one popover, two modes. 'react' (double-tap): the reaction row alone.
   'menu' (long-press): the select actions — Quote / Copy / Forward — and no
   reactions. Only the visible half is built, so neither mode pays for (or
   leaks listeners into) the other. */
/** @param {BubbleMeta} meta @param {{x:number, y:number}} at @param {'menu'|'react'} mode */
function openActionSheet(meta, at, mode) {
  const reactions = $('action-reactions');
  reactions.innerHTML = '';
  const menuMode = mode === 'menu';
  $('action-quote').classList.toggle('hidden', !menuMode);
  $('action-copy').classList.toggle('hidden', !menuMode);
  $('action-forward').classList.toggle('hidden', !menuMode);
  if (menuMode) {
    $('action-quote').onclick = () => { setQuote(meta); closeActionSheet(); };
    $('action-copy').onclick = () => { copyBubbleText(meta.text); closeActionSheet(); };
    $('action-forward').onclick = () => { closeActionSheet(); openForwardSheet(meta); };
  } else {
    const mine = state.myReactions.get(meta.id);
    // The quick row: the 6 whitelisted taps (unchanged behaviour), wrapped so the
    // emoji picker below can swap the whole row out with one class toggle.
    const quick = document.createElement('div');
    quick.className = 'reaction-quick';
    for (const emoji of REACTIONS) {
      const already = !!(mine && mine.has(emoji));
      const b = document.createElement('button');
      b.className = 'action-reaction' + (already ? ' reacted' : '');
      b.textContent = emoji;
      if (already) {
        b.disabled = true;   // already reacted this session — pressed and inert
      } else {
        b.onclick = () => { react(state.selected, meta.id, emoji); closeActionSheet(); };
      }
      quick.appendChild(b);
    }
    reactions.appendChild(quick);
    // ── BUG 2 (react with ANY emoji) ──────────────────────────────────────────
    // …plus a "+" that hands off to the phone's NATIVE emoji keyboard, so the
    // user isn't limited to the 6. The pick flows through the SAME react() path.
    addEmojiPicker(reactions, quick, meta);
    // ── end BUG 2 ─────────────────────────────────────────────────────────────
  }
  actionOpenedAt = Date.now();
  positionActionMenu(at);
}

/* ---------- select-menu actions: copy & forward (feature #31) ---------- */

/* Copy the pressed bubble's text. navigator.clipboard needs a secure context
   and can still reject inside some gesture timings; the execCommand path is the
   time-tested fallback and works from any user gesture. Either way the toast
   answers the tap — silence here would read as a broken button. */
/** @param {string} text */
async function copyBubbleText(text) {
  let ok = false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      ok = true;
    }
  } catch { /* fall through to execCommand */ }
  if (!ok) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
  }
  flashToast(ok ? 'Copied' : 'Couldn’t copy');
}

/* The forward picker: every room and contact except the thread we're in.
   Tapping a row sends "↪ from <author>: <text>" to that agent through the SAME
   pending/deliver pipeline as a typed message — durable outbox, offline
   queueing, retry, and a normal echo in the receiving thread, all for free. */
/** @param {BubbleMeta} meta */
function openForwardSheet(meta) {
  const list = $('forward-list');
  list.innerHTML = '';
  /** @type {Array<Room | Contact>} */
  const targets = [...state.rooms, ...state.contacts.filter((c) => c.id !== state.selected)];
  for (const t of targets) {
    const row = document.createElement('button');
    row.className = 'forward-row';
    const name = document.createElement('span');
    name.className = 'forward-row-name';
    name.textContent = t.name;
    row.appendChild(name);
    if (!isRoomId(t.id) && /** @type {Contact} */ (t).status !== 'live') {
      const off = document.createElement('span');
      off.className = 'forward-row-note';
      off.textContent = 'offline — will queue';
      row.appendChild(off);
    }
    row.onclick = () => {
      forwardTo(t.id, meta);
      closeForwardSheet();
      flashToast('Forwarded to ' + t.name);
    };
    list.appendChild(row);
  }
  $('forward-sheet').classList.remove('hidden');
}

function closeForwardSheet() { $('forward-sheet').classList.add('hidden'); }

/** @param {string} targetId @param {BubbleMeta} meta */
function forwardTo(targetId, meta) {
  /** @type {PendingMsg} */
  const msg = {
    clientId: crypto.randomUUID(),
    agent: targetId,
    name: threadName(targetId) || '?',
    text: forwardText(meta.name, meta.text),
    image: null,
    quote: null,
    ts: new Date().toISOString(),
    mstate: 'sending',
    inflight: false,
  };
  state.pending.push(msg);
  savePending();
  renderList();          // the target's row preview shows the forward immediately
  if (!state.connected) {
    msg.mstate = 'queued';
    savePending();
    return;
  }
  deliver(msg);
}

$('forward-close').addEventListener('click', closeForwardSheet);
$('forward-backdrop').addEventListener('click', closeForwardSheet);

/* A quiet self-dismissing toast (reuses the context-gauge's .ctx-toast look —
   same quiet register, its own element so the modules stay uncoupled). */
let toastTimer = null;
/** @param {string} text */
function flashToast(text) {
  let el = document.getElementById('app-toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'app-toast';
    el.className = 'ctx-toast hidden';
    document.body.appendChild(el);
  }
  el.textContent = text;
  el.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add('hidden'), 1600);
}

/* ── BUG 2 helper: the "react with any emoji" affordance ─────────────────────
   Appends a "+" button and a hidden capture input to the reaction row. Tapping
   "+" hides the 6-tap quick row, reveals the input, and focuses it — on iOS that
   pops the keyboard; the user switches to emoji (🌐 key) and taps one. The FIRST
   emoji entered is captured (grapheme-aware — a ZWJ sequence, flag, or skin-tone
   modifier stays whole) and sent through react() exactly like a quick tap, then
   the sheet closes. No CDN, no emoji data, no build step: the phone's own
   keyboard IS the picker (strict CSP-friendly). The whole thing lives inside
   #action-reactions, which openActionSheet clears on every open, so it never
   stacks or leaks a stale listener across opens.
   NOTE for Vint: end-to-end this needs the daemon's reactionEmoji whitelist
   (httpapi.go) opened up — today anything outside the 6 returns 400 "bad-emoji"
   and react() rolls the optimistic badge back. The PWA side is ready; the
   companion daemon change is out of this agent's file scope. */
/** @param {HTMLElement} reactions @param {HTMLElement} quick @param {BubbleMeta} meta */
function addEmojiPicker(reactions, quick, meta) {
  const more = document.createElement('button');
  more.className = 'action-reaction action-reaction-more';
  more.textContent = '+';
  more.setAttribute('aria-label', 'React with any emoji');
  quick.appendChild(more);

  const picker = document.createElement('input');
  picker.id = 'action-emoji-input';
  picker.className = 'action-emoji-input hidden';
  picker.type = 'text';
  picker.autocomplete = 'off';
  picker.setAttribute('autocapitalize', 'none');
  picker.setAttribute('aria-label', 'Type or pick any emoji');
  picker.placeholder = 'Pick any emoji…';
  reactions.appendChild(picker);

  more.onclick = () => {
    quick.classList.add('hidden');        // hand the row over to the picker
    picker.classList.remove('hidden');
    // Focus reveals the OS keyboard. The popover may end up behind the keyboard,
    // but that's immaterial: we capture the first emoji and close immediately, so
    // the user interacts with the keyboard, never the input itself.
    picker.focus();
  };

  let fired = false;   // one reaction per open — guard against a double 'input'
  picker.addEventListener('input', (e) => {
    if (/** @type {InputEvent} */ (e).isComposing || fired) return;   // ignore mid-IME-composition frames
    const emoji = firstEmoji(picker.value);
    if (!emoji) return;                    // nothing pickable yet (empty / letters)
    fired = true;
    react(state.selected, meta.id, emoji); // SAME path the 6 quick taps use
    picker.blur();                         // dismiss the keyboard
    closeActionSheet();
  });
}

/* The first WHOLE emoji in a string, grapheme-aware. Intl.Segmenter keeps a ZWJ
   sequence (👨‍👩‍👧), a flag (🇺🇸), a skin-tone modifier (👍🏽) or a
   variation-selector emoji (❤️) intact as one user-perceived character. Skips any
   leading non-emoji (a stray letter the keyboard was still on) and returns '' when
   there is no emoji yet — so the picker only fires on a real pick, and a
   multi-emoji paste yields just the first (the "take the first emoji" guard). */
/** @param {string} str @returns {string} */
function firstEmoji(str) {
  const s = str || '';
  /** @param {string} g */
  const isEmoji = (g) => /(\p{Extended_Pictographic}|\p{Regional_Indicator})/u.test(g);
  // Intl.Segmenter is es2022; the lib is es2020, so reach it via an inert cast
  // (a comment) rather than bumping the shared tsconfig lib.
  if (typeof Intl !== 'undefined' && /** @type {any} */ (Intl).Segmenter) {
    for (const { segment } of new (/** @type {any} */ (Intl).Segmenter)(undefined, { granularity: 'grapheme' }).segment(s)) {
      if (isEmoji(segment)) return segment;
    }
    return '';
  }
  // Fallback (no Segmenter): first pictographic/flag code point, greedily
  // extended over trailing joiners / variation selectors / skin tones / flags.
  const cps = Array.from(s);
  for (let i = 0; i < cps.length; i++) {
    if (!isEmoji(cps[i])) continue;
    // Extend the base emoji over trailing joiners / variation selectors /
    // skin-tone modifiers / regional-indicator (flag) halves — numeric
    // code-point checks, so no invisible literals live in the source.
    let g = cps[i];
    const cont = (ch) => {
      const cp = ch.codePointAt(0);
      return cp === 0x200D || cp === 0xFE0F ||        // ZWJ, VS16
             (cp >= 0x1F3FB && cp <= 0x1F3FF) ||       // skin-tone modifiers
             (cp >= 0x1F1E6 && cp <= 0x1F1FF);         // regional indicators
    };
    for (let j = i + 1; j < cps.length && cont(cps[j]); j++) g += cps[j];
    return g;
  }
  return '';
}

function closeActionSheet() { $('action-sheet').classList.add('hidden'); }

// Raise the menu AT the press point (touch or cursor), in viewport coordinates.
// position:fixed makes {x,y} a viewport anchor — immune to the feed's scroll
// offset and any positioned ancestor (the old code anchored to the bubble's
// box, so a tall bubble threw the menu to the top of the screen). Reveal to
// measure, clamp 8px inside every edge, and flip ABOVE the finger when placing
// below it would spill past the bottom. CSSOM writes, allowed by the CSP.
/** @param {{x:number, y:number}} at */
function positionActionMenu(at) {
  const menu = $('action-menu');
  menu.style.visibility = 'hidden';        // reveal to measure, place, then show
  $('action-sheet').classList.remove('hidden');
  const mw = menu.offsetWidth, mh = menu.offsetHeight, pad = 8;
  const vw = window.innerWidth, vh = window.innerHeight;
  let top = at.y;
  if (top + mh + pad > vh) top = at.y - mh;   // would spill below → flip above the finger
  top = Math.max(pad, Math.min(top, vh - mh - pad));
  const left = Math.max(pad, Math.min(at.x, vw - mw - pad));
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
  menu.style.visibility = 'visible';
}

// Tap outside the menu (or Esc) dismisses; the 350ms guard swallows the
// synthetic click that follows the long-press so the sheet doesn't self-close.
$('action-sheet').addEventListener('click', (e) => {
  if (e.target !== $('action-sheet')) return;
  if (Date.now() - actionOpenedAt < 350) return;
  closeActionSheet();
});
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeActionSheet(); });

/* Send an emoji reaction. The badge itself arrives via the daemon's echoed
   'reaction' event over SSE; myReactions only tracks this session's taps for the
   pressed/disabled state, so it rolls back if the POST doesn't land. */
/** @param {string | null} agent @param {number} eventId @param {string} emoji */
async function react(agent, eventId, emoji) {
  if (!agent) return;
  const set = state.myReactions.get(eventId) || new Set();
  if (set.has(emoji)) return;
  set.add(emoji);
  state.myReactions.set(eventId, set);
  const res = await api('/api/react', { agent, event_id: eventId, emoji });
  if (!res || !res.ok) {
    set.delete(emoji);
    if (!set.size) state.myReactions.delete(eventId);
  }
}

/* ---------- quote chip ---------- */

// First 80 chars of a bubble, flattened to one line (plainPreview strips
// thinking/markdown noise); the daemon re-clamps to 80 runes authoritatively.
/** @param {string} [text] @returns {string} */
function quoteExcerptText(text) {
  const clean = plainPreview(text) || (text || '').replace(/\s+/g, ' ').trim();
  return clean.slice(0, 80);
}

/** @param {BubbleMeta} meta */
function setQuote(meta) {
  state.quote = { name: meta.name, excerpt: quoteExcerptText(meta.text) };
  renderQuoteChip();
  // ── BUG 1 (quote focus-steal) ───────────────────────────────────────────────
  // Deliberately do NOT focus the composer here. On mobile, focusing the
  // textarea the instant "Quote" is tapped pops the on-screen keyboard up over
  // the screen — jarring and unwanted (iPhone daily-driver field report). The
  // quote is now fully armed anyway: the chip is up (renderQuoteChip above) and
  // state.quote rides on the next reply (see sendMessage/deliver). The user taps
  // the box themselves when ready to type. Was: input.focus();
  // ── end BUG 1 ────────────────────────────────────────────────────────────────
}

function renderQuoteChip() {
  const chip = $('quote-chip');
  if (!state.quote) { chip.classList.add('hidden'); return; }
  $('quote-chip-name').textContent = state.quote.name || '';
  $('quote-chip-text').textContent = state.quote.excerpt || '';
  chip.classList.remove('hidden');
}

$('quote-chip-remove').addEventListener('click', () => { state.quote = null; renderQuoteChip(); });

/* ---------- actions ---------- */

/* Every POST is bounded by a 10s timeout: a hung request aborts and returns
   null (a failed send, retryable). Returns the Response otherwise so callers
   can read res.ok / res.status. */
/** @param {string} path @param {any} payload @returns {Promise<Response | null>} */
export async function api(path, payload) {   // exported: context-gauge.js POSTs /api/compact through it
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 10000);
  try {
    return await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal: ctrl.signal,
    });
  } catch (e) {
    return null;   // network error or timeout
  } finally {
    clearTimeout(timer);
  }
}

/* Send (or re-send) one pending message. Idempotent by client_id, so a
   retry that the server already saw comes back 200 duplicate — still ok. */
/** @param {PendingMsg} msg */
async function deliver(msg) {
  if (msg.inflight) return;
  if (!navigator.onLine) {
    msg.mstate = 'queued'; savePending(); renderFeed(); return;
  }
  msg.inflight = true;
  msg.mstate = 'sending';
  savePending();
  renderFeed();
  /** @type {{agent:string, text:string, client_id:string, image?:string, quote?:Quote}} */
  const payload = { agent: msg.agent, text: msg.text, client_id: msg.clientId };
  if (msg.image) payload.image = msg.image;
  // Quote rides on /api/send only (the upload path ignores it). Sent from
  // deliver() — not sendMessage — so a queued reply keeps its quote on retry.
  if (msg.quote && !msg.image) payload.quote = msg.quote;
  const res = await api(msg.image ? '/api/upload' : '/api/send', payload);
  msg.inflight = false;
  if (res && res.ok) {
    msg.mstate = 'sent';            // 2xx (incl. duplicate) — server has it
    setConnected(true);
  } else if (res && res.status === 409) {
    msg.mstate = 'queued';          // contact has no live session right now
  } else {
    msg.mstate = 'failed';          // timeout / network / 5xx / 400
    if (!res) setConnected(false);
  }
  savePending();
  renderFeed();
}

/* Retry every undelivered outbox message. Called on reconnect, on returning
   to the foreground, and when an offline contact comes back to life. */
function flushOutbox() {
  if (!navigator.onLine) return;
  for (const msg of state.pending) {
    if (msg.inflight) continue;
    if (msg.mstate !== 'sent') deliver(msg);
  }
}

/* The server broadcasts its own "sent" event for each accepted message;
   drop the matching local echo so the thread shows one bubble, not two.
   Match on the server-echoed client_id first — text-matching can splice out
   the wrong queued message when two share the same text ("yes"/"go"). Fall
   back to text only when no client_id is present (older server / no match).
   Uploads arrive with a " 📷 photo" suffix the echo doesn't have. */
/** @param {string} agent @param {string} [text] @param {string} [clientId] */
function dropPendingEcho(agent, text, clientId) {
  let i = -1;
  if (clientId) {
    // The server named the exact message; if its echo is already gone there
    // is nothing to drop. Falling through to text here used to splice out a
    // DIFFERENT queued message that happened to share the text ("yes"/"go").
    i = state.pending.findIndex((m) => m.clientId === clientId);
  } else {
    const bare = (text || '').replace(/\s*📷 photo$/, '').trim();
    i = state.pending.findIndex((m) =>
      m.agent === agent && (m.text === (text || '') || m.text.trim() === bare));
  }
  if (i !== -1) { state.pending.splice(i, 1); savePending(); }
}

function savePending() {
  try {
    localStorage.setItem('outbox', JSON.stringify(
      state.pending.filter((m) => m.mstate !== 'sent').map((m) => ({
        clientId: m.clientId, agent: m.agent, name: m.name, text: m.text,
        image: m.image || null, quote: m.quote || null, ts: m.ts, mstate: m.mstate,
      }))));
  } catch (e) { /* storage full — best-effort, like the event cache */ }
}

function loadPending() {
  const saved = JSON.parse(localStorage.getItem('outbox') || '[]');
  state.pending = saved.map((m) => ({
    ...m,
    inflight: false,
    mstate: m.mstate === 'sending' ? 'queued' : m.mstate,
  }));
}

/** @param {string} agent @param {string} key */
async function approve(agent, key) {
  await api('/api/approve', { agent, key });
}

/* ---------- notifications (best-effort; no-op where unsupported) ----------
   Peeled into ./notifications.js (imported at the top of this file): the
   enable-push button + tap flow, permission request, Web Push subscribe, the
   VAPID-key decode, the hidden-tab page notification, and
   clearDeliveredNotifications. The spine calls updatePushButton /
   requestNotifyPermission / maybeNotify / clearDeliveredNotifications, imported
   back at the top. */

init();

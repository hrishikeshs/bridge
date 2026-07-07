/* bridge — phone client.
   Talks to the daemon over REST + Server-Sent Events. */

'use strict';

const $ = (id) => document.getElementById(id);

/* ---------- theme ----------
   Every colour is a CSS custom property; three [data-theme] palettes live in
   style.css. Golden Hour is the default (the base :root values + the manifest
   colours), so an unset attribute renders it with no flash. We apply the saved
   theme as the very first thing app.js does — the CSP forbids an inline <script>
   in <head>, so this is the earliest hook; a non-default theme may show one
   Golden-Hour frame before this runs, which is acceptable. */

const THEMES = ['golden-hour', 'dusk', 'international-orange'];
// <meta name="theme-color"> per theme (browser/PWA chrome tint). The manifest
// is static, so its theme_color/background_color track the default only.
const THEME_META = {
  'golden-hour': '#FAF5EC',
  'dusk': '#141B26',
  'international-orange': '#1C3A5E',
};
// Picker copy + a 5-swatch preview (ground, outbound, inbound, accent, resolved).
const THEME_INFO = {
  'golden-hour': { name: 'Golden Hour',
    swatches: ['#FAF5EC', '#D3653B', '#EAF0F6', '#4E739F', '#59805D'] },
  'dusk': { name: 'Dusk',
    swatches: ['#141B26', '#DF7B4E', '#26344A', '#8AAAC9', '#7FB287'] },
  'international-orange': { name: 'International Orange',
    swatches: ['#1C3A5E', '#C8432B', '#EEF2F6', '#4C7FB5', '#55875B'] },
};

function currentTheme() {
  const t = localStorage.getItem('theme');
  return THEMES.includes(t) ? t : 'golden-hour';
}

function applyTheme(theme) {
  if (!THEMES.includes(theme)) theme = 'golden-hour';
  document.documentElement.setAttribute('data-theme', theme);
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', THEME_META[theme]);
}

function setTheme(theme) {
  if (!THEMES.includes(theme)) return;
  localStorage.setItem('theme', theme);
  applyTheme(theme);
}

applyTheme(currentTheme());

/* ---------- background (the scenery layer) ----------
   Off / airy / whisper — the veil strength over the Golden Gate photo.
   Fog density follows the real San Francisco marine-layer schedule: dense
   mornings, burned off by afternoon, rolling back at dusk. */

const WALLPAPERS = ['airy', 'whisper', 'off'];
const WALLPAPER_NAMES = { airy: 'Bridge · airy veil', whisper: 'Bridge · whisper veil', off: 'Off' };

function currentWallpaper() {
  const w = localStorage.getItem('wallpaper');
  return WALLPAPERS.includes(w) ? w : 'airy';
}

function applyWallpaper(w) {
  if (!WALLPAPERS.includes(w)) w = 'airy';
  document.documentElement.setAttribute('data-wallpaper', w);
  updateFog();
}

function setWallpaper(w) {
  if (!WALLPAPERS.includes(w)) return;
  localStorage.setItem('wallpaper', w);
  applyWallpaper(w);
}

// SF marine layer, by local hour: thick mornings, clear afternoons, the bank
// rolls back in around dusk, settles overnight. The app has weather.
function fogDensity(hour) {
  if (hour >= 5 && hour < 11) return 1.0;
  if (hour >= 11 && hour < 17) return 0.35;
  if (hour >= 17 && hour < 22) return 0.85;
  return 0.6;
}

function updateFog() {
  document.documentElement.style.setProperty(
    '--fog-density', String(fogDensity(new Date().getHours())));
}

/* ---------- light (the Golden-Hour palette drift) ----------
   The default theme is named for a moment of light, so it follows one. Same
   spirit as the fog above — a month-keyed table and a little arithmetic, no
   astronomy — but here the palette drifts through five phases across the day.
   Each phase is a data-phase attribute on <html>; style.css maps it to a small
   set of hue-token overrides, scoped to the golden-hour theme so the other
   palettes never drift. 'day' is the anchor: it sets no overrides, so mid-day
   is exactly the Golden Hour of before. */

// Approximate SF sunrise/sunset by month, as the clock reads them (DST folded
// in). Off by twenty minutes is fine — this is weather, not an almanac.
//               Jan   Feb   Mar   Apr   May   Jun   Jul   Aug   Sep   Oct   Nov   Dec
const SUNRISE = [7.4,  7.0,  7.1,  6.4,  6.0,  5.8,  6.0,  6.4,  6.9,  7.3,  6.9,  7.2];
const SUNSET  = [17.2, 17.8, 19.0, 19.7, 20.2, 20.5, 20.4, 20.0, 19.3, 18.5, 17.0, 16.9];

// The phase for a moment, keyed off that month's sun. The narrow bands
// (dawn / golden / dusk) hug sunrise and sunset; day fills the long middle;
// night is everything left over — the evening and the small hours.
function solarPhase(date) {
  const rise = SUNRISE[date.getMonth()];
  const set  = SUNSET[date.getMonth()];
  const h = date.getHours() + date.getMinutes() / 60;   // decimal local hour
  if (h >= rise - 0.75 && h < rise + 1)   return 'dawn';    // 45m before → 1h after sunrise
  if (h >= set - 1.5   && h < set)        return 'golden';  // last 90m of daylight
  if (h >= set         && h < set + 0.75) return 'dusk';    // sunset → 45m after
  if (h >= rise + 1    && h < set - 1.5)  return 'day';     // mid-morning → afternoon (anchor)
  return 'night';
}

// Default ON; the settings toggle stores 'off' to opt out. Only an explicit
// 'off' disables it, so the drift is on for everyone who never opens settings.
function paletteFollowsSun() {
  return localStorage.getItem('paletteSun') !== 'off';
}

// Off → the static day anchor (today's exact look). The attribute is inert
// under the other themes; the CSS scoping guarantees it.
function applyPhase() {
  const phase = paletteFollowsSun() ? solarPhase(new Date()) : 'day';
  document.documentElement.setAttribute('data-phase', phase);
}

function setPaletteSun(on) {
  localStorage.setItem('paletteSun', on ? 'on' : 'off');
  applyPhase();
}

// The weather (fog) and the light (palette) both key off the local hour, so
// they re-check together on one timer, twice an hour.
setInterval(() => { updateFog(); applyPhase(); }, 30 * 60 * 1000);
applyWallpaper(currentWallpaper());
applyPhase();
// Arm the drift transition only after the first paint, so a phase correction
// on a slow cold-load lands as a quiet snap (like the theme does) rather than
// a 2.4s smear; every flip from here on dissolves gently.
requestAnimationFrame(() => document.documentElement.classList.add('phase-animate'));

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

const state = {
  contacts: [],          // roster from /api/status
  rooms: [],             // shared threads from /api/status (v1: just #crew)
  events: [],            // chronological event list
  attentions: new Map(), // contact id -> latest unresolved attention event
  reactions: new Map(),  // target event id -> array of emoji (folded from 'reaction' events)
  myReactions: new Map(),// target event id -> Set of emoji THIS phone sent this session
  quote: null,           // {name, excerpt} — the bubble the composer is replying to
  view: 'list',          // 'list' (conversation list) | 'thread' (one contact)
  selected: null,        // contact id of the open thread, or null on the list
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
function isRoomId(id) { return typeof id === 'string' && id.startsWith('room:'); }
function roomById(id) { return state.rooms.find((r) => r.id === id) || null; }

// A thread target is legitimate if it's a known contact OR a known room. A
// notification tap deep-links with contact="room:crew", so navigation must
// validate against both (never trust an id from outside to pick a target).
function isValidTarget(id) {
  return state.contacts.some((c) => c.id === id) || state.rooms.some((r) => r.id === id);
}

// Display name for a thread id — a contact's name or a room's ("#crew"). Used
// wherever contactName() alone would return undefined for a room.
function threadName(id) {
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
    // #19(b): idle detection for the screensaver. Pointer/key events are
    // non-passive so a waking tap can be swallowed (dismiss only); scroll-class
    // events are passive (just re-arm) to keep scrolling smooth. All capture so
    // they still fire while the dimmed list is pointer-events:none.
    ['touchstart', 'pointerdown', 'mousedown', 'keydown'].forEach(
      (t) => document.addEventListener(t, onUserActivity, { capture: true }));
    ['scroll', 'touchmove', 'wheel', 'mousemove'].forEach(
      (t) => document.addEventListener(t, onUserActivity, { capture: true, passive: true }));
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
  const code = $('pair-code').value.trim();
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
function clockUnix(sec) {
  const d = new Date((sec || 0) * 1000);
  return isNaN(d) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
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

function markSeen(contactId) {
  state.lastSeen[contactId] = state.lastEventId;
  localStorage.setItem('lastSeen', JSON.stringify(state.lastSeen));
}

// Unread = agent replies/mentions newer than the stored cursor. Outbound and
// self events never count. This reads the SAME lastSeen cursor the old boolean
// used (a per-contact event id), so existing devices need no migration — the
// old hasUnread was just this count > 0.
function unreadCount(contactId) {
  const seen = state.lastSeen[contactId] || 0;
  let n = 0;
  for (const e of state.events) {
    if (e.agent === contactId && e.id > seen &&
        (e.type === 'reply' || e.type === 'mention' || e.type === 'peer')) n++;
  }
  return n;
}

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

/* iMessage behavior: opening the app clears its pile from Notification
   Center — everything a delivered banner said is richer in-app anyway. iOS
   never withdraws delivered notifications itself, so stale "needs your
   attention" banners otherwise outlive their prompts (field report,
   2026-07-06). Best-effort: notification support may be absent entirely. */
async function clearDeliveredNotifications() {
  try {
    if (!('serviceWorker' in navigator)) return;
    const reg = await navigator.serviceWorker.ready;
    (await reg.getNotifications()).forEach((n) => {
      // Opening the app clears DELIVERED notifications — but a "needs you"
      // whose prompt is still open is a live demand, not a delivered one.
      // Closing it here silenced other agents' active prompts every time the
      // app was foregrounded (review round 3 hygiene).
      const m = (n.tag || '').match(/^attn-(.+)$/);
      if (m && state.attentions.has(m[1])) return;
      n.close();
    });
  } catch (e) { /* unsupported or not yet registered — nothing to clear */ }
}

// Enter a thread and push it onto history, so back pops to the list.
function navigateToThread(id) {
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
function selectContactIfValid(id) {
  if (id && isValidTarget(id)) navigateToThread(id);
}

$('back-btn').addEventListener('click', () => history.back());

/* Interrupt: one tap → the daemon sends the agent a bare Escape (stop
   mid-thought). Held burst messages are then delivered immediately, so
   "stop — do this instead" arrives in the right order. */
$('stop-btn').addEventListener('click', async () => {
  if (!state.selected) return;
  const btn = $('stop-btn');
  btn.disabled = true;
  await api('/api/interrupt', { agent: state.selected });
  btn.disabled = false;
});

/* ---------- feature #19(b): idle screensaver ----------
   After IDLE_SCREENSAVER_MS of no interaction on the list, the list UI fades
   down to reveal the full scenery (Golden Gate under the drifting marine-layer
   fog, on its existing CSS animation). Any touch / scroll / key, a return to
   the foreground, or an incoming attention/message restores it instantly.
   Only the list view carries scenery, and only with a wallpaper set, so the
   idle timer arms there alone — a thread (words over solid ground) never dims.
   The class rides on <html>; the CSS fades #list-view and drops its pointer
   events, and onUserActivity swallows the waking gesture so the first tap only
   dismisses (it never also taps a row underneath). */
const IDLE_SCREENSAVER_MS = 45000;
let screensaverTimer = null;

function screensaverEligible() {
  return state.view === 'list' && currentWallpaper() !== 'off' && !document.hidden;
}

function armScreensaver() {
  clearTimeout(screensaverTimer);
  if (!screensaverEligible()) return;
  screensaverTimer = setTimeout(engageScreensaver, IDLE_SCREENSAVER_MS);
}

function engageScreensaver() {
  if (!screensaverEligible()) return;   // re-check: view/wallpaper may have changed
  document.documentElement.classList.add('screensaver');
}

// Restore the UI (if dimmed) and restart the idle countdown. Safe to call from
// anywhere — on a thread it just clears the timer (ineligible).
function wakeScreensaver() {
  document.documentElement.classList.remove('screensaver');
  armScreensaver();
}

function onUserActivity(e) {
  if (document.documentElement.classList.contains('screensaver')) {
    document.documentElement.classList.remove('screensaver');
    // Swallow the waking gesture: it only dismisses the screensaver, it must
    // not also activate whatever sits under the finger. Only the pointer/key
    // listeners are non-passive, so only they can (and do) preventDefault.
    const swallow = e.type === 'touchstart' || e.type === 'pointerdown' ||
                    e.type === 'mousedown' || e.type === 'keydown';
    if (swallow && e.cancelable) { e.preventDefault(); e.stopPropagation(); }
  }
  armScreensaver();
}

// A "moment" worth waking for: a new attention card or an inbound message.
// Heartbeats, typing, status and reactions never wake the screensaver.
function isMomentEvent(event) {
  return event.type === 'attention' || event.type === 'reply' ||
         event.type === 'mention' || event.type === 'peer' || event.type === 'paper';
}
/* ---------- end #19(b) ---------- */

/* ---------- settings sheet ---------- */

// The gear opens a slide-up sheet: a theme picker (applies + persists on tap)
// and the notification control (state + enable flow, relocated from the gear).
$('settings-btn').addEventListener('click', openSettings);
$('settings-close').addEventListener('click', closeSettings);
$('settings-backdrop').addEventListener('click', closeSettings);
$('notif-row').addEventListener('click', async () => {
  await requestNotifyPermission();
  updatePushButton();
  renderNotifState();
});

// My status: the human's away line. 'change' fires on blur/Enter for a text
// input, so an edit saves when you leave the field; Enter blurs to commit it.
// The ✕ clears. Both POST /api/mystatus {text}; the daemon echoes a live
// 'mystatus' event so every open phone (and the next agent to reach out) syncs.
$('mystatus-input').addEventListener('change', (e) => saveMyStatus(e.target.value));
$('mystatus-input').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); e.target.blur(); }
});
// preventDefault on mousedown keeps the input focused when ✕ is tapped, so its
// blur doesn't fire a spurious 'change' (old value) that races the clear's POST.
$('mystatus-clear').addEventListener('mousedown', (e) => e.preventDefault());
$('mystatus-clear').addEventListener('click', () => { $('mystatus-input').value = ''; saveMyStatus(''); });

async function saveMyStatus(text) {
  // Mirror the daemon's clampAway: one line, capped — so the input and the
  // stored value never disagree (the daemon clamps again authoritatively).
  text = (text || '').replace(/[\r\n\t]+/g, ' ').trim().slice(0, 120);
  state.myStatus = text;
  $('mystatus-input').value = text;
  renderMyStatus();
  try {
    await fetch('/api/mystatus', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text }),
    });
  } catch (e) { /* offline — the daemon keeps its last value; a later edit retries */ }
}

function openSettings() {
  renderThemeOptions();
  renderWallpaperOptions();
  renderPaletteSun();
  renderNotifState();
  $('mystatus-input').value = state.myStatus || '';
  $('settings-sheet').classList.remove('hidden');
}

function renderWallpaperOptions() {
  const box = $('wallpaper-options');
  const active = currentWallpaper();
  box.innerHTML = '';
  for (const key of WALLPAPERS) {
    const row = document.createElement('button');
    row.className = 'theme-row';
    const name = document.createElement('span');
    name.className = 'theme-name';
    name.textContent = WALLPAPER_NAMES[key];
    const check = document.createElement('span');
    check.className = 'check';
    check.textContent = key === active ? '✓' : '';
    row.appendChild(name);
    row.appendChild(check);
    row.onclick = () => { setWallpaper(key); renderWallpaperOptions(); };
    box.appendChild(row);
  }
}

// "Palette follows the sun" — a single on/off switch. The drift only touches
// the Golden Hour palette, so the control is shown ONLY under that theme (the
// whole section hides otherwise; the CSS scoping would make it inert there
// anyway). role="switch" + aria-checked so it reads as a toggle to VoiceOver.
function renderPaletteSun() {
  const section = $('palette-sun-section');
  const box = $('palette-sun-options');
  const golden = currentTheme() === 'golden-hour';
  section.classList.toggle('hidden', !golden);
  box.textContent = '';
  if (!golden) return;

  const on = paletteFollowsSun();
  const row = document.createElement('button');
  row.className = 'sheet-row toggle-row';
  row.setAttribute('role', 'switch');
  row.setAttribute('aria-checked', on ? 'true' : 'false');

  const label = document.createElement('span');
  label.className = 'toggle-label';
  label.textContent = 'Palette follows the sun';

  const sw = document.createElement('span');
  sw.className = on ? 'toggle on' : 'toggle';
  const knob = document.createElement('span');
  knob.className = 'toggle-knob';
  sw.appendChild(knob);

  row.appendChild(label);
  row.appendChild(sw);
  row.onclick = () => { setPaletteSun(!paletteFollowsSun()); renderPaletteSun(); };
  box.appendChild(row);
}

function closeSettings() {
  $('settings-sheet').classList.add('hidden');
}

function renderThemeOptions() {
  const box = $('theme-options');
  const active = currentTheme();
  box.innerHTML = '';
  for (const key of THEMES) {
    const info = THEME_INFO[key];
    const row = document.createElement('button');
    row.className = 'theme-row';

    const name = document.createElement('span');
    name.className = 'theme-name';
    name.textContent = info.name;

    const strip = document.createElement('span');
    strip.className = 'swatches';
    for (const c of info.swatches) {
      const sw = document.createElement('span');
      sw.className = 'swatch';
      sw.style.background = c;   // CSSOM write — allowed by the style-src CSP
      strip.appendChild(sw);
    }

    const check = document.createElement('span');
    check.className = 'check';
    check.textContent = key === active ? '✓' : '';

    row.appendChild(name);
    row.appendChild(strip);
    row.appendChild(check);
    // Re-render the palette-sun control too: it appears only under Golden Hour.
    row.onclick = () => { setTheme(key); renderThemeOptions(); renderPaletteSun(); };
    box.appendChild(row);
  }
}

function renderNotifState() {
  const el = $('notif-state');
  const row = $('notif-row');
  const supported = 'Notification' in window &&
    'serviceWorker' in navigator && 'PushManager' in window;
  if (!supported) {
    el.textContent = 'Notifications · not supported here';
    row.disabled = true;
  } else if (Notification.permission === 'granted') {
    el.textContent = 'Notifications · On';
    row.disabled = true;
  } else if (Notification.permission === 'denied') {
    el.textContent = 'Notifications · blocked in iOS Settings';
    row.disabled = true;
  } else {
    el.textContent = 'Notifications · tap to enable';
    row.disabled = false;
  }
}

/* ---------- conversation list ---------- */

// Order: last activity (newest first); then live before offline; then name.
// Attention deliberately does NOT reorder (honest ordering) — the row gets an
// accent instead.
function listSort(a, b) {
  const am = lastActivityMs(a), bm = lastActivityMs(b);
  if (am !== bm) return bm - am;
  const ao = a.status === 'offline', bo = b.status === 'offline';
  if (ao !== bo) return ao ? 1 : -1;
  return (a.name || '').localeCompare(b.name || '');
}

// The subtle "You: <status>" line under the list header — your own away line,
// shown only when set. It mirrors exactly what an agent hears the moment it
// messages you (the daemon's AIM auto-responder). Rendered from renderList (the
// single hook every roster/status change already flows through) so it stays in
// sync without scattering calls.
function renderMyStatus() {
  const el = $('my-status');
  if (!el) return;
  const t = state.myStatus || '';
  el.textContent = t ? 'You: ' + t : '';
  el.classList.toggle('hidden', !t);
}

function renderList() {
  renderMyStatus();
  const list = $('contact-list');
  const empty = $('list-empty');
  // Onboarding holds until the first agent connects: a lone #crew row (a party
  // line with nobody in it) would only bury the "use bridge" hint. Once there's
  // at least one contact, the room joins the roster and sorts by activity like
  // any other row.
  if (!state.contacts.length) {
    list.innerHTML = '';
    empty.classList.remove('hidden');
    updateUnreadTotals();
    return;
  }
  empty.classList.add('hidden');
  const rows = state.contacts.slice();
  for (const r of state.rooms) rows.push({ id: r.id, name: r.name, room: true });
  rows.sort(listSort);
  list.innerHTML = '';
  for (const c of rows) list.appendChild(c.room ? makeRoomRow(c) : makeRow(c));
  updateUnreadTotals();
}

// The #crew row: a 📢 monogram (no presence dot — a room has no health), a
// persistent "everyone — party line" subtitle in place of an away line, the
// newest-message preview, and an unread badge — all from the same helpers a
// contact row uses, keyed on the room's string id.
function makeRoomRow(room) {
  const id = room.id;
  const row = document.createElement('button');
  row.className = 'row room';
  row.onclick = () => navigateToThread(id);

  const av = document.createElement('span');
  av.className = 'avatar room-avatar';
  av.textContent = '📢';
  row.appendChild(av);

  const main = document.createElement('span');
  main.className = 'row-main';

  const top = document.createElement('span');
  top.className = 'row-top';
  const name = document.createElement('span');
  name.className = 'row-name';
  name.textContent = room.name || '#crew';
  const time = document.createElement('span');
  time.className = 'row-time';
  const ms = lastActivityMs(room);
  time.textContent = ms ? listTime(ms) : '';
  top.appendChild(name);
  top.appendChild(time);
  main.appendChild(top);

  const st = document.createElement('span');
  st.className = 'row-status';
  st.textContent = 'everyone — party line';
  main.appendChild(st);

  const bottom = document.createElement('span');
  bottom.className = 'row-bottom';
  const preview = document.createElement('span');
  preview.className = 'row-preview';
  const p = roomPreview(id);
  if (p.cls) preview.classList.add(p.cls);
  preview.textContent = p.text;
  bottom.appendChild(preview);
  const n = unreadCount(id);
  if (n > 0) {
    const badge = document.createElement('span');
    badge.className = 'row-badge';
    badge.textContent = n > 99 ? '99+' : String(n);
    bottom.appendChild(badge);
  }
  main.appendChild(bottom);
  row.appendChild(main);
  return row;
}

// The room's own preview derivation — the newest room message, reusing the same
// helpers previewFor leans on. In a room an inbound bubble wears its author, so
// the preview prefixes the author's name (yours says "You:"). Empty when the
// room has no messages yet (the subtitle already says what it is).
function roomPreview(id) {
  const item = newestMessage(id);
  if (!item) return { text: '', cls: 'muted' };
  const out = item.type === 'sent' || item.mstate !== undefined;   // event vs outbox echo
  const body = item.image ? '📷 photo' : (plainPreview(item.text) || '📷 photo');
  const prefix = out ? 'You: ' : ((item.name || 'agent') + ': ');
  return { text: prefix + body };
}

function makeRow(contact) {
  const id = contact.id;
  const offline = contact.status === 'offline';
  const row = document.createElement('button');
  row.className = 'row' + (offline ? ' offline' : '') +
    (state.attentions.has(id) ? ' attn' : '');
  row.onclick = () => navigateToThread(id);

  const av = document.createElement('span');
  av.className = 'avatar';
  av.style.background = avatarColor(contact.name);
  av.textContent = monogram(contact.name);
  const sdot = document.createElement('span');
  sdot.className = 'status-dot ' + (offline ? 'offline' : (contact.health || 'ok'));
  av.appendChild(sdot);
  row.appendChild(av);

  const main = document.createElement('span');
  main.className = 'row-main';

  const top = document.createElement('span');
  top.className = 'row-top';
  // ── feature #18: transport badge ──────────────────────────────────────────
  // Name + an optional transport-flavor tag share one flex line, so the badge
  // hugs the name while the timestamp still floats to the right edge. tmux
  // contacts report no flavor and get no badge (see transportBadge).
  const nameLine = document.createElement('span');
  nameLine.className = 'name-line';
  const name = document.createElement('span');
  name.className = 'row-name';
  name.textContent = contact.name || 'contact';
  nameLine.appendChild(name);
  const badge = transportBadge(contact);
  if (badge) nameLine.appendChild(badge);
  // ── end #18 ───────────────────────────────────────────────────────────────
  const time = document.createElement('span');
  time.className = 'row-time';
  const ms = lastActivityMs(contact);
  time.textContent = ms ? listTime(ms) : '';
  top.appendChild(nameLine);
  top.appendChild(time);

  main.appendChild(top);
  // Away/status one-liner from the plugin fields map; omitted when absent so
  // it never leaves an empty gap.
  const status = contact.fields && contact.fields.status;
  if (status) {
    const st = document.createElement('span');
    st.className = 'row-status';
    st.textContent = status;
    main.appendChild(st);
  }

  const bottom = document.createElement('span');
  bottom.className = 'row-bottom';
  const preview = document.createElement('span');
  preview.className = 'row-preview';
  const p = previewFor(contact);
  if (p.cls) preview.classList.add(p.cls);
  preview.textContent = p.text;
  bottom.appendChild(preview);
  const n = unreadCount(id);
  if (n > 0) {
    const badge = document.createElement('span');
    badge.className = 'row-badge';
    badge.textContent = n > 99 ? '99+' : String(n);
    bottom.appendChild(badge);
  }

  main.appendChild(bottom);
  row.appendChild(main);
  return row;
}

/* ── feature #18: transport badge ──────────────────────────────────────────
   A small, secondary tag after a contact's name naming its client-hosted
   transport ("emacs" / "magnus" / …). The daemon sends contact.transport
   ("remote") and contact.transport_flavor on /api/status (httpapi.go); a tmux
   contact reports no flavor, so it gets nothing. Rooms build their own row
   (makeRoomRow) and never call this. Palette-native and quieter than the name
   or the presence dot, by design. */
function transportBadge(contact) {
  const flavor = contact && contact.transport_flavor;
  if (!flavor) return null;
  const badge = document.createElement('span');
  badge.className = 'transport-badge';
  badge.textContent = flavor;
  return badge;
}

// Monogram + a deterministic colour derived from the name — stable across
// loads (adjective-animal names like "swift-wolf" → "SW").
function monogram(name) {
  const parts = (name || '?').trim().split(/[\s_-]+/).filter(Boolean);
  if (!parts.length) return '?';
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

// Deterministic per-name hue → a 135° gradient (base hue to a darker stop),
// richer than a flat disc. Assigned via the CSSOM (el.style.background), which
// the strict style-src CSP allows — unlike a string style attribute.
function avatarColor(name) {
  let h = 0;
  const s = name || '';
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  const hue = h % 360;
  return 'linear-gradient(135deg, hsl(' + hue + ' 52% 50%), hsl(' + hue + ' 56% 34%))';
}

// The one-line row preview. Precedence per the charter: live typing → open
// prompt → away status (only when nothing is unread) → newest message → the
// contact's directory / a placeholder. The away line stands in for an already-
// read history, but a fresh reply must never hide behind a stale status — so
// unread previews win (spec).
function previewFor(contact) {
  const id = contact.id;
  if ((state.typing.get(id) || 0) > Date.now()) return { text: 'typing…', cls: 'typing' };
  if (contact.health === 'prompt') return { text: '🔔 needs your approval', cls: 'alert' };
  if (contact.away && unreadCount(id) === 0) return { text: '💬 ' + contact.away, cls: 'away' };
  const item = newestMessage(id);
  if (!item) return { text: contact.directory || 'no messages yet', cls: 'muted' };
  const out = item.type === 'sent' || item.mstate !== undefined;   // event vs outbox echo
  const body = item.image ? '📷 photo' : (plainPreview(item.text) || '📷 photo');
  const prefix = out ? 'You: ' : (item.type === 'peer' ? (item.name || 'agent') + ': ' : '');
  return { text: prefix + body };
}

// Newest message-like item touching the contact — a stored reply/mention/sent
// event or a not-yet-confirmed outbox echo — whichever is more recent.
function newestMessage(id) {
  let ev = null;
  for (let i = state.events.length - 1; i >= 0; i--) {
    const e = state.events[i];
    if (e.agent === id && (e.type === 'reply' || e.type === 'mention' ||
        e.type === 'sent' || e.type === 'peer')) {
      ev = e; break;
    }
  }
  let pend = null;
  for (const m of state.pending) if (m.agent === id) pend = m;   // last wins (chronological)
  if (ev && pend) return (Date.parse(pend.ts) || 0) >= (Date.parse(ev.ts) || 0) ? pend : ev;
  return ev || pend;
}

// Milliseconds of the last activity of ANY kind touching the contact (drives
// ordering). 0 for a contact the phone has never seen an event for.
function lastActivityMs(contact) {
  const id = contact.id;
  let ms = 0;
  for (let i = state.events.length - 1; i >= 0; i--) {
    if (state.events[i].agent === id) { ms = Date.parse(state.events[i].ts) || 0; break; }
  }
  for (const m of state.pending) {
    if (m.agent === id) { const t = Date.parse(m.ts) || 0; if (t > ms) ms = t; }
  }
  return ms;
}

// Strip thinking/response markers and markdown syntax, collapsing to a single
// preview line — a row preview renders as plain text, so literal **stars** and
// `backticks` are just noise there (spotted in the field, 2026-07-06).
function plainPreview(text) {
  return (text || '')
    .replace(/\[thinking\][\s\S]*?(?:\[end-thinking\]|\[\/thinking\]|(?=\[response\])|$)/g, '')
    .replace(/\[response\]/g, '')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/\*([^*\n]+)\*/g, '$1')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/^#+\s+/gm, '')
    .replace(/\s+/g, ' ')
    .trim();
}

function updateThreadHeader() {
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

function threadStatusText(contact) {
  if (!contact) return '';
  let s = 'live';
  if (contact.status === 'offline') {
    s = 'offline';
  } else {
    const h = HEALTH_LABELS[contact.health];
    if (h) s += ' · ' + h;
  }
  // The away line rides under the name, after presence/health.
  if (contact.away) s += ' · 💬 ' + contact.away;
  return s;
}

/* ── context gauge ───────────────────────────────────────────────────────────
   The agent's context-window usage, shown in the thread header UNDER the
   "vint · live · <status>" line, as a bar that IS a button. It appears only at
   ≥70% (below that: no bar, no noise), fills to the actual %, and ramps
   amber(--attn)→red(--danger) toward 100. It is tappable ONLY when the agent is
   idle — health "ok" is the one idle state; working / prompt / offline render it
   greyed and inert (you compact an agent who has put its pen down, not one
   mid-thought). A tap opens a confirm sheet; only on confirm does it POST
   /api/compact {agent:id} — never a typed command. Contract: /api/status may
   carry contact.context_pct (int 0-100, omitted when unknown); the daemon
   re-derives it after a compact, so the bar simply drops on the next poll.

   All per-contact fields (health, away, context_pct, …) ride on the raw contact
   objects assigned in refreshStatus/init — nothing to store separately. */

// One-time capability check: color-mix lets the fill blend the theme's own
// --attn/--danger so every palette stays native. Where it's absent, fall back
// to solid amber (still palette-native, just no ramp).
const CTX_COLOR_MIX = !!(window.CSS && CSS.supports &&
  CSS.supports('background', 'color-mix(in srgb, red 50%, blue)'));

// contact id -> ms until which an in-flight compact keeps the bar in its brief
// "compacting…" state. Cleared when it expires or the next poll drops the bar.
const compactState = new Map();
const COMPACT_GRACE_MS = 30000;   // one status-poll cycle: a hard ceiling on "compacting…"

let compactTarget = null;         // contact id the open confirm sheet acts on
let ctxToastTimer = null;

// Validate context_pct off a contact per the contract: an int 0-100, or null
// when the field is absent / not a number (unknown / sessionless → no bar).
function contextPct(contact) {
  if (!contact) return null;
  const v = contact.context_pct;
  if (typeof v !== 'number' || !isFinite(v)) return null;
  return Math.max(0, Math.min(100, Math.round(v)));
}

// Render (or remove) the header gauge for the selected contact. Called from
// updateThreadHeader, so it re-runs on thread-open, every SSE frame, and every
// status poll — the % moves and the enabled-state flips with health, live.
function renderContextGauge(contact) {
  const host = document.querySelector('.thread-header .thread-id');
  if (!host) return;
  let gauge = $('ctx-gauge');
  const pct = contextPct(contact);
  // No bar for rooms, unknown %, or anything below 70 — silence is the default.
  if (!contact || isRoomId(contact.id) || pct === null || pct < 70) {
    if (gauge) gauge.remove();
    return;
  }
  const id = contact.id;
  let busy = compactState.get(id) || 0;
  if (busy && busy <= Date.now()) { compactState.delete(id); busy = 0; }
  // health === "ok" is the ONLY idle state → the only time a compact is allowed.
  const enabled = contact.health === 'ok' && contact.status !== 'offline' && !busy;

  if (!gauge) {
    gauge = document.createElement('button');
    gauge.id = 'ctx-gauge';
    gauge.className = 'ctx-gauge';
    gauge.type = 'button';
    const track = document.createElement('span');
    track.className = 'ctx-gauge-track';
    const fill = document.createElement('span');
    fill.className = 'ctx-gauge-fill';
    track.appendChild(fill);
    const label = document.createElement('span');
    label.className = 'ctx-gauge-label';
    gauge.appendChild(track);
    gauge.appendChild(label);
    gauge.addEventListener('click', onGaugeTap);
    host.appendChild(gauge);
  }
  const fill = gauge.querySelector('.ctx-gauge-fill');
  const label = gauge.querySelector('.ctx-gauge-label');
  fill.style.width = pct + '%';
  // Amber at 70 → red toward 100: 0% danger at 70, 100% danger at 100. CSSOM
  // writes are CSP-allowed (like avatarColor). var() resolves per active theme.
  const mix = Math.max(0, Math.min(100, Math.round((pct - 70) / 30 * 100)));
  fill.style.background = CTX_COLOR_MIX
    ? 'color-mix(in srgb, var(--danger) ' + mix + '%, var(--attn))'
    : 'var(--attn)';
  label.textContent = busy ? 'compacting…' : (pct + '%');
  gauge.classList.toggle('ctx-busy', !!busy);
  gauge.disabled = !enabled;                       // a disabled button ignores taps
  gauge.setAttribute('aria-disabled', enabled ? 'false' : 'true');
  gauge.setAttribute('aria-label', busy
    ? ('compacting ' + (contact.name || threadName(id)) + '’s context')
    : ('context ' + pct + '% full' + (enabled ? ' — tap to compact' : '')));
}

// A tap on the bar. Disabled buttons don't fire click, but re-check the idle
// gate defensively (the roster can flip between render and tap).
function onGaugeTap() {
  const c = state.contacts.find((x) => x.id === state.selected);
  if (!c) return;
  const pct = contextPct(c);
  if (pct === null || pct < 70) return;
  if (c.health !== 'ok' || c.status === 'offline') return;   // idle-only
  if ((compactState.get(c.id) || 0) > Date.now()) return;    // already compacting
  openCompactConfirm(c);
}

// Build the confirm sheet + toast once (index.html is not ours to edit). The
// sheet reuses the settings-sheet chrome so it matches the app exactly.
function ensureCompactUI() {
  if ($('ctx-confirm')) return;
  const root = $('app') || document.body;

  const sheet = document.createElement('div');
  sheet.id = 'ctx-confirm';
  sheet.className = 'sheet hidden';
  sheet.innerHTML =
    '<div class="sheet-backdrop" id="ctx-confirm-backdrop"></div>' +
    '<div class="sheet-panel ctx-confirm-panel">' +
      '<div class="sheet-grip"></div>' +
      '<div class="sheet-title">Compact context</div>' +
      '<p class="ctx-confirm-msg" id="ctx-confirm-msg"></p>' +
      '<div class="ctx-confirm-actions">' +
        '<button type="button" class="ctx-btn primary" id="ctx-confirm-ok">Compact</button>' +
        '<button type="button" class="ctx-btn" id="ctx-confirm-cancel">Cancel</button>' +
      '</div>' +
    '</div>';
  root.appendChild(sheet);
  $('ctx-confirm-backdrop').addEventListener('click', closeCompactConfirm);
  $('ctx-confirm-cancel').addEventListener('click', closeCompactConfirm);
  $('ctx-confirm-ok').addEventListener('click', () => doCompact(compactTarget));

  const toast = document.createElement('div');
  toast.id = 'ctx-toast';
  toast.className = 'ctx-toast hidden';
  root.appendChild(toast);
}

function openCompactConfirm(contact) {
  ensureCompactUI();
  compactTarget = contact.id;
  const name = contact.name || threadName(contact.id) || 'this agent';
  $('ctx-confirm-msg').textContent =
    'Compact ' + name + '’s context? This summarizes the conversation so far to free up room.';
  $('ctx-confirm').classList.remove('hidden');
}

function closeCompactConfirm() {
  const el = $('ctx-confirm');
  if (el) el.classList.add('hidden');
  compactTarget = null;
}

// Confirmed: POST /api/compact (same device auth as every other /api/* POST via
// api()). Show a brief optimistic "compacting…" state; on 200 the next poll
// drops the bar; on 409/400/failure undo it and say so gently (never loudly).
async function doCompact(id) {
  closeCompactConfirm();
  if (!id) return;
  compactState.set(id, Date.now() + COMPACT_GRACE_MS);
  if (state.view === 'thread') updateThreadHeader();
  const res = await api('/api/compact', { agent: id });
  const name = threadName(id) || 'the agent';
  if (res && res.ok) return;                    // 200 {ok:true}: poll clears the bar
  compactState.delete(id);                      // undo the optimistic state
  if (state.view === 'thread') updateThreadHeader();
  if (res && res.status === 409) {
    showCompactToast(name + ' is working — try again in a moment');
  } else if (res && res.status === 400) {
    showCompactToast(name + ' is offline — try again in a moment');
  } else {
    showCompactToast('Couldn’t reach ' + name + ' — try again in a moment');
  }
}

function showCompactToast(text) {
  ensureCompactUI();
  const el = $('ctx-toast');
  if (!el) return;
  el.textContent = text;
  el.classList.remove('hidden');
  clearTimeout(ctxToastTimer);
  ctxToastTimer = setTimeout(() => el.classList.add('hidden'), 3200);
}

// Esc closes the confirm sheet (mirrors the action-sheet's dismissal).
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeCompactConfirm(); });
/* ── end context gauge ─────────────────────────────────────────────────────*/

// Total unread across contacts → app-icon badge (where supported) + a
// document.title prefix; both cleared when everything is read.
function updateUnreadTotals() {
  let total = 0;
  for (const c of state.contacts) total += unreadCount(c.id);
  for (const r of state.rooms) total += unreadCount(r.id);   // #crew unreads count too
  document.title = total > 0 ? '(' + total + ') bridge' : 'bridge';
  if ('setAppBadge' in navigator) {
    if (total > 0) navigator.setAppBadge(total).catch(() => {});
    else if ('clearAppBadge' in navigator) navigator.clearAppBadge().catch(() => {});
  }
}

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
  $('msg-input').placeholder = (g && g.agent === state.selected && g.until > Date.now())
    ? 'Tell ' + (threadName(state.selected) || 'the agent') + ' what to do differently…'
    : 'Message ' + (threadName(state.selected) || 'contact') + '…';
  $('send-btn').disabled = false;
  // Photos are a 1:1 feature in v1 — the room upload path has no fan-out, so a
  // photo to #crew would stick queued. Disable the attach button in a room.
  $('attach-btn').disabled = isRoomId(state.selected);
}

/* The "↑ show earlier" row at the top of a windowed feed. Widening the window
   re-renders taller, so we pin the viewport to the messages the reader was
   already looking at: record the height, grow, re-render, then push scrollTop
   down by exactly how much taller it got. The reader sits at the top when this
   fires (the button IS the top), so renderFeed's near-bottom stick can't trip,
   and pinFeedBottom never runs from here — the view holds where the eye is. */
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

function contactName(id) {
  const contact = state.contacts.find((c) => c.id === id);
  return contact && contact.name;
}

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

const BUBBLE_TARGET = 550;  // chars a bubble aims for
const BUBBLE_MAX = 900;     // a text (or lone paragraph) beyond this gets split

function splitPleasing(text) {
  if (!text || text.length <= BUBBLE_MAX) return [text];
  // Atomic units never split: [thinking] blocks and fenced code.
  const units = [];
  const atomicRe = /(\[thinking\][\s\S]*?(?:\[end[- ]?thinking\]|\[\/thinking\]|$)|```[\s\S]*?(?:```|$))/g;
  let last = 0, m;
  while ((m = atomicRe.exec(text)) !== null) {
    if (m.index > last) units.push(...text.slice(last, m.index).split(/\n{2,}/));
    units.push(m[1]);
    last = m.index + m[0].length;
  }
  if (last < text.length) units.push(...text.slice(last).split(/\n{2,}/));
  const clean = units.map((u) => u.trim()).filter(Boolean);

  // Pack paragraphs toward the target. A list stays glued to the short
  // intro that ends with ":"; a giant lone paragraph splits at sentences.
  const bubbles = [];
  let cur = '';
  for (const u of clean) {
    const atomic = /^(```|\[thinking\])/.test(u);
    if (u.length > BUBBLE_MAX && !atomic) {
      if (cur) { bubbles.push(cur); cur = ''; }
      bubbles.push(...sentencePack(u));
      continue;
    }
    const isList = /^([-*•]|\d+[.)])\s/.test(u);
    const fits = cur && cur.length + u.length + 2 <= BUBBLE_TARGET;
    const glued = cur && isList && /:\s*$/.test(cur);
    if (fits || glued) cur += '\n\n' + u;
    else { if (cur) bubbles.push(cur); cur = u; }
  }
  if (cur) bubbles.push(cur);
  return bubbles.length ? bubbles : [text];
}

function sentencePack(par) {
  const parts = par.split(/(?<=[.!?…])\s+/);
  const out = [];
  let cur = '';
  for (const s of parts) {
    if (cur && cur.length + s.length + 1 > BUBBLE_TARGET) { out.push(cur); cur = s; }
    else cur = cur ? cur + ' ' + s : s;
  }
  if (cur) out.push(cur);
  return out;
}

/* Local echo for an outgoing message. Its delivery state (sending / sent /
   failed / queued) shows as a glyph in the who-line; failed messages get a
   retry button that re-sends with the same client_id (safe — the server
   dedups). Replaced by the server's own "sent" event when it arrives. */
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

/* A photo in a fixed-size box (.photo-box owns the dimensions in CSS). The box
   reserves its full space before the image decodes, so a loading photo can't
   shift the feed — zero layout shift by construction. loading="lazy" keeps
   offscreen history photos off the wire until scrolled near; decoding="async"
   keeps the decode off the main thread. Tap toggles .full to lift the
   cover-crop and show the whole photo (no lightbox, no new chrome). */
function photoBox(src) {
  const box = document.createElement('div');
  box.className = 'photo-box';
  const img = document.createElement('img');
  img.src = src;
  img.alt = '';
  img.loading = 'lazy';
  img.decoding = 'async';
  box.appendChild(img);
  box.onclick = () => box.classList.toggle('full');
  return box;
}

function typingBubble(name) {
  const el = document.createElement('div');
  el.className = 'msg typing';
  const label = document.createElement('span');
  label.className = 'who';
  label.textContent = name + ' is working';
  el.appendChild(label);
  const dots = document.createElement('span');
  dots.className = 'dots';
  for (let i = 0; i < 3; i++) dots.appendChild(document.createElement('i'));
  el.appendChild(dots);
  return el;
}

function who(label) {
  const el = document.createElement('span');
  el.className = 'who';
  el.textContent = label;
  return el;
}

// Timestamp inside the bubble, bottom-right (styled by .msg .stamp). No-op
// when the event carries no parseable time.
function appendStamp(bubble, ts) {
  const t = localTime(ts);
  if (!t) return;
  const el = document.createElement('span');
  el.className = 'stamp';
  el.textContent = t;
  bubble.appendChild(el);
}

function localTime(ts) {
  const d = new Date(ts);
  return isNaN(d) ? '' :
    d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// iMessage-convention compact stamp for a conversation row:
// today → time; yesterday → "Yesterday"; this week → weekday; older → date.
function listTime(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return '';
  const days = daysAgo(d);
  if (days <= 0) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (days === 1) return 'Yesterday';
  if (days < 7) return d.toLocaleDateString([], { weekday: 'short' });
  return d.toLocaleDateString([], { month: 'numeric', day: 'numeric', year: '2-digit' });
}

// Day-separator label inside a thread feed.
function dayLabel(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return '';
  const days = daysAgo(d);
  if (days <= 0) return 'Today';
  if (days === 1) return 'Yesterday';
  if (days < 7) return d.toLocaleDateString([], { weekday: 'long' });
  return d.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric' });
}

// Whole calendar days between D and now (0 = today, 1 = yesterday, …).
function daysAgo(d) {
  const startOfDay = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  return Math.round((startOfDay(new Date()) - startOfDay(d)) / 86400000);
}

/* Render text with [thinking] blocks collapsed into tappable pills and
   very long remainders clamped behind "show more". */
function richText(text) {
  const container = document.createElement('div');
  container.className = 'rich';
  const re = /\[thinking\]([\s\S]*?)(?:\[end-thinking\]|\[\/thinking\]|(?=\[response\])|$)/g;
  let cursor = 0;
  let match;
  while ((match = re.exec(text)) !== null) {
    appendPlain(container, text.slice(cursor, match.index));
    appendThinking(container, match[1].trim());
    cursor = re.lastIndex;
  }
  appendPlain(container, text.slice(cursor).replace(/\[response\]/g, '').trim());
  return container;
}

function appendPlain(container, chunk) {
  chunk = chunk.trim();
  if (!chunk) return;
  const el = document.createElement('span');
  el.className = 'plain';
  if (chunk.length > 1200) {
    const short = chunk.slice(0, 1000) + '…';
    appendRich(el, short);
    const more = document.createElement('button');
    more.className = 'show-more';
    more.textContent = 'show more';
    more.onclick = () => {   // a toggle, not a one-way door
      const expanded = more.textContent === 'collapse';
      el.textContent = '';
      appendRich(el, expanded ? short : chunk);
      more.textContent = expanded ? 'show more' : 'collapse';
    };
    container.appendChild(el);
    container.appendChild(more);
  } else {
    appendRich(el, chunk);
    container.appendChild(el);
  }
}

/* Minimal inline markdown for bubbles — **bold**, *italic*, `code` — built
   from DOM nodes exactly like the linkifier (never innerHTML), so message
   content still cannot inject markup. Single-level on purpose: code spans
   don't linkify, bold/italic contents still do. Anything unmatched renders
   as the literal text it always was. */
function appendRich(parent, text) {
  const re = /(`[^`\n]+`)|(\*\*(?=\S)[^*]+?(?<=\S)\*\*)|(\*(?=\S)[^*\n]+?(?<=\S)\*)/g;
  let cursor = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > cursor) appendLinkified(parent, text.slice(cursor, m.index));
    if (m[1]) {
      const code = document.createElement('code');
      code.className = 'md-code';
      code.textContent = m[1].slice(1, -1);
      parent.appendChild(code);
    } else if (m[2]) {
      const b = document.createElement('strong');
      appendLinkified(b, m[2].slice(2, -2));
      parent.appendChild(b);
    } else {
      const i = document.createElement('em');
      appendLinkified(i, m[3].slice(1, -1));
      parent.appendChild(i);
    }
    cursor = m.index + m[0].length;
  }
  if (cursor < text.length) appendLinkified(parent, text.slice(cursor));
}

/* Append TEXT to PARENT, turning http(s) URLs into tappable links. Builds
   text and anchor nodes directly — never innerHTML — so message content
   cannot inject markup. Trailing sentence punctuation stays out of the href. */
function appendLinkified(parent, text) {
  const re = /https?:\/\/[^\s]+/g;
  let cursor = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    let url = m[0];
    const trail = url.match(/[.,!?;:'")\]}>]+$/);
    if (trail) url = url.slice(0, -trail[0].length);
    if (!url) continue;
    if (m.index > cursor) {
      parent.appendChild(document.createTextNode(text.slice(cursor, m.index)));
    }
    const a = document.createElement('a');
    a.href = url;                 // regex guarantees an http(s) scheme
    a.textContent = url;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    parent.appendChild(a);
    cursor = m.index + url.length;
  }
  if (cursor < text.length) {
    parent.appendChild(document.createTextNode(text.slice(cursor)));
  }
}

function appendThinking(container, thought) {
  if (!thought) return;
  const words = thought.split(/\s+/).length;
  const pill = document.createElement('button');
  pill.className = 'think-pill';
  pill.textContent = '💭 thinking · ' + words + ' words';
  const body = document.createElement('div');
  body.className = 'think-body hidden';
  body.textContent = thought;
  pill.onclick = () => {
    const open = body.classList.toggle('hidden');
    pill.textContent = open ? '💭 thinking · ' + words + ' words' : '💭 hide thinking';
  };
  container.appendChild(pill);
  container.appendChild(body);
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
function resolveAttentions(events) {
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

// First meaningful line of a captured prompt for the collapsed card: strip
// TUI box-drawing / bullet noise and return the first line with real content.
function firstLine(text) {
  const lines = (text || '').split('\n');
  let fallback = '';
  for (const raw of lines) {
    const cleaned = raw.replace(/[│╭╮╰╯─┌┐└┘|>❯•*\s]+/g, ' ').trim();
    if (/[A-Za-z0-9]/.test(cleaned)) return cleaned;
    if (!fallback && raw.trim()) fallback = raw.trim();
  }
  return fallback || '(prompt)';
}

/* An attention event. Live → the full tappable approval card. Resolved →
   collapsed: one dimmed prompt line + a resolution line, no buttons. */
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
function promptOptions(text) {
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
function offerGuidance(agentID, name) {
  state.guidance = { agent: agentID, until: Date.now() + 2 * 60 * 1000 };
  input.placeholder = 'Tell ' + (name || 'the agent') + ' what to do differently…';
  input.focus();
}

/* ---------- composer ---------- */

const input = $('msg-input');

function autogrow() {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 120) + 'px';
}

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
function setComposerFocusMode(on) {
  document.querySelectorAll('button, [href], input, [tabindex]').forEach((el) => {
    if (el === input) return;
    if (on) {
      if (!el.dataset.tabSaved) el.dataset.tabSaved = el.tabIndex;
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
  const file = e.target.files && e.target.files[0];
  e.target.value = '';
  if (!file) return;
  const dataUrl = await downscale(file).catch(() => null);
  if (!dataUrl) {
    // Never fail silently: a CSP block or an undecodable file used to end
    // here with no trace (found live 2026-07-06).
    alert('Couldn’t read that photo — try picking it again.');
    return;
  }
  pendingImage = dataUrl.split(',')[1];
  $('attach-thumb').src = dataUrl;
  $('attach-preview').classList.remove('hidden');
});

$('attach-remove').addEventListener('click', clearAttachment);

function clearAttachment() {
  pendingImage = null;
  $('attach-thumb').src = '';
  $('attach-preview').classList.add('hidden');
}

/* Re-encode client-side: caps dimensions at 2048px and always produces
   JPEG — smaller uploads and HEIC handled for free by the canvas. */
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

/* Long-press detection. A 500ms hold that doesn't drift into a scroll raises the
   sheet; the sheet's own overlay then intercepts the trailing synthetic click,
   so it can't close the sheet or fire a link under the finger (the backdrop
   handler ignores clicks in the first 350ms after opening). */
let lpTimer = null;
let lpStart = null;
let actionOpenedAt = 0;

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
    lpTimer = setTimeout(() => { lpStart = null; openActionSheet(meta, at); }, 500);
  }, { passive: true });
  el.addEventListener('touchmove', (e) => {
    if (!lpStart) return;
    const t = e.touches && e.touches[0];
    if (t && Math.hypot(t.clientX - lpStart.x, t.clientY - lpStart.y) > 10) {
      clearTimeout(lpTimer); lpStart = null;   // it's a scroll, not a press
    }
  }, { passive: true });
  el.addEventListener('touchend', () => { clearTimeout(lpTimer); lpStart = null; }, { passive: true });
  // Desktop: right-click / trackpad long-press raises the same sheet at the cursor.
  el.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    openActionSheet(meta, { x: e.clientX, y: e.clientY });
  });
}

function openActionSheet(meta, at) {
  const reactions = $('action-reactions');
  reactions.innerHTML = '';
  const mine = state.myReactions.get(meta.id);
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
    reactions.appendChild(b);
  }
  $('action-quote').onclick = () => { setQuote(meta); closeActionSheet(); };
  actionOpenedAt = Date.now();
  positionActionMenu(at);
}

function closeActionSheet() { $('action-sheet').classList.add('hidden'); }

// Raise the menu AT the press point (touch or cursor), in viewport coordinates.
// position:fixed makes {x,y} a viewport anchor — immune to the feed's scroll
// offset and any positioned ancestor (the old code anchored to the bubble's
// box, so a tall bubble threw the menu to the top of the screen). Reveal to
// measure, clamp 8px inside every edge, and flip ABOVE the finger when placing
// below it would spill past the bottom. CSSOM writes, allowed by the CSP.
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
function quoteExcerptText(text) {
  const clean = plainPreview(text) || (text || '').replace(/\s+/g, ' ').trim();
  return clean.slice(0, 80);
}

function setQuote(meta) {
  state.quote = { name: meta.name, excerpt: quoteExcerptText(meta.text) };
  renderQuoteChip();
  input.focus();
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
async function api(path, payload) {
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
async function deliver(msg) {
  if (msg.inflight) return;
  if (!navigator.onLine) {
    msg.mstate = 'queued'; savePending(); renderFeed(); return;
  }
  msg.inflight = true;
  msg.mstate = 'sending';
  savePending();
  renderFeed();
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

async function approve(agent, key) {
  await api('/api/approve', { agent, key });
}

/* ---------- notifications (best-effort; no-op where unsupported) ---------- */

// Trace push setup/errors without breaking the flow. Was called but never
// defined, so every push error path (incl. the catch) threw a ReferenceError.
const pushDebug = (m) => console.debug('[push]', m);

// Show the enable-notifications button whenever push is supported but not yet
// granted+subscribed. iOS requires the permission prompt to come from a tap.
function updatePushButton() {
  const btn = $('enable-push');
  if (!btn) return;
  const supported = 'Notification' in window && 'serviceWorker' in navigator && 'PushManager' in window;
  const granted = supported && Notification.permission === 'granted';
  btn.classList.toggle('hidden', !supported || granted);
  if (granted) enablePush();   // already allowed on a prior visit: (re)subscribe
}

$('enable-push').addEventListener('click', async () => {
  await requestNotifyPermission();
  updatePushButton();
});

async function requestNotifyPermission() {
  if (!('Notification' in window)) return;
  if (Notification.permission === 'default') {
    await Notification.requestPermission().catch(() => {});
  }
  if (Notification.permission === 'granted') enablePush();
}

/* Subscribe this device to Web Push so the daemon can ring it with the app
   closed. Idempotent — safe to call on every load once permission is granted. */
async function enablePush() {
  try {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) { return;
    }
    if (Notification.permission !== 'granted') { pushDebug('not granted'); return; }
    const reg = await navigator.serviceWorker.ready;
    let sub = await reg.pushManager.getSubscription();
    if (!sub) {
      const res = await fetch('/api/push/key');
      if (!res.ok) { pushDebug('key fetch failed ' + res.status); return; }
      const { key } = await res.json();
      sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlB64ToUint8Array(key),
      });
    }
    const subRes = await fetch('/api/push/subscribe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sub),
    });
    if (!subRes.ok) { pushDebug('subscribe failed ' + subRes.status); return; }
    pushDebug('subscribed');
  } catch (e) { pushDebug('ERROR ' + (e && e.message ? e.message : e)); }
}

function urlB64ToUint8Array(base64) {
  const pad = '='.repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  return Uint8Array.from([...raw].map((c) => c.charCodeAt(0)));
}

/* Page-side notification for events arriving while the tab is hidden. Routed
   through the service worker with the daemon's own tag scheme, so it REPLACES
   the matching Web Push instead of stacking a second, untagged banner beside
   it — and it becomes visible to clearDeliveredNotifications and tappable
   into the right thread, neither of which a bare page-scoped Notification
   was (review round 3 hygiene). Bare Notification remains as the fallback. */
function maybeNotify(event) {
  if (!('Notification' in window)) return;
  if (Notification.permission !== 'granted' || !document.hidden) return;
  let title, body, tag;
  if (event.type === 'attention') {
    title = (event.name || 'agent') + ' needs your attention';
    body = (event.text || '').slice(-120);
    tag = 'attn-' + event.agent;
  } else if (event.type === 'mention' || event.type === 'reply' || event.type === 'peer') {
    // A room peer already flows here; its agent id is the room, so the tag is
    // "msg-room:crew" (matching the daemon's push) and the title names the
    // author "in #crew" so a lock-screen banner says who and where.
    title = isRoomId(event.agent) ? (event.name || 'agent') + ' in #crew' : (event.name || 'bridge');
    body = (event.text || '').slice(0, 160);
    tag = 'msg-' + event.agent;
  } else {
    return;
  }
  navigator.serviceWorker.ready
    .then((reg) => reg.showNotification(title, {
      body, tag,
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      data: { contact: event.agent },
    }))
    .catch(() => { try { new Notification(title, { body }); } catch (e) { /* blocked */ } });
}

init();

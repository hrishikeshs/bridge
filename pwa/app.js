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

setInterval(updateFog, 30 * 60 * 1000);   // re-check the weather twice an hour
applyWallpaper(currentWallpaper());

const HEALTH_LABELS = { working: 'working', prompt: 'waiting on you', offline: 'offline' };
const STATE_GLYPH = { sending: '🕐', sent: '✓', failed: '⚠️', queued: '📮' };

const state = {
  contacts: [],          // roster from /api/status
  events: [],            // chronological event list
  attentions: new Map(), // contact id -> latest unresolved attention event
  view: 'list',          // 'list' (conversation list) | 'thread' (one contact)
  selected: null,        // contact id of the open thread, or null on the list
  lastEventId: 0,
  lastSeen: JSON.parse(localStorage.getItem('lastSeen') || '{}'),
  source: null,          // EventSource
  typing: new Map(),     // contact id -> expiry ms; fed by transient events
  connected: false,      // SSE open / last request reached the bridge
  pending: [],           // local echoes + outbox (see loadPending)
  guidance: null,        // {agent, until} — a No was tapped; next message is the "do this instead"
};

/* Outbox: unsent/undelivered messages, persisted so they survive an app
   restart. A restored "sending" message is unconfirmed, so it reverts to
   "queued" until a flush retries it. */
loadPending();

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
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
    // Notification tap (app already open): the SW posts the contact to open.
    // Validate it against the live roster before selecting — never trust a
    // value from outside to pick the target agent.
    navigator.serviceWorker.addEventListener('message', (e) => {
      if (e.data && e.data.type === 'open-contact') selectContactIfValid(e.data.contact);
    });
  }
  const res = await fetch('/api/status').catch(() => null);
  if (!res) return showOffline();          // unreachable: show cached feed
  if (res.status === 401) return showPairing();
  const data = await res.json();
  state.contacts = data.contacts || [];
  setConnected(true);
  showApp();
  await loadHistory();
  // With the roster and history loaded, land on the view named by the URL:
  // the list, or — cold-open from a notification tap (/?contact=<id> or
  // #/c/<id>) — straight into that contact's thread. restoreView validates
  // the id against the roster (a crafted id must not select a real target).
  restoreView();
  connectEvents();
  setInterval(refreshStatus, 30000);
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) {
      refreshStatus(); connectEvents(); flushOutbox();
      // Returning to the foreground on an open thread clears its unread.
      if (state.view === 'thread' && state.selected) markSeen(state.selected);
      renderList();
    }
  });
  window.addEventListener('online', () => { connectEvents(); flushOutbox(); });
  window.addEventListener('offline', () => setConnected(false));
  updatePushButton();          // iOS only prompts on a tap, so offer a button
}

/* Server unreachable (laptop asleep, daemon restarting): show the last
   cached conversation read-only, and retry until the bridge is back. */
function showOffline() {
  const cached = JSON.parse(localStorage.getItem('eventCache') || 'null');
  if (cached) {
    state.contacts = cached.contacts || [];
    cached.events.forEach(ingest);
  }
  showApp();
  setConnected(false);
  setTimeout(init, 5000);
}

function cacheEvents() {
  try {
    localStorage.setItem('eventCache', JSON.stringify({
      contacts: state.contacts,
      events: state.events.slice(-100),
    }));
  } catch (e) { /* storage full — cache is best-effort */ }
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
  const res = await fetch('/api/history?since=0').catch(() => null);
  if (!res || !res.ok) return;
  const data = await res.json();
  (data.events || []).forEach(ingest);
  pruneAttentions();   // history replay can resurrect orphaned attentions
  cacheEvents();
  renderFeed();
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
  source.onopen = () => {
    reconnectDelay = 1000;   // healthy link — reset the backoff
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

function updateBanner() {
  const banner = $('conn-banner');
  if (state.connected) {
    banner.className = 'banner hidden';
  } else if (!navigator.onLine) {
    banner.textContent = '📴 You’re offline';
    banner.className = 'banner offline';
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
        (e.type === 'reply' || e.type === 'mention')) n++;
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
}

function openThread(id) {
  state.view = 'thread';
  state.selected = id;
  document.documentElement.setAttribute('data-view', 'thread');  // scenery off
  $('thread-view').classList.remove('hidden');
  $('list-view').classList.add('hidden');
  if (!document.hidden) markSeen(id);
  updateThreadHeader();
  updateAttentionBanner();
  renderFeed();
  restoreDraft();
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
  if (id && state.contacts.some((c) => c.id === id)) openThread(id);
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
  if (id && state.contacts.some((c) => c.id === id)) navigateToThread(id);
  else showList();
}

window.addEventListener('popstate', routeFromLocation);

// Notification deep-link (SW postMessage, app already open). Validate the id
// against the roster so an unknown/crafted value can't select a real target.
function selectContactIfValid(id) {
  if (id && state.contacts.some((c) => c.id === id)) navigateToThread(id);
}

$('back-btn').addEventListener('click', () => history.back());

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

function openSettings() {
  renderThemeOptions();
  renderWallpaperOptions();
  renderNotifState();
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
    row.onclick = () => { setTheme(key); renderThemeOptions(); };
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

function renderList() {
  const list = $('contact-list');
  const empty = $('list-empty');
  if (!state.contacts.length) {
    list.innerHTML = '';
    empty.classList.remove('hidden');
    updateUnreadTotals();
    return;
  }
  empty.classList.add('hidden');
  const rows = state.contacts.slice().sort(listSort);
  list.innerHTML = '';
  for (const c of rows) list.appendChild(makeRow(c));
  updateUnreadTotals();
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
  const name = document.createElement('span');
  name.className = 'row-name';
  name.textContent = contact.name || 'contact';
  const time = document.createElement('span');
  time.className = 'row-time';
  const ms = lastActivityMs(contact);
  time.textContent = ms ? listTime(ms) : '';
  top.appendChild(name);
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
// prompt → newest message → the contact's directory / a placeholder.
function previewFor(contact) {
  const id = contact.id;
  if ((state.typing.get(id) || 0) > Date.now()) return { text: 'typing…', cls: 'typing' };
  if (contact.health === 'prompt') return { text: '🔔 needs your approval', cls: 'alert' };
  const item = newestMessage(id);
  if (!item) return { text: contact.directory || 'no messages yet', cls: 'muted' };
  const out = item.type === 'sent' || item.mstate !== undefined;   // event vs outbox echo
  const body = item.image ? '📷 photo' : (plainPreview(item.text) || '📷 photo');
  return { text: (out ? 'You: ' : '') + body };
}

// Newest message-like item touching the contact — a stored reply/mention/sent
// event or a not-yet-confirmed outbox echo — whichever is more recent.
function newestMessage(id) {
  let ev = null;
  for (let i = state.events.length - 1; i >= 0; i--) {
    const e = state.events[i];
    if (e.agent === id && (e.type === 'reply' || e.type === 'mention' || e.type === 'sent')) {
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
  const c = state.contacts.find((x) => x.id === state.selected);
  $('thread-name').textContent = (c && c.name) || 'contact';
  $('thread-status').textContent = threadStatusText(c);
}

function threadStatusText(contact) {
  if (!contact) return '';
  if (contact.status === 'offline') return 'offline';
  const h = HEALTH_LABELS[contact.health];
  return 'live' + (h ? ' · ' + h : '');
}

// Total unread across contacts → app-icon badge (where supported) + a
// document.title prefix; both cleared when everything is read.
function updateUnreadTotals() {
  let total = 0;
  for (const c of state.contacts) total += unreadCount(c.id);
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
  const events = visibleEvents();
  const resolutions = resolveAttentions(events);   // attn event id -> resolution
  let lastDay = '';
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
    if (!res && event.type === 'attention') {
      const c = state.contacts.find((x) => x.id === event.agent);
      if (!c || !c.attention) res = { kind: 'cleared', ts: null };   // orphaned: roster says no prompt
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
    ? 'Tell ' + (contactName(state.selected) || 'the agent') + ' what to do differently…'
    : 'Message ' + (contactName(state.selected) || 'contact') + '…';
  $('send-btn').disabled = false;
  $('attach-btn').disabled = false;
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
    el.appendChild(richText(event.text));
    appendStamp(el, event.ts);
  } else if (event.type === 'reply') {
    el.className = 'msg reply';
    el.appendChild(who(event.name || '?'));
    el.appendChild(richText(event.text));
    appendStamp(el, event.ts);
  } else if (event.type === 'mention') {
    el.className = 'msg mention';
    el.appendChild(who((event.name || 'contact') + ' · @mention'));
    el.appendChild(richText(event.text));
    appendStamp(el, event.ts);
  } else {
    el.className = 'msg system';
    el.textContent = (event.name || '') + ' · ' + event.type +
      (event.text ? ' · ' + event.text : '');
  }
  return el;
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
  if (msg.image) {
    const thumb = document.createElement('img');
    thumb.className = 'sent-thumb';
    thumb.src = 'data:image/jpeg;base64,' + msg.image;
    thumb.alt = '';
    el.appendChild(thumb);
  }
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
      if (open) { res.set(open.id, { kind: 'approved', ts: e.ts }); open = null; }
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
  } else {
    el.appendChild(who((event.name || '?') + ' needs your attention'));
    el.appendChild(promptExcerpt(event.text));
    el.appendChild(approveKeys(event));
  }
  return el;
}

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

/* Parse numbered options like "❯ 1. Yes" out of the prompt so buttons
   carry labels instead of bare digits. */
function promptOptions(text) {
  const options = [];
  for (const line of (text || '').split('\n')) {
    const m = line.match(/^\s*(?:❯\s*)?([123])\.\s*(.+?)\s*$/);
    if (m) options.push({ key: m[1], label: normalizeOption(m[2]) });
  }
  return options.length ? options : [
    { key: '1', label: 'Yes' }, { key: '3', label: 'No' }];
}

// Claude Code phrases its dialog options verbosely ("Yes, and don't ask
// again this session", "No, and tell Claude what to do differently…").
// On a phone card they normalize to the canonical trio; anything that
// doesn't match stays as (truncated) dialog text, so unusual dialogs keep
// their real choices. The tapped KEY is always the dialog's own number.
function normalizeOption(label) {
  if (/don'?t ask|always/i.test(label)) return 'Always';
  if (/^yes/i.test(label)) return 'Yes';
  if (/^no/i.test(label)) return 'No';
  return label.slice(0, 28);
}

function approveKeys(event) {
  const keys = document.createElement('div');
  keys.className = 'keys';
  for (const opt of promptOptions(event.text)) {
    const btn = document.createElement('button');
    btn.textContent = opt.label;
    btn.onclick = () => {
      approve(event.agent, opt.key);
      // "No" in Claude Code means "no — and tell me what to do instead":
      // the agent opens a guidance input, and the next phone message lands
      // straight in it. Teach that ("chat about this") instead of leaving
      // the user wondering what No did.
      if (opt.label === 'No') offerGuidance(event.agent, event.name);
    };
    keys.appendChild(btn);
  }
  const esc = document.createElement('button');
  esc.textContent = '⎋';
  esc.className = 'esc';
  esc.onclick = () => approve(event.agent, 'esc');
  keys.appendChild(esc);
  return keys;
}

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
  const msg = {
    clientId: crypto.randomUUID(),
    agent: state.selected,               // always the contact id
    name: contactName(state.selected) || '?',
    text: body,
    image: image || null,
    ts: new Date().toISOString(),
    mstate: 'sending',
    inflight: false,
  };
  state.pending.push(msg);
  state.guidance = null;   // whatever this message was, the No has its answer
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
  if (clientId) i = state.pending.findIndex((m) => m.clientId === clientId);
  if (i === -1) {
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
        image: m.image || null, ts: m.ts, mstate: m.mstate,
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

function maybeNotify(event) {
  if (!('Notification' in window)) return;
  if (Notification.permission !== 'granted' || !document.hidden) return;
  if (event.type === 'attention') {
    new Notification(event.name + ' needs your attention', {
      body: (event.text || '').slice(-120),
    });
  } else if (event.type === 'mention' || event.type === 'reply') {
    new Notification(event.name || 'bridge', {
      body: (event.text || '').slice(0, 160),
    });
  }
}

init();

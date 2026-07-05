/* bridge — phone client.
   Talks to the daemon over REST + Server-Sent Events. */

'use strict';

const $ = (id) => document.getElementById(id);

const HEALTH_GLYPHS = { ok: '●', working: '◐', prompt: '◉', offline: '○' };
const HEALTH_LABELS = { working: 'working', prompt: 'waiting on you', offline: 'offline' };
const STATE_GLYPH = { sending: '🕐', sent: '✓', failed: '⚠️', queued: '📮' };

const state = {
  contacts: [],          // roster from /api/status
  events: [],            // chronological event list
  attentions: new Map(), // contact id -> latest unresolved attention event
  selected: 'all',
  lastEventId: 0,
  lastSeen: JSON.parse(localStorage.getItem('lastSeen') || '{}'),
  source: null,          // EventSource
  typing: new Map(),     // contact id -> expiry ms; fed by transient events
  connected: false,      // SSE open / last request reached the bridge
  pending: [],           // local echoes + outbox (see loadPending)
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
  if (changed) renderFeed();
}, 2000);

/* ---------- bootstrap ---------- */

async function init() {
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  }
  const res = await fetch('/api/status').catch(() => null);
  if (!res) return showOffline();          // unreachable: show cached feed
  if (res.status === 401) return showPairing();
  const data = await res.json();
  state.contacts = data.contacts || [];
  setConnected(true);
  showApp();
  await loadHistory();
  connectEvents();
  setInterval(refreshStatus, 30000);
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) { refreshStatus(); connectEvents(); flushOutbox(); }
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
  renderTabs();
  renderFeed();
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
  renderTabs();
}

async function loadHistory() {
  const res = await fetch('/api/history?since=0').catch(() => null);
  if (!res || !res.ok) return;
  const data = await res.json();
  (data.events || []).forEach(ingest);
  cacheEvents();
  renderFeed();
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
      return;
    }
    const added = ingest(event);
    // Drop the local echo only once the server's own 'sent' event has been
    // accepted for render — otherwise a deduped event would remove the echo
    // and leave no bubble at all.
    if (added && event.type === 'sent') dropPendingEcho(event.agent, event.text);
    cacheEvents();
    renderFeed();
    renderTabs();
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
  $('conn-dot').classList.toggle('on', !!on);
  updateBanner();
}

/* Slim status banner under the header. Hidden when the bridge is reachable;
   distinguishes "phone has no network" from "phone online, Mac asleep". */
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

function hasUnread(contactId) {
  const seen = state.lastSeen[contactId] || 0;
  return state.events.some((e) =>
    e.agent === contactId && e.id > seen && e.type !== 'sent');
}

/* ---------- tabs ---------- */

function renderTabs() {
  const tabs = $('contact-tabs');
  tabs.innerHTML = '';
  tabs.appendChild(makeTab('all', 'All', null, null));
  // Server orders live contacts first, offline contacts last — preserve it.
  for (const contact of state.contacts) {
    tabs.appendChild(makeTab(contact.id, contact.name, contact.health, contact.status));
  }
}

function makeTab(id, label, health, status) {
  const el = document.createElement('button');
  const offline = status === 'offline';
  el.className = 'tab' + (state.selected === id ? ' active' : '') +
    (offline ? ' offline' : '');
  el.textContent = label;
  if (offline) {
    // Contact with chat history but no live session: readable, sends queue.
    el.appendChild(chip('offline-label', 'offline'));
  } else if (health) {
    const dot = document.createElement('span');
    dot.className = 'h ' + health;
    dot.textContent = HEALTH_GLYPHS[health] || '·';
    dot.title = HEALTH_LABELS[health] || health;
    el.prepend(dot);
  }
  if (state.attentions.has(id)) {
    el.appendChild(chip('bang', '!'));
  } else if (id !== 'all' && hasUnread(id)) {
    el.appendChild(chip('unread', ''));
  }
  el.onclick = () => {
    state.selected = id;
    if (id !== 'all') markSeen(id);
    renderTabs();
    renderFeed();
    restoreDraft();
  };
  return el;
}

function chip(cls, text) {
  const el = document.createElement('span');
  el.className = cls;
  el.textContent = text;
  return el;
}

/* ---------- feed ---------- */

function visibleEvents() {
  if (state.selected === 'all') return state.events;
  return state.events.filter((e) => e.agent === state.selected);
}

function visiblePending() {
  if (state.selected === 'all') return state.pending;
  return state.pending.filter((m) => m.agent === state.selected);
}

function renderFeed() {
  const feed = $('feed');
  const stick = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 60;
  feed.innerHTML = '';
  let lastDay = '';
  for (const event of visibleEvents()) {
    const day = (event.ts || '').slice(0, 10);
    if (day && day !== lastDay) {
      lastDay = day;
      const sep = document.createElement('div');
      sep.className = 'day-sep';
      sep.textContent = day;
      feed.appendChild(sep);
    }
    feed.appendChild(renderEvent(event));
  }
  for (const msg of visiblePending()) feed.appendChild(renderPending(msg));
  const now = Date.now();
  for (const [id, until] of state.typing) {
    if (until < now) continue;
    if (state.selected !== 'all' && state.selected !== id) continue;
    feed.appendChild(typingBubble(contactName(id) || 'contact'));
  }
  if (stick) feed.scrollTop = feed.scrollHeight;
  if (state.selected !== 'all') markSeen(state.selected);
  $('msg-input').placeholder = state.selected === 'all'
    ? 'Pick a contact to message…'
    : 'Message ' + (contactName(state.selected) || 'contact') + '…';
  const noContact = state.selected === 'all';
  $('send-btn').disabled = noContact;
  $('attach-btn').disabled = noContact;
}

function contactName(id) {
  const contact = state.contacts.find((c) => c.id === id);
  return contact && contact.name;
}

function renderEvent(event) {
  const el = document.createElement('div');
  if (event.type === 'sent') {
    el.className = 'msg sent';
    el.appendChild(who('you → ' + (event.name || '?'), event.ts));
    el.appendChild(richText(event.text));
  } else if (event.type === 'reply') {
    el.className = 'msg reply';
    el.appendChild(who(event.name || '?', event.ts));
    el.appendChild(richText(event.text));
  } else if (event.type === 'mention') {
    el.className = 'msg mention';
    el.appendChild(who((event.name || 'contact') + ' · @mention', event.ts));
    el.appendChild(richText(event.text));
  } else if (event.type === 'attention') {
    el.className = 'attention' +
      (state.attentions.get(event.agent) === event ? '' : ' resolved');
    el.appendChild(who((event.name || '?') + ' needs your attention', event.ts));
    el.appendChild(promptExcerpt(event.text));
    el.appendChild(approveKeys(event));
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
  const w = who('you → ' + msg.name, msg.ts);
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

function who(label, ts) {
  const el = document.createElement('span');
  el.className = 'who';
  el.textContent = label + (ts ? '  ' + localTime(ts) : '');
  return el;
}

function localTime(ts) {
  const d = new Date(ts);
  return isNaN(d) ? '' :
    d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
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
    appendLinkified(el, chunk.slice(0, 1000) + '…');
    const more = document.createElement('button');
    more.className = 'show-more';
    more.textContent = 'show more';
    more.onclick = () => {
      el.textContent = '';
      appendLinkified(el, chunk);
      more.remove();
    };
    container.appendChild(el);
    container.appendChild(more);
  } else {
    appendLinkified(el, chunk);
    container.appendChild(el);
  }
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

function promptExcerpt(text) {
  const pre = document.createElement('pre');
  const lines = (text || '').split('\n');
  if (lines.length > 4) {
    pre.textContent = lines.slice(-4).join('\n');
    const more = document.createElement('button');
    more.className = 'show-more';
    more.textContent = 'show full prompt';
    more.onclick = () => { pre.textContent = text; more.remove(); };
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
    if (m) options.push({ key: m[1], label: m[2].slice(0, 28) });
  }
  return options.length ? options : [
    { key: '1', label: 'Yes' }, { key: '3', label: 'No' }];
}

function approveKeys(event) {
  const keys = document.createElement('div');
  keys.className = 'keys';
  for (const opt of promptOptions(event.text)) {
    const btn = document.createElement('button');
    btn.textContent = opt.label;
    btn.onclick = () => approve(event.agent, opt.key);
    keys.appendChild(btn);
  }
  const esc = document.createElement('button');
  esc.textContent = '⎋';
  esc.className = 'esc';
  esc.onclick = () => approve(event.agent, 'esc');
  keys.appendChild(esc);
  return keys;
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
  if (!dataUrl) return;
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
  if ((!body && !image) || state.selected === 'all') return;
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
   Uploads arrive with a " 📷 photo" suffix the echo doesn't have. */
function dropPendingEcho(agent, text) {
  const bare = (text || '').replace(/\s*📷 photo$/, '').trim();
  const i = state.pending.findIndex((m) =>
    m.agent === agent && (m.text === (text || '') || m.text.trim() === bare));
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

function pushDebug(msg) {
  fetch('/api/push/debug', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ msg: String(msg), perm: (window.Notification && Notification.permission) }),
  }).catch(() => {});
}

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
  pushDebug('enable-push tapped');
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
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      pushDebug('unsupported'); return;
    }
    if (Notification.permission !== 'granted') { pushDebug('not granted'); return; }
    const reg = await navigator.serviceWorker.ready;
    pushDebug('sw ready');
    let sub = await reg.pushManager.getSubscription();
    if (!sub) {
      const res = await fetch('/api/push/key');
      if (!res.ok) { pushDebug('key fetch failed ' + res.status); return; }
      const { key } = await res.json();
      pushDebug('got key, subscribing');
      sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlB64ToUint8Array(key),
      });
      pushDebug('subscribed ok');
    } else {
      pushDebug('already had subscription');
    }
    const r = await fetch('/api/push/subscribe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sub),
    });
    pushDebug('posted subscription ' + r.status);
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

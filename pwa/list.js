// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — the conversation list (roster view).
   Peeled out of app.js (round 2 of the ES-module split); behaviour unchanged.

   The list screen: sorting (listSort), the Focus filter (applyFocus), the render
   pass (renderList), the contact and #crew rows (makeRow / makeRoomRow), their
   previews (previewFor / roomPreview), the avatar monogram + colour, the #18
   transport badge, and the unread-totals rollup (updateUnreadTotals: app-icon
   badge + document.title). It reads the message/unread math from
   ./messagemath.js, the row-preview flattener + list timestamp from
   ./textformat.js, and renderMyStatus from ./settings.js; from the core it takes
   $/state/isRoomId, navigateToThread (rows push history) and routeHold (the
   route-health preview phrase — humanizeAgo stays with it in the core). The spine
   imports renderList (every roster/status change) and updateUnreadTotals (the
   feed clears unread) back. */

'use strict';

import { $, state, isRoomId, navigateToThread, routeHold } from './app.js';
import {
  newestMessage, lastActivityMs, myLastSentMs, lastMessageMs, unreadCount,
} from './messagemath.js';
import { plainPreview, listTime } from './textformat.js';
import { renderMyStatus } from './settings.js';

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

// Focus mode: show only rows with activity in the last FOCUS_WINDOW_MS, but
// never fewer than FOCUS_FLOOR — an away-stretch fills up to the floor with the
// contacts you last MESSAGED (myLastSentMs), so the screen is never empty and
// the scenery can breathe. Off (the default) returns every row unchanged.
const FOCUS_WINDOW_MS = 20 * 60 * 1000;
const FOCUS_FLOOR = 2;
function applyFocus(rows) {
  if (!state.focus) return rows;
  const cut = Date.now() - FOCUS_WINDOW_MS;
  const shown = rows.filter((c) => lastMessageMs(c.id) >= cut);
  if (shown.length < FOCUS_FLOOR) {
    const byMine = rows.slice().sort((a, b) => myLastSentMs(b.id) - myLastSentMs(a.id));
    for (const c of byMine) {
      if (shown.length >= FOCUS_FLOOR) break;
      if (!shown.includes(c)) shown.push(c);
    }
    shown.sort(listSort);   // restore the natural activity order after filling
  }
  return shown;
}

export function renderList() {
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
  const shown = applyFocus(rows);
  list.innerHTML = '';
  for (const c of shown) list.appendChild(c.room ? makeRoomRow(c) : makeRow(c));
  updateUnreadTotals();
}

// Total unread across contacts → app-icon badge (where supported) + a
// document.title prefix; both cleared when everything is read.
export function updateUnreadTotals() {
  let total = 0;
  for (const c of state.contacts) total += unreadCount(c.id);
  for (const r of state.rooms) total += unreadCount(r.id);   // #crew unreads count too
  document.title = total > 0 ? '(' + total + ') bridge' : 'bridge';
  if ('setAppBadge' in navigator) {
    if (total > 0) navigator.setAppBadge(total).catch(() => {});
    else if ('clearAppBadge' in navigator) navigator.clearAppBadge().catch(() => {});
  }
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

/** @param {Contact} contact */
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
  // Route Health L1: a stuck route overrides the health dot — 'stale' (grey,
  // the remote client stopped attesting) or 'held' (amber, mail queued + blocked).
  // Absent hold_reason (healthy, or an older daemon) falls through to the health dot.
  const routeCls = contact.hold_reason === 'stale' ? 'stale'
    : contact.hold_reason ? 'held'
    : (contact.health || 'ok');
  sdot.className = 'status-dot ' + (offline ? 'offline' : routeCls);
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
/** @param {Contact} contact */
function previewFor(contact) {
  const id = contact.id;
  if ((state.typing.get(id) || 0) > Date.now()) return { text: 'typing…', cls: 'typing' };
  if (contact.health === 'prompt') return { text: '🔔 needs your approval', cls: 'alert' };
  const hold = routeHold(contact);
  if (hold) return { text: '⚠ ' + hold, cls: 'alert' };   // route-health: mail queued + blocked
  if (contact.away && unreadCount(id) === 0) return { text: '💬 ' + contact.away, cls: 'away' };
  const item = newestMessage(id);
  if (!item) return { text: contact.directory || 'no messages yet', cls: 'muted' };
  const out = item.type === 'sent' || item.mstate !== undefined;   // event vs outbox echo
  const body = item.image ? '📷 photo' : (plainPreview(item.text) || '📷 photo');
  const prefix = out ? 'You: ' : (item.type === 'peer' ? (item.name || 'agent') + ': ' : '');
  return { text: prefix + body };
}

/* bridge — context gauge (+ compact confirm/POST flow).
   Peeled out of app.js (round 1 of the ES-module split); behaviour unchanged.

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

'use strict';

import { $, state, isRoomId, threadName, api, updateThreadHeader } from './app.js';

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
export function renderContextGauge(contact) {
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

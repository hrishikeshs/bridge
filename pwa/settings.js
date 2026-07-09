// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — the settings sheet (+ the Focus toggle).
   Peeled out of app.js (round 2 of the ES-module split); behaviour unchanged.

   The gear's slide-up sheet — theme picker, wallpaper picker, "palette follows
   the sun" switch, the notification control, and the "my status" away line — plus
   the list-header Focus toggle. It imports the appearance setters/getters from
   ./appearance.js, the two notification entry points from ./notifications.js, and
   $/state from the core; toggleFocus re-renders the list, so renderList is
   imported back from the core too. renderMyStatus lives here (the sheet owns the
   away line) and is imported by the list module, which calls it from renderList.

   The event listeners (focus-btn / settings-btn / settings-close /
   settings-backdrop / notif-row / mystatus-*) wire at import time, the same
   startup point they occupied in app.js. That runs while the app.js↔settings.js
   ESM cycle is still evaluating — before the core's `$`/`state` bindings are
   live — so the wiring resolves nodes via document.getElementById directly ($ is
   exactly that alias) and the one eager setFocusButton() reads the persisted
   value straight from localStorage; every $/state use inside a handler runs
   later, once the bindings exist. */

'use strict';

import { $, state, renderList } from './app.js';
import {
  THEMES, THEME_INFO, currentTheme, setTheme,
  WALLPAPERS, WALLPAPER_NAMES, currentWallpaper, setWallpaper,
  paletteFollowsSun, setPaletteSun,
} from './appearance.js';
import { updatePushButton, requestNotifyPermission } from './notifications.js';

// The gear opens a slide-up sheet: a theme picker (applies + persists on tap)
// and the notification control (state + enable flow, relocated from the gear).
// Focus toggle: filter the list to recent chats (applyFocus). A persisted UI
// preference mirrored on state.focus — the same shape as the theme/wallpaper.
export function setFocusButton() {
  const b = $('focus-btn');
  if (b) b.classList.toggle('active', state.focus);
}
export function toggleFocus() {
  state.focus = !state.focus;
  localStorage.setItem('focus', state.focus ? '1' : '0');
  setFocusButton();
  renderList();
}
document.getElementById('focus-btn').addEventListener('click', toggleFocus);
// Eager initial paint of the toggle. This fires during the ESM cycle, before the
// core's `state` binding is live, so it reads the persisted flag from
// localStorage directly (identical to state.focus, which the core seeds the same
// way) rather than through setFocusButton()/state.
{
  const b = document.getElementById('focus-btn');
  if (b) b.classList.toggle('active', localStorage.getItem('focus') === '1');
}
document.getElementById('settings-btn').addEventListener('click', openSettings);
document.getElementById('settings-close').addEventListener('click', closeSettings);
document.getElementById('settings-backdrop').addEventListener('click', closeSettings);
document.getElementById('notif-row').addEventListener('click', async () => {
  await requestNotifyPermission();
  updatePushButton();
  renderNotifState();
});

// My status: the human's away line. 'change' fires on blur/Enter for a text
// input, so an edit saves when you leave the field; Enter blurs to commit it.
// The ✕ clears. Both POST /api/mystatus {text}; the daemon echoes a live
// 'mystatus' event so every open phone (and the next agent to reach out) syncs.
document.getElementById('mystatus-input').addEventListener('change', (e) => saveMyStatus(/** @type {HTMLInputElement} */ (e.target).value));
document.getElementById('mystatus-input').addEventListener('keydown', (e) => {
  if (/** @type {KeyboardEvent} */ (e).key === 'Enter') { e.preventDefault(); /** @type {HTMLInputElement} */ (e.target).blur(); }
});
// preventDefault on mousedown keeps the input focused when ✕ is tapped, so its
// blur doesn't fire a spurious 'change' (old value) that races the clear's POST.
document.getElementById('mystatus-clear').addEventListener('mousedown', (e) => e.preventDefault());
document.getElementById('mystatus-clear').addEventListener('click', () => { /** @type {HTMLInputElement} */ ($('mystatus-input')).value = ''; saveMyStatus(''); });

/** @param {string} text */
async function saveMyStatus(text) {
  // Mirror the daemon's clampAway: one line, capped — so the input and the
  // stored value never disagree (the daemon clamps again authoritatively).
  text = (text || '').replace(/[\r\n\t]+/g, ' ').trim().slice(0, 120);
  state.myStatus = text;
  /** @type {HTMLInputElement} */ ($('mystatus-input')).value = text;
  renderMyStatus();
  try {
    await fetch('/api/mystatus', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text }),
    });
  } catch (e) { /* offline — the daemon keeps its last value; a later edit retries */ }
}

export function openSettings() {
  renderThemeOptions();
  renderWallpaperOptions();
  renderPaletteSun();
  renderNotifState();
  /** @type {HTMLInputElement} */ ($('mystatus-input')).value = state.myStatus || '';
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
  const row = /** @type {HTMLButtonElement} */ ($('notif-row'));
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

// The subtle "You: <status>" line under the list header — your own away line,
// shown only when set. It mirrors exactly what an agent hears the moment it
// messages you (the daemon's AIM auto-responder). Rendered from renderList (the
// single hook every roster/status change already flows through) so it stays in
// sync without scattering calls.
export function renderMyStatus() {
  const el = $('my-status');
  if (!el) return;
  const t = state.myStatus || '';
  el.textContent = t ? 'You: ' + t : '';
  el.classList.toggle('hidden', !t);
}

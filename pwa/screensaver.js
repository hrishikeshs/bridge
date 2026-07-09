// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — feature #19(b): idle screensaver.
   Peeled out of app.js (round 1 of the ES-module split); behaviour unchanged.

   After IDLE_SCREENSAVER_MS of no interaction on the list, the list UI fades
   down to reveal the full scenery (Golden Gate under the drifting marine-layer
   fog, on its existing CSS animation). Any touch / scroll / key, a return to
   the foreground, or an incoming attention/message restores it instantly.
   Only the list view carries scenery, and only with a wallpaper set, so the
   idle timer arms there alone — a thread (words over solid ground) never dims.
   The class rides on <html>; the CSS fades #list-view and drops its pointer
   events, and onUserActivity swallows the waking gesture so the first tap only
   dismisses (it never also taps a row underneath). */

'use strict';

import { state, currentWallpaper } from './app.js';

const IDLE_SCREENSAVER_MS = 45000;
let screensaverTimer = null;

function screensaverEligible() {
  return state.view === 'list' && currentWallpaper() !== 'off' && !document.hidden;
}

export function armScreensaver() {
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
export function wakeScreensaver() {
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
/** @param {BridgeEvent} event @returns {boolean} */
export function isMomentEvent(event) {
  return event.type === 'attention' || event.type === 'reply' ||
         event.type === 'mention' || event.type === 'peer' || event.type === 'paper';
}

// Wire the document-level idle detection. Called once from init()'s one-time
// wiring guard. Pointer/key events are non-passive so a waking tap can be
// swallowed (dismiss only); scroll-class events are passive (just re-arm) to
// keep scrolling smooth. All capture so they still fire while the dimmed list
// is pointer-events:none.
export function initScreensaver() {
  ['touchstart', 'pointerdown', 'mousedown', 'keydown'].forEach(
    (t) => document.addEventListener(t, onUserActivity, { capture: true }));
  ['scroll', 'touchmove', 'wheel', 'mousemove'].forEach(
    (t) => document.addEventListener(t, onUserActivity, { capture: true, passive: true }));
}

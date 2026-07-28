// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — notifications (best-effort; no-op where unsupported).
   Peeled out of app.js (round 2 of the ES-module split); behaviour unchanged.

   Everything that touches the browser Notification / ServiceWorker / PushManager
   surface and the daemon's /api/push endpoints: the enable-push button state and
   its tap flow, the permission request, the Web Push subscribe, the base64→bytes
   VAPID-key decode, the page-side notification for a hidden tab, and clearing
   delivered notifications when the app is foregrounded. It imports $/state/
   isRoomId from the core; the spine calls the four entry points back
   (updatePushButton, requestNotifyPermission, maybeNotify,
   clearDeliveredNotifications). The $('enable-push') click listener rides along
   here — it only calls this module's own functions — and wires at import time,
   the same startup point it occupied in app.js. */

'use strict';

import { $, state, isRoomId } from './app.js';

/* iMessage behavior: opening the app clears its pile from Notification
   Center — everything a delivered banner said is richer in-app anyway. iOS
   never withdraws delivered notifications itself, so stale "needs your
   attention" banners otherwise outlive their prompts (field report,
   2026-07-06). Best-effort: notification support may be absent entirely. */
export async function clearDeliveredNotifications() {
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

// Trace push setup/errors without breaking the flow. Was called but never
// defined, so every push error path (incl. the catch) threw a ReferenceError.
const pushDebug = (m) => console.debug('[push]', m);

/* The user's own switch, persisted: '1' = notifications turned OFF on this
   phone from the settings sheet. The OS permission stays granted — JS has no
   way to hand a granted permission back — so "off" is an app-level choice:
   unsubscribe now AND gate every auto-(re)subscribe path until turned back on.
   Without the gate, the next app open would silently re-enable (line below:
   enablePush runs on every load once granted). */
const PUSH_OFF_KEY = 'push-off';
export function pushUserOff() { return localStorage.getItem(PUSH_OFF_KEY) === '1'; }

// Show the enable-notifications button whenever push is supported but not yet
// granted+subscribed. iOS requires the permission prompt to come from a tap.
// A user who turned notifications OFF in settings is not nagged by the banner.
export function updatePushButton() {
  const btn = $('enable-push');
  if (!btn) return;
  const supported = 'Notification' in window && 'serviceWorker' in navigator && 'PushManager' in window;
  const granted = supported && Notification.permission === 'granted';
  btn.classList.toggle('hidden', !supported || granted || pushUserOff());
  if (granted && !pushUserOff()) enablePush();   // already allowed on a prior visit: (re)subscribe
}

// Wired at import time (same startup point as in app.js). This runs while the
// app.js↔notifications.js ESM cycle is still evaluating — before app.js's `$`
// const is initialised — so it resolves the node via document.getElementById
// directly ($ is exactly that alias); every OTHER $ call in this module runs
// later, from the spine, once the binding is live.
document.getElementById('enable-push').addEventListener('click', async () => {
  await requestNotifyPermission();
  updatePushButton();
});

export async function requestNotifyPermission() {
  if (!('Notification' in window)) return;
  if (Notification.permission === 'default') {
    await Notification.requestPermission().catch(() => {});
  }
  if (Notification.permission === 'granted') enablePush();
}

/* The settings toggle's two directions. ON clears the off-flag first (so the
   subscribe paths are un-gated), then walks the normal permission flow — from
   'default' that shows the iOS prompt, which must come from a tap. OFF records
   the choice, silences the daemon first (an orphaned endpoint would otherwise
   only die by a later 410), then releases the browser-side subscription. Both
   halves of OFF are best-effort and independently recoverable: a failed POST is
   healed by the daemon's 410 pruning; a failed local unsubscribe is harmless
   because the daemon no longer knows the endpoint. */
/** @param {boolean} on */
export async function setPushEnabled(on) {
  if (on) {
    localStorage.removeItem(PUSH_OFF_KEY);
    await requestNotifyPermission();
    return;
  }
  localStorage.setItem(PUSH_OFF_KEY, '1');
  try {
    await fetch('/api/push/unsubscribe', { method: 'POST' });
  } catch (e) { pushDebug('unsubscribe POST failed (offline?) — daemon prunes on 410 later'); }
  try {
    if ('serviceWorker' in navigator) {
      const reg = await navigator.serviceWorker.ready;
      const sub = await reg.pushManager.getSubscription();
      if (sub) await sub.unsubscribe();
    }
  } catch (e) { pushDebug('local unsubscribe failed: ' + (e && e.message ? e.message : e)); }
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
        // TS 5.9's Uint8Array<ArrayBufferLike> vs BufferSource(ArrayBufferView<ArrayBuffer>)
        // generic mismatch — a lib-type nicety, not a runtime concern.
        applicationServerKey: /** @type {BufferSource} */ (urlB64ToUint8Array(key)),
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

/** @param {string} base64 @returns {Uint8Array} */
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
/** @param {BridgeEvent} event */
export function maybeNotify(event) {
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

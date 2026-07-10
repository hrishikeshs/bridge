/* bridge service worker — minimal shell cache.
   API calls always go to the network; the shell falls back to cache so the app
   opens instantly offline — and, since v42, on a STALLED network too (the
   lie-fi race below), not just on a hard network error. */

'use strict';

const CACHE = 'bridge-v43';
// app.js is a native ES module that imports these peeled modules; every one must
// be precached (a missed entry = a broken PWA offline on the phone). Bump CACHE
// whenever this list or any shell file changes.
const SHELL = ['/', '/style.css', '/app.js',
               '/screensaver.js', '/context-gauge.js', '/textformat.js', '/appearance.js',
               '/messagemath.js', '/notifications.js', '/settings.js', '/list.js',
               '/manifest.webmanifest',
               '/wallpaper.jpg',
               '/icons/icon-180.png', '/icons/icon-192.png', '/icons/icon-512.png'];

// How long a shell fetch may hang before the cache answers instead. The
// 2026-07-08 refactor review (C2, the hospital-corridor case): network-first is
// right when the link works, but a stalled-yet-connected link (1-bar cellular, a
// black-holed tailnet peer) neither resolves nor rejects for tens of seconds —
// while a complete precached shell sits on the device. 2.5s keeps any usable
// network first (shell fetches resolve well under it) and bounds a lie-fi open
// to a couple of module waves instead of a 30–60s white screen.
const STALL_MS = 2500;

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)));
  self.skipWaiting();
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
  );
  self.clients.claim();
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith('/api/')) return; // never intercept the API
  const network = fetch(e.request).then((res) => {
    // Only cache successful GETs — a transient 5xx (or a non-GET) must not
    // poison the offline shell (e.g. a bad /app.js served forever).
    if (e.request.method === 'GET' && res.ok) {
      const copy = res.clone();
      caches.open(CACHE).then((c) => c.put(e.request, copy));
    }
    return res;
  });
  e.respondWith((async () => {
    // Race the network against the stall timer. A working network wins and
    // behaves exactly as before (network-first, cache refreshed); hard-offline
    // rejects instantly into the cache fallback as before; a STALLED fetch now
    // loses to the timer and the precached shell opens, while the request keeps
    // running in the background (waitUntil below) so its eventual response
    // still lands in the cache for next time.
    const timer = new Promise((resolve) => { setTimeout(resolve, STALL_MS, undefined); });
    const winner = await Promise.race([network.catch(() => undefined), timer]);
    if (winner) return winner;
    const cached = await caches.match(e.request, { ignoreSearch: true });
    if (cached) return cached;
    return network; // nothing cached (first run): let the request run its course
  })());
  // Keep the worker alive so a stall-loser response still completes its cache.put.
  e.waitUntil(network.then(() => undefined, () => undefined));
});

// Web Push: the daemon fires these so the phone rings with the app closed.
self.addEventListener('push', (e) => {
  let d = { title: 'bridge', body: '' };
  try {
    // A `null` (or non-object) JSON body would make the d.title/d.body reads
    // below throw a TypeError and drop the notification — fall back to {}.
    const parsed = e.data.json();
    d = (parsed && typeof parsed === 'object') ? parsed : {};
  } catch (_) { if (e.data) d.body = e.data.text(); }
  const tag = d.tag || undefined;
  const seq = Number(d.seq) || 0;
  e.waitUntil((async () => {
    // Web Push has no ordering guarantee: a same-tag push REPLACES what's on
    // screen, so a stale "✓ handled" arriving late would erase a fresh "needs
    // you". The daemon stamps a per-tag sequence; never show a push older
    // than the notification already displayed for its tag.
    if (tag && seq) {
      const existing = await self.registration.getNotifications({ tag });
      if (existing.some((n) => n.data && Number(n.data.seq) > seq)) return;
    }
    await self.registration.showNotification(d.title || 'bridge', {
      body: d.body || '',
      tag,
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      data: { contact: d.contact || '', seq },   // deep-link target on tap + ordering
    });
  })());
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const contact = (e.notification.data && e.notification.data.contact) || '';
  const url = contact ? '/?contact=' + encodeURIComponent(contact) : '/';
  e.waitUntil(clients.matchAll({ type: 'window', includeUncontrolled: true })
    .then((wins) => {
      for (const w of wins) {
        if ('focus' in w) {
          if (contact) w.postMessage({ type: 'open-contact', contact });
          return w.focus();
        }
      }
      return clients.openWindow(url);
    }));
});

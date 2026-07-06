/* bridge service worker — minimal shell cache.
   API calls always go to the network; the shell falls back to cache
   so the app opens instantly (and shows the reconnect state) offline. */

'use strict';

const CACHE = 'bridge-v18';
const SHELL = ['/', '/style.css', '/app.js', '/manifest.webmanifest',
               '/wallpaper.jpg',
               '/icons/icon-192.png', '/icons/icon-512.png'];

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
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        // Only cache successful GETs — a transient 5xx (or a non-GET) must not
        // poison the offline shell (e.g. a bad /app.js served forever).
        if (e.request.method === 'GET' && res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(e.request, { ignoreSearch: true }))
  );
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
  e.waitUntil(self.registration.showNotification(d.title || 'bridge', {
    body: d.body || '',
    tag: d.tag || undefined,
    icon: '/icons/icon-192.png',
    badge: '/icons/icon-192.png',
    data: { contact: d.contact || '' },   // deep-link target on tap
  }));
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

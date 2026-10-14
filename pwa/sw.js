/* bridge service worker — minimal shell cache.
   API calls always go to the network; the shell falls back to cache so the app
   opens instantly offline — and, since v42, on a STALLED network too (the
   lie-fi race below), not just on a hard network error. */

'use strict';

const CACHE = 'bridge-v49';
// Unlike the replaceable shell cache, this tiny ledger is part of notification
// correctness: it remembers which daemon generation the phone has accepted
// even when the worker is replaced or the shell cache is upgraded.
const GENERATION_CACHE = 'bridge-notification-generation-v1';
const GENERATION_LEDGER_KEY = '/__bridge_notification_generation__';
// app.js is a native ES module that imports these peeled modules; every one must
// be precached (a missed entry = a broken PWA offline on the phone). Bump CACHE
// whenever this list or any shell file changes.
const SHELL = ['/', '/style.css', '/app.js',
               '/screensaver.js', '/context-gauge.js', '/textformat.js', '/appearance.js',
               '/messagemath.js', '/notifications.js', '/settings.js', '/list.js',
               '/export.js',
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
      Promise.all(keys.filter((k) => k !== CACHE && k !== GENERATION_CACHE)
        .map((k) => caches.delete(k))))
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

/* Notification generation ledger.

   The daemon's per-tag push sequence starts over after a restart. Comparing
   only that sequence would let either a fresh low sequence be rejected by an
   old high one, or a delayed old-generation push replace current state. The
   durable daemon generation orders those two worlds globally; server_epoch
   authenticates same-generation messages as belonging to the same process.

   Both push delivery and app messages use this one promise chain. Service
   worker events may overlap, and a read/modify/write ledger without this
   serialization can lose an advancement even though Cache Storage persists
   each individual write. */
let generationLedger;
let generationOperations = Promise.resolve();

function serializedGenerationOperation(fn) {
  const operation = generationOperations.then(fn, fn);
  // A failed Cache Storage operation must not poison every later event.
  generationOperations = operation.catch(() => undefined);
  return operation;
}

function emptyGenerationLedger() {
  return { generation: 0, epoch: '', sequences: Object.create(null) };
}

function corruptGenerationLedger() {
  return { generation: 0, epoch: '', sequences: Object.create(null), corrupt: true };
}

function validGeneration(value) {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
}

function normalizeSequence(value) {
  if (value === undefined || value === null || value === 0) return 0;
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0
    ? value : null;
}

function normalizeGenerationLedger(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value) ||
      !(value.generation === 0 || validGeneration(value.generation)) ||
      typeof value.epoch !== 'string' ||
      !value.sequences || typeof value.sequences !== 'object' ||
      Array.isArray(value.sequences)) return null;
  const epoch = value.epoch.trim();
  if ((value.generation === 0 && epoch) || (value.generation > 0 && !epoch)) return null;

  const ledger = { generation: value.generation, epoch, sequences: Object.create(null) };
  for (const [tag, seq] of Object.entries(value.sequences)) {
    const normalized = normalizeSequence(seq);
    if (!normalized) return null;
    ledger.sequences[tag] = normalized;
  }
  return ledger;
}

function copyGenerationLedger(ledger) {
  return {
    generation: ledger.generation,
    epoch: ledger.epoch,
    sequences: Object.assign(Object.create(null), ledger.sequences),
  };
}

async function loadGenerationLedger() {
  if (generationLedger) return generationLedger;
  const cache = await caches.open(GENERATION_CACHE);
  const response = await cache.match(GENERATION_LEDGER_KEY);
  if (!response) {
    generationLedger = emptyGenerationLedger();
    return generationLedger;
  }
  try {
    generationLedger = normalizeGenerationLedger(await response.json()) ||
      corruptGenerationLedger();
  } catch (_) {
    // Resetting corrupt durable state to generation 0 would admit delayed
    // legacy pushes. Fail closed until the open PWA supplies an authoritative
    // generation message, which can safely rebuild the ledger.
    generationLedger = corruptGenerationLedger();
  }
  return generationLedger;
}

async function storeGenerationLedger(ledger) {
  const cache = await caches.open(GENERATION_CACHE);
  await cache.put(GENERATION_LEDGER_KEY, new Response(JSON.stringify(ledger), {
    headers: { 'Content-Type': 'application/json' },
  }));
  generationLedger = ledger;
}

function notificationNamespace(notification) {
  const data = notification && notification.data ? notification.data : {};
  return {
    generation: validGeneration(data.server_generation) ? data.server_generation : 0,
    epoch: typeof data.server_epoch === 'string' ? data.server_epoch.trim() : '',
  };
}

async function retireNotificationsOutside(generation, epoch, authoritative) {
  const delivered = await self.registration.getNotifications();
  for (const notification of delivered) {
    const namespace = notificationNamespace(notification);
    const outside = namespace.generation !== generation || namespace.epoch !== epoch;
    const obsolete = namespace.generation < generation ||
      (namespace.generation === generation && namespace.epoch !== epoch);
    if ((authoritative && outside) || (!authoritative && obsolete)) notification.close();
  }
}

// Returns the accepted namespace plus a private `advanced` flag, or null when
// an event belongs to an old/unknown daemon. Missing generation is the legacy
// generation 0 and remains acceptable only until a modern generation arrives.
function acceptGeneration(ledger, data) {
  const hasGeneration = Object.prototype.hasOwnProperty.call(data, 'server_generation');
  if (!hasGeneration) {
    if (ledger.generation !== 0) return null;
    return { generation: 0, epoch: '', advanced: false };
  }

  const generation = data.server_generation;
  const epoch = typeof data.server_epoch === 'string' ? data.server_epoch.trim() : '';
  if (!validGeneration(generation) || !epoch || generation < ledger.generation) return null;
  if (generation === ledger.generation) {
    if (!ledger.epoch || ledger.epoch !== epoch) return null;
    return { generation, epoch, advanced: false };
  }
  return { generation, epoch, advanced: true };
}

async function advanceGeneration(data, tag, seq, authoritative = false) {
  const current = await loadGenerationLedger();
  let accepted;
  if (current.corrupt) {
    const generation = data.server_generation;
    const epoch = typeof data.server_epoch === 'string' ? data.server_epoch.trim() : '';
    if (!authoritative || !validGeneration(generation) || !epoch) return null;
    accepted = { generation, epoch, advanced: true };
  } else {
    accepted = acceptGeneration(current, data);
  }
  if (!accepted) return null;

  let next = current.corrupt ? emptyGenerationLedger() : copyGenerationLedger(current);
  let changed = false;
  if (accepted.advanced) {
    next.generation = accepted.generation;
    next.epoch = accepted.epoch;
    next.sequences = Object.create(null);
    // Commit the namespace before retiring old notifications. If cleanup is
    // interrupted, every later accepted modern event retries it below, so the
    // two durable surfaces converge rather than stranding an old notification.
    await storeGenerationLedger(next);
    next = copyGenerationLedger(next);
    changed = false;
  }

  if (accepted.generation > 0) {
    await retireNotificationsOutside(accepted.generation, accepted.epoch, authoritative);
  }

  // Sequence 0 means deliberately unsequenced: it neither consults nor moves
  // a tag's high-water mark. Positive sequences must strictly increase.
  if (tag && seq) {
    if (seq <= (next.sequences[tag] || 0)) return null;
    next.sequences[tag] = seq;
    changed = true;
  }

  if (changed) await storeGenerationLedger(next);
  return accepted;
}

// An open PWA learns the daemon generation from authoritative sync and sends
// it here. This closes stale lock-screen state immediately; no push has to
// arrive before the service worker learns that the daemon was replaced.
self.addEventListener('message', (e) => {
  const data = e.data;
  if (!data) return;
  if (data.type === 'server-generation') {
    e.waitUntil(serializedGenerationOperation(() => advanceGeneration({
      server_generation: data.generation,
      server_epoch: data.epoch,
    }, '', 0, true)));
    return;
  }
  if (data.type !== 'page-notification') return;
  const tag = typeof data.tag === 'string' && data.tag ? data.tag : undefined;
  e.waitUntil(serializedGenerationOperation(async () => {
    // Page-originated banners share the exact generation queue used by Web
    // Push. If displaying an old banner stalls while a new generation arrives,
    // the queued advancement necessarily retires it after display completes.
    const accepted = await advanceGeneration(data, tag, 0);
    if (!accepted) return;
    await self.registration.showNotification(data.title || 'bridge', {
      body: data.body || '',
      tag,
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      data: {
        contact: data.contact || '',
        seq: 0,
        server_generation: accepted.generation,
        server_epoch: accepted.epoch,
      },
    });
  }));
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
  const tag = typeof d.tag === 'string' && d.tag ? d.tag : undefined;
  const seq = normalizeSequence(d.seq);
  e.waitUntil(serializedGenerationOperation(async () => {
    if (seq === null) return;
    const accepted = await advanceGeneration(d, tag, seq);
    if (!accepted) return;
    await self.registration.showNotification(d.title || 'bridge', {
      body: d.body || '',
      tag,
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      // Deep-link target on tap plus enough provenance to retire delivered
      // notifications when a newer daemon generation is observed.
      data: {
        contact: d.contact || '',
        seq,
        server_generation: accepted.generation,
        server_epoch: accepted.epoch,
      },
    });
  }));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const contact = (e.notification.data && e.notification.data.contact) || '';
  const namespace = notificationNamespace(e.notification);
  let url = contact ? '/?contact=' + encodeURIComponent(contact) : '/';
  if (contact && namespace.generation && namespace.epoch) {
    url += '&server_generation=' + namespace.generation
      + '&server_epoch=' + encodeURIComponent(namespace.epoch);
  }
  e.waitUntil(clients.matchAll({ type: 'window', includeUncontrolled: true })
    .then((wins) => {
      for (const w of wins) {
        if ('focus' in w) {
          if (contact) w.postMessage({
            type: 'open-contact', contact,
            server_generation: namespace.generation,
            server_epoch: namespace.epoch,
          });
          return w.focus();
        }
      }
      return clients.openWindow(url);
    }));
});

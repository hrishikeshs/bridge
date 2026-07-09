'use strict';

/* ============================================================================
   bridge PWA — headless behaviour harness
   ----------------------------------------------------------------------------
   Loads the ASSEMBLED app (pwa/index.html's DOM + pwa/app.js) into jsdom, with
   every browser API the app touches replaced by a controllable fake, then hands
   tests a driver they can steer through the app's real seams:

     - a fake `fetch`      → canned /api/status & /api/history, a call log for
                             POST bodies, and per-path programmable responses
     - a fake `EventSource`→ tests inject SSE frames (attention/reply/hb/…)
     - a fake clock        → deterministic setTimeout/setInterval/Date.now/rAF
     - stubbed Notification / navigator.serviceWorker / PushManager / matchMedia
       / crypto.randomUUID

   The tests then assert on the resulting DOM and visible behaviour — never on
   internal functions. That is the whole point: after the upcoming ES-module
   split, the functions move, but the DOM contract and the browser seams do not,
   so these same tests keep their meaning.

   ┌──────────────────────────────────────────────────────────────────────────┐
   │  THE SPLIT SEAM                                                            │
   │  `APP_ENTRY` + `injectApp()` below are the ONE place the module-split      │
   │  agent repoints. Today they read pwa/app.js and inject it as a classic     │
   │  <script>. Post-split, repoint APP_ENTRY at the module entry (and, if the  │
   │  entry uses `import`, swap injectApp's body for a bundling step — see the  │
   │  note there). Nothing else in the harness or the tests should need to      │
   │  change.                                                                   │
   └──────────────────────────────────────────────────────────────────────────┘
   ========================================================================== */

const fs = require('node:fs');
const path = require('node:path');
const { JSDOM } = require('jsdom');
const esbuild = require('esbuild'); // TEST-ONLY: bundles the module graph for jsdom (see SPLIT SEAM 2/2)

const PWA_DIR = path.resolve(__dirname, '..', '..', 'pwa');
const INDEX_HTML = path.join(PWA_DIR, 'index.html');

// ⤵⤵⤵ SPLIT SEAM (1 of 2): the app entry the harness loads.
// app.js is the native-ESM entry AND the shared core: index.html loads it with
// <script type="module">, and it `import`s the peeled feature modules
// (./screensaver.js, ./context-gauge.js), which in turn import shared helpers
// back from it. esbuild follows this graph from APP_ENTRY at test time.
const APP_ENTRY = path.join(PWA_DIR, 'app.js');
// ⤴⤴⤴

// ⤵⤵⤵ SPLIT SEAM (2 of 2): how that entry is put into the page.
// The SHIPPED app is build-free native ES modules: the browser loads
// `<script type="module" src="/app.js">` and resolves each `import './x.js'` as
// a plain same-origin file over the network. jsdom, however, cannot resolve that
// `import` graph from disk off a <script type=module src>. So for the TEST load
// ONLY we bundle the graph with esbuild (buildSync → one classic IIFE, in-memory
// via write:false, never touching disk) and inject that string as a classic
// <script>. This keeps the shipped app build-free — esbuild runs only in this
// test process and pwa/*.js never imports it — while handing jsdom a single
// synchronous script, exactly as before the split. The bundle is memoised: the
// module graph is static within a run, so we build it at most once per process.
let _bundleCache = null;
function bundleApp() {
  if (_bundleCache === null) {
    const out = esbuild.buildSync({
      entryPoints: [APP_ENTRY],
      bundle: true,
      format: 'iife',
      charset: 'utf8', // keep emoji / em-dashes / curly quotes verbatim in the injected source
      write: false,
    });
    _bundleCache = out.outputFiles[0].text;
  }
  return _bundleCache;
}
function injectApp(win) {
  const script = win.document.createElement('script');
  script.textContent = bundleApp();
  win.document.body.appendChild(script); // the IIFE bundle runs synchronously here
}
// ⤴⤴⤴

/* ---------- a realistic canned roster (fields verified against httpapi.go) ---
   Contacts carry: id, name, directory, status ("live"|"offline"),
   health ("ok"|"working"|"prompt"|"offline"), attention (bool), away,
   fields, transport, transport_flavor, context_pct. Rooms: id, name.
   The status wrapper carries contacts, rooms, my_status, started, now. */
function defaultStatus() {
  return {
    contacts: [
      { id: 'vint',   name: 'vint',   directory: '~/workspace/bridge', status: 'live',    health: 'ok',      attention: false, transport: 'remote', transport_flavor: 'emacs' },
      { id: 'ludwig', name: 'ludwig', directory: '~/magnus',           status: 'live',    health: 'working', attention: false, transport: 'remote', transport_flavor: 'magnus' },
      { id: 'marvin', name: 'marvin', directory: '~/',                 status: 'offline', health: 'offline', attention: false },
    ],
    rooms: [{ id: 'room:crew', name: '#crew' }],
    my_status: '',
    version: 'test',
    now: 1720000000,
    started: 1720000000,
  };
}

/* ---------- small async utilities (test-side; use REAL node timers) ---------- */
function delay(ms) { return new Promise((r) => setTimeout(r, ms)); }
function flush(n = 4) {
  let p = Promise.resolve();
  for (let i = 0; i < n; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}
async function waitFor(pred, timeout = 3000) {
  const start = Date.now();
  while (Date.now() - start < timeout) {
    let ok = false;
    try { ok = !!pred(); } catch (_) { ok = false; }
    if (ok) return true;
    await delay(5);
  }
  try { return !!pred(); } catch (_) { return false; }
}

/* ---------- fake clock ------------------------------------------------------
   Installed on the jsdom window BEFORE app.js runs, so every bare
   setTimeout/setInterval/clearTimeout/clearInterval/requestAnimationFrame and
   Date.now() the app uses is deterministic and driven only by tick(). Only the
   window's timers are faked; the harness/test process keeps real node timers. */
function installClock(win) {
  let now = 1720000000000; // fixed base (ms)
  let seq = 1;
  const timers = new Map(); // id -> { cb, time, interval|null, args }

  win.Date.now = () => now;
  win.setTimeout = (cb, ms = 0, ...args) => { const id = seq++; timers.set(id, { cb, time: now + Math.max(0, ms), interval: null, args }); return id; };
  win.setInterval = (cb, ms = 0, ...args) => { const id = seq++; timers.set(id, { cb, time: now + Math.max(0, ms), interval: Math.max(1, ms), args }); return id; };
  win.clearTimeout = (id) => { timers.delete(id); };
  win.clearInterval = (id) => { timers.delete(id); };
  win.requestAnimationFrame = (cb) => win.setTimeout(() => cb(now), 0);
  win.cancelAnimationFrame = (id) => win.clearTimeout(id);

  return {
    now: () => now,
    tick(ms) {
      const target = now + ms;
      let guard = 0;
      while (true) {
        if (++guard > 200000) throw new Error('fake clock: too many timer iterations (runaway timer?)');
        let next = null;
        for (const [id, t] of timers) {
          if (t.time <= target && (next === null || t.time < next.t.time)) next = { id, t };
        }
        if (!next) break;
        now = next.t.time;
        const t = timers.get(next.id);
        if (!t) continue;
        if (t.interval) t.time = now + t.interval; else timers.delete(next.id);
        t.cb(...(t.args || [])); // let throws propagate — a real app error should fail the test
      }
      now = target;
    },
  };
}

/* ---------- fake EventSource ------------------------------------------------
   Instances register into `instances` so the harness can grab the live one and
   inject frames. Static CONNECTING/OPEN/CLOSED match the real API (app.js
   compares against EventSource.CLOSED). */
function makeEventSource(instances) {
  class FakeEventSource {
    constructor(url) {
      this.url = url;
      this.readyState = FakeEventSource.CONNECTING;
      this.onopen = null; this.onmessage = null; this.onerror = null;
      this._listeners = Object.create(null);
      instances.push(this);
    }
    addEventListener(type, fn) { (this._listeners[type] || (this._listeners[type] = [])).push(fn); }
    removeEventListener(type, fn) {
      const a = this._listeners[type]; if (!a) return;
      const i = a.indexOf(fn); if (i >= 0) a.splice(i, 1);
    }
    close() { this.readyState = FakeEventSource.CLOSED; }

    // ---- test controls ----
    emitOpen() {
      this.readyState = FakeEventSource.OPEN;
      const ev = { type: 'open' };
      if (this.onopen) this.onopen(ev);
      (this._listeners.open || []).forEach((f) => f(ev));
    }
    emitError() {
      const ev = { type: 'error' };
      if (this.onerror) this.onerror(ev);
      (this._listeners.error || []).forEach((f) => f(ev));
    }
    emitMessage(obj) {
      const ev = { type: 'message', data: typeof obj === 'string' ? obj : JSON.stringify(obj) };
      if (this.onmessage) this.onmessage(ev);
      (this._listeners.message || []).forEach((f) => f(ev));
    }
    // a NAMED SSE event (the app registers 'hb' via addEventListener)
    emitNamed(type, obj) {
      const ev = { type, data: obj == null ? undefined : (typeof obj === 'string' ? obj : JSON.stringify(obj)) };
      (this._listeners[type] || []).forEach((f) => f(ev));
    }
  }
  FakeEventSource.CONNECTING = 0;
  FakeEventSource.OPEN = 1;
  FakeEventSource.CLOSED = 2;
  return FakeEventSource;
}

/* ---------- fake fetch ------------------------------------------------------
   Records every call (method + parsed JSON body) and routes by path. Tests can
   override any path via harness.handlers[path] = (call) => ({status, body}). */
function toResponse(spec) {
  if (spec && typeof spec.json === 'function') return spec; // already response-like
  const status = (spec && spec.status) || 200;
  const body = spec ? spec.body : { ok: true };
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() { return body; },
    async text() { return JSON.stringify(body); },
    headers: { get() { return null; } },
  };
}
function makeFetch(harness) {
  return async function fetch(url, init = {}) {
    const method = (init.method || 'GET').toUpperCase();
    let body = null;
    if (init.body != null) { try { body = JSON.parse(init.body); } catch (_) { body = init.body; } }
    const call = { url: String(url), method, body, headers: init.headers || {} };
    harness.calls.push(call);
    const p = call.url.split('?')[0];

    const h = harness.handlers[p];
    let spec;
    if (typeof h === 'function') spec = h(call);
    else if (h) spec = h;
    else spec = harness.defaultResponse(p, call);
    return toResponse(spec);
  };
}

/* ---------- browser API stubs ----------------------------------------------- */
function installNotification(win, permission) {
  function Notification(title, opts) { this.title = title; this.opts = opts; }
  Notification.permission = permission || 'default';
  Notification.requestPermission = async () => Notification.permission;
  win.Notification = Notification;
}

function installServiceWorker(win) {
  const registration = {
    getNotifications: async () => [],
    showNotification: async () => {},
    pushManager: {
      getSubscription: async () => null,
      subscribe: async () => ({ endpoint: 'https://push.test/x', keys: {}, toJSON() { return { endpoint: 'https://push.test/x' }; } }),
    },
  };
  const sw = {
    register: async () => registration,
    addEventListener: () => {},
    ready: Promise.resolve(registration),
  };
  Object.defineProperty(win.navigator, 'serviceWorker', { configurable: true, value: sw });
  win.PushManager = function PushManager() {};
}

function installMisc(win, harness) {
  win.matchMedia = (q) => ({
    matches: false, media: q, onchange: null,
    addListener() {}, removeListener() {},
    addEventListener() {}, removeEventListener() {}, dispatchEvent() { return false; },
  });
  const uuid = () => 'uuid-' + (++harness._uuid);
  if (!win.crypto) { win.crypto = { randomUUID: uuid }; }
  else if (!win.crypto.randomUUID) {
    try { win.crypto.randomUUID = uuid; }
    catch (_) { Object.defineProperty(win, 'crypto', { configurable: true, value: Object.assign({}, win.crypto, { randomUUID: uuid }) }); }
  }
}

/* ---------- the loader ------------------------------------------------------ */
async function loadApp(opts = {}) {
  const status = opts.status || defaultStatus();
  const history = opts.history || [];
  const url = opts.url || 'http://localhost/';
  const notifPerm = opts.notificationPermission || 'default';

  const html = fs.readFileSync(INDEX_HTML, 'utf8');
  // runScripts: 'dangerously' lets our injected <script> run in the page's
  // context. The external <script src="/app.js"> in index.html is NOT fetched
  // (no resource loader configured), so it's a no-op — we inject app.js
  // ourselves, after the stubs are installed.
  // pretendToBeVisual makes document.hidden === false / visibilityState
  // 'visible' — the app gates markSeen and the screensaver on !document.hidden,
  // so without this it would behave as a backgrounded tab. jsdom's own rAF loop
  // (the only real timer pretendToBeVisual would start) never runs because
  // installClock overrides window.requestAnimationFrame right after this.
  const dom = new JSDOM(html, { url, runScripts: 'dangerously', pretendToBeVisual: true });
  const win = dom.window;

  // Optional localStorage seed, applied BEFORE the app runs — so a second load
  // can carry a first load's persisted state (e.g. the lastSeen unread cursor)
  // and prove it survives a "reopen". Additive: absent → a fresh, empty store.
  if (opts.localStorage) {
    for (const [k, v] of Object.entries(opts.localStorage)) win.localStorage.setItem(k, v);
  }

  const harness = {
    dom, win, document: win.document,
    status, history,
    calls: [],
    handlers: {},
    esInstances: [],
    _uuid: 0,
    clock: null,
  };

  harness.defaultResponse = (p /*, call */) => {
    if (p === '/api/status') return { status: 200, body: harness.status };
    if (p === '/api/history') return { status: 200, body: { events: harness.history } };
    if (p === '/api/push/key') return { status: 200, body: { key: 'AAAA' } };
    return { status: 200, body: { ok: true } };
  };

  // Install stubs BEFORE app.js runs — init() fires immediately on load.
  harness.clock = installClock(win);
  win.fetch = makeFetch(harness);
  win.EventSource = makeEventSource(harness.esInstances);
  installNotification(win, notifPerm);
  installServiceWorker(win);
  installMisc(win, harness);

  // Run the app (THE SPLIT SEAM lives in injectApp / APP_ENTRY above).
  injectApp(win);

  // init() is async (awaits /api/status then /api/history then connectEvents).
  // Wait until it has wired the stream (an EventSource exists) OR bounced to the
  // pairing screen; then flush pending rAFs so the DOM is settled.
  await waitFor(() => harness.esInstances.length > 0
    || (win.document.getElementById('pair-screen')
        && !win.document.getElementById('pair-screen').classList.contains('hidden')));
  harness.clock.tick(20);
  await flush();

  /* ---- driver helpers (all query the DOM / drive real seams) ---- */
  harness.$ = (id) => win.document.getElementById(id);
  harness.qs = (sel, root) => (root || win.document).querySelector(sel);
  harness.qsa = (sel, root) => Array.from((root || win.document).querySelectorAll(sel));
  harness.es = () => harness.esInstances[harness.esInstances.length - 1];
  harness.title = () => win.document.title;

  harness.callsTo = (p) => harness.calls.filter((c) => c.url.split('?')[0] === p);
  harness.lastCallTo = (p) => { const a = harness.callsTo(p); return a[a.length - 1]; };

  harness.flush = flush;
  harness.waitUntil = (pred, timeout) => waitFor(pred, timeout);
  harness.tick = async (ms) => { harness.clock.tick(ms); await flush(); };

  // Inject one SSE frame through the live stream and let the app render.
  harness.sse = async (obj) => { harness.es().emitMessage(obj); await flush(); };
  harness.sseNamed = async (type, obj) => { harness.es().emitNamed(type, obj); await flush(); };

  // Click a conversation-list row by its visible name (real navigation path).
  harness.openContact = async (name) => {
    const rows = harness.qsa('#contact-list .row');
    const row = rows.find((r) => {
      const n = r.querySelector('.row-name');
      return n && n.textContent === name;
    });
    if (!row) {
      const seen = rows.map((r) => r.querySelector('.row-name') && r.querySelector('.row-name').textContent);
      throw new Error('openContact: no row named "' + name + '" (saw: ' + seen.join(', ') + ')');
    }
    row.click();
    await flush();
    harness.clock.tick(20);
    await flush();
  };

  harness.click = async (idOrEl) => {
    const el = typeof idOrEl === 'string' ? harness.$(idOrEl) : idOrEl;
    if (!el) throw new Error('click: no element ' + idOrEl);
    el.click();
    await flush();
  };

  harness.dispatch = (type, target) => {
    const t = target || win.document;
    t.dispatchEvent(new win.Event(type, { bubbles: true, cancelable: true }));
  };

  harness.setNotifPermission = (p) => { win.Notification.permission = p; };

  harness.teardown = () => { try { win.close(); } catch (_) {} };

  return harness;
}

module.exports = { loadApp, defaultStatus, PWA_DIR, INDEX_HTML, APP_ENTRY };

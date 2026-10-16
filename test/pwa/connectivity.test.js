'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function unreachable() { throw new Error('tailnet route unavailable'); }

test('new JavaScript tolerates an older cached HTML shell', async (t) => {
  const h = await loadApp({
    beforeApp: (win) => win.document.getElementById('connection-help').remove(),
  });
  t.after(() => h.teardown());

  assert.ok(h.es(), 'the app initialized instead of crashing on optional modal bindings');
  assert.equal(h.$('app').classList.contains('hidden'), false);
});

test('a sustained online route failure explains Tailscale once', async (t) => {
  const h = await loadApp({
    allowOffline: true,
    handlers: { '/api/status': unreachable },
  });
  t.after(() => h.teardown());

  assert.equal(h.$('connection-help').classList.contains('hidden'), true,
    'a fast failure first gets the ordinary retry grace period');
  await h.tick(6000);

  const help = h.$('connection-help');
  assert.equal(help.classList.contains('hidden'), false);
  assert.equal(help.getAttribute('role'), 'dialog');
  assert.equal(help.getAttribute('aria-modal'), 'true');
  assert.match(help.textContent, /Tailscale is connected on this phone/i);
  assert.match(help.textContent, /Mac is awake/i);
  assert.match(help.textContent, /Bridge is running/i);
  assert.match(help.textContent, /Cached conversations may be out of date/i);
  assert.match(h.$('conn-banner').textContent, /Can’t reach Bridge/i);
  assert.equal(h.$('list-view').hasAttribute('inert'), true,
    'background controls are inert while the modal is open');

  await h.click('connection-help-dismiss');
  assert.equal(help.classList.contains('hidden'), true);
  assert.equal(h.$('list-view').hasAttribute('inert'), false);
  await h.tick(30000);
  assert.equal(help.classList.contains('hidden'), true,
    'automatic retries do not nag again during the same outage');
});

test('a black-holed status request times out and overlapping checks coalesce', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  let aborted = false;
  h.handlers['/api/status'] = (call) => new Promise((resolve, reject) => {
    call.signal.addEventListener('abort', () => {
      aborted = true;
      reject(new Error('aborted'));
    }, { once: true });
  });
  const before = h.callsTo('/api/status').length;
  h.dispatch('visibilitychange');
  h.dispatch('visibilitychange');
  await h.flush();
  assert.equal(h.callsTo('/api/status').length, before + 1,
    'foreground and overlapping checks share the in-flight request');

  await h.tick(5000);
  assert.equal(aborted, true, 'the black-holed request was bounded');
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  await h.tick(1000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), false);
});

test('newer SSE traffic wins over an older status timeout', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  h.handlers['/api/status'] = (call) => new Promise((resolve, reject) => {
    call.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
  });
  h.dispatch('visibilitychange');
  await h.flush();
  await h.sse({
    id: 1, type: 'reply', agent: 'vint', name: 'vint', text: 'still live',
    ts: '2026-10-16T00:00:00Z',
  });
  assert.ok(h.qsa('.dot.on').length > 0, 'live traffic marked Bridge reachable');
  await h.tick(5000); // the older status request now times out
  assert.ok(h.qsa('.dot.on').length > 0,
    'the stale timeout did not overwrite newer reachability proof');
  assert.equal(h.$('conn-banner').classList.contains('hidden'), true);
  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  assert.equal(h.$('conn-banner').classList.contains('hidden'), true);
});

test('connectivity help waits rather than stealing focus from the composer', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');
  h.$('msg-input').focus();

  h.handlers['/api/status'] = unreachable;
  h.dispatch('visibilitychange');
  await h.flush();
  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  assert.equal(h.document.activeElement, h.$('msg-input'));

  h.$('msg-input').blur();
  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), false);
});

test('quick background and foreground rearms a coalesced status probe', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  h.handlers['/api/status'] = (call) => new Promise((resolve, reject) => {
    call.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
  });
  const before = h.callsTo('/api/status').length;
  h.dispatch('visibilitychange');
  await h.flush();
  Object.defineProperty(h.document, 'hidden', { configurable: true, value: true });
  h.dispatch('visibilitychange');
  Object.defineProperty(h.document, 'hidden', { configurable: true, value: false });
  h.dispatch('visibilitychange');
  await h.flush();
  assert.equal(h.callsTo('/api/status').length, before + 1,
    'foreground coalesced onto the still-pending pre-hide request');
  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), false);
});

test('a successful direct module API call cancels the warning', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  h.handlers['/api/status'] = unreachable;
  h.dispatch('visibilitychange');
  await h.flush();
  await h.click('settings-btn');
  h.$('mystatus-input').value = 'away for a moment';
  h.dispatch('change', h.$('mystatus-input'));
  await h.flush();
  assert.equal(h.callsTo('/api/mystatus').length, 1);
  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  assert.equal(h.$('conn-banner').classList.contains('hidden'), true);
});

test('Try again reconnects immediately and a later outage can explain itself', async (t) => {
  let reachable = false;
  const h = await loadApp({
    allowOffline: true,
    handlers: {
      '/api/status': (call) => {
        if (!reachable) return unreachable();
        return h.defaultResponse('/api/status', call);
      },
    },
  });
  t.after(() => h.teardown());

  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), false);
  const before = h.callsTo('/api/status').length;
  reachable = true;
  await h.click('connection-help-retry');
  assert.ok(await h.waitUntil(() => h.esInstances.length > 0));
  assert.ok(h.callsTo('/api/status').length > before, 'retry made an immediate status request');
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  assert.equal(h.$('conn-banner').classList.contains('hidden'), true);

  h.handlers['/api/status'] = unreachable;
  h.dispatch('visibilitychange');
  await h.flush();
  await h.tick(6000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), false,
    'successful recovery reset the once-per-outage latch');
});

test('a phone with no network keeps the accurate offline banner and no Tailscale prompt', async (t) => {
  const h = await loadApp({
    allowOffline: true,
    online: false,
    handlers: { '/api/status': unreachable },
  });
  t.after(() => h.teardown());

  await h.tick(12000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  assert.match(h.$('conn-banner').textContent, /You’re offline/i);
});

test('an HTTP error proves the route and does not blame Tailscale', async (t) => {
  const h = await loadApp({
    allowOffline: true,
    handlers: { '/api/status': { status: 503, body: { error: 'starting' } } },
  });
  t.after(() => h.teardown());

  await h.tick(12000);
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
  assert.match(h.$('conn-banner').textContent, /Can’t reach Bridge/i);
});

test('a pairing failure routes to pairing without connectivity advice', async (t) => {
  const h = await loadApp({ handlers: { '/api/status': { status: 401, body: {} } } });
  t.after(() => h.teardown());

  await h.tick(12000);
  assert.equal(h.$('pair-screen').classList.contains('hidden'), false);
  assert.equal(h.$('connection-help').classList.contains('hidden'), true);
});

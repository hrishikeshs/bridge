'use strict';
/* A phone cache is a replica, never authority. Every wake synchronizes one
   daemon generation before SSE or mutations may resume. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp, defaultStatus } = require('./harness');

function event(id, text, extra = {}) {
  return { id, type: 'reply', agent: 'vint', name: 'vint', text,
    ts: '2026-10-14T10:00:00Z', ...extra };
}

function cachedReplica(epoch, events, extra = {}) {
  return JSON.stringify({
    version: 2,
    serverStarted: 100,
    serverEpoch: epoch,
    contacts: [],
    rooms: [],
    events,
    reactions: [],
    lastEventId: events.length ? events[events.length - 1].id : 0,
    ...extra,
  });
}

function pending(clientId = 'pending-1') {
  return [{
    clientId, agent: 'vint', name: 'vint', text: 'possibly accepted',
    image: null, quote: null, ts: '2026-10-14T09:00:00Z', mstate: 'queued',
  }];
}

function syncResponse(status, events, reset = false) {
  return {
    status: 200,
    body: { ...status, reset, events },
    headers: { 'X-Bridge-Server-Epoch': status.server_epoch },
  };
}

function deferred() {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

test('changed daemon generation replaces the replica and purges ambiguous pending sends', async (t) => {
  const status = defaultStatus();
  status.started = 200;
  status.server_epoch = 'epoch-new';
  const h = await loadApp({
    status,
    history: [event(7, 'server survived')],
    localStorage: {
      eventCache: cachedReplica('epoch-old', [event(900, 'phone-only stale')]),
      lastSeen: JSON.stringify({ vint: 900 }),
      outbox: JSON.stringify(pending()),
      theme: 'dark',
      'draft-vint': 'unfinished thought',
    },
  });
  t.after(() => h.teardown());

  assert.equal(h.callsTo('/api/sync')[0].url, '/api/sync?since=900&epoch=epoch-old');
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.equal(cache.serverStarted, 200);
  assert.equal(cache.serverEpoch, 'epoch-new');
  assert.deepEqual(cache.events.map((e) => [e.id, e.text]), [[7, 'server survived']]);
  assert.equal(h.win.localStorage.getItem('lastSeen'), null);
  assert.equal(h.win.localStorage.getItem('outbox'), null,
    'an unconfirmed send is not replayed into a replacement daemon');
  assert.equal(h.callsTo('/api/send').length, 0);
  assert.equal(h.win.localStorage.getItem('theme'), 'dark', 'device preference survived');
  assert.equal(h.win.localStorage.getItem('draft-vint'), 'unfinished thought', 'draft survived');
  assert.match(h.es().url, /epoch=epoch-new$/);
});

test('legacy cache migration requests a full authoritative sync once', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-upgraded';
  const legacy = JSON.stringify({
    contacts: [], rooms: [], events: [event(800, 'legacy stale')],
    reactions: [], lastEventId: 800,
  });
  const h = await loadApp({
    status,
    history: [event(8, 'fresh after upgrade')],
    localStorage: { eventCache: legacy },
  });
  t.after(() => h.teardown());

  assert.equal(h.callsTo('/api/sync')[0].url, '/api/sync?since=0');
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.deepEqual(cache.events.map((e) => e.id), [8]);
  assert.equal(cache.serverEpoch, 'epoch-upgraded');
});

test('unchanged generation uses a cheap event delta', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-same';
  const h = await loadApp({
    status,
    history: [event(50, 'cached'), event(51, 'delta')],
    localStorage: { eventCache: cachedReplica('epoch-same', [event(50, 'cached')]) },
  });
  t.after(() => h.teardown());

  assert.equal(h.callsTo('/api/sync')[0].url, '/api/sync?since=50&epoch=epoch-same');
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.deepEqual(cache.events.map((e) => e.id), [50, 51]);
});

test('the authoritative snapshot wins over older state-folding events', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-authoritative';
  status.my_status = 'current server status';
  status.contacts[0].away = 'current agent status';
  const h = await loadApp({
    status,
    history: [
      event(1, 'stale human status', { type: 'mystatus' }),
      event(2, 'stale agent status', { type: 'status' }),
    ],
  });
  t.after(() => h.teardown());

  assert.equal(h.$('my-status').textContent, 'You: current server status');
  await h.openContact('vint');
  assert.match(h.$('thread-status').textContent, /current agent status/);
  assert.doesNotMatch(h.$('thread-status').textContent, /stale agent status/);
});

test('foreground wake replaces the old stream and replica after restart', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-before';
  const h = await loadApp({ status, history: [event(100, 'before restart')] });
  t.after(() => h.teardown());
  const oldSource = h.es();

  status.started += 100;
  status.server_epoch = 'epoch-after';
  h.history.splice(0, h.history.length, event(1, 'after restart'));
  await h.openContact('vint');
  h.$('msg-input').value = 'queued during the gap';
  Object.defineProperty(h.win.navigator, 'onLine', { configurable: true, value: false });
  h.$('send-btn').click();
  Object.defineProperty(h.win.navigator, 'onLine', { configurable: true, value: true });

  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length >= 2
    && h.esInstances.length >= 2));
  await h.flush();

  assert.equal(oldSource.readyState, h.win.EventSource.CLOSED);
  assert.match(h.es().url, /since=1&epoch=epoch-after$/);
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.deepEqual(cache.events.map((e) => [e.id, e.text]), [[1, 'after restart']]);
  assert.equal(h.win.localStorage.getItem('outbox'), null);
  assert.equal(h.callsTo('/api/send').length, 0);
  assert.equal(h.$('list-view').classList.contains('hidden'), false,
    'a replacement daemon returns the UI to its authoritative roster');
  assert.equal(h.$('thread-view').classList.contains('hidden'), true);
  assert.equal(h.win.location.hash, '#/');
  assert.equal(h.$('thread-name').textContent, '');
  assert.equal(h.$('feed').childElementCount, 0, 'epoch-A bubbles were removed from the DOM');
});

test('failed sync keeps SSE and mutations fenced until a retry succeeds', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-before-failure';
  const h = await loadApp({ status, history: [event(9, 'old')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const oldSource = h.es();
  status.server_epoch = 'epoch-after-failure';
  h.history.splice(0, h.history.length, event(1, 'new'));
  h.handlers['/api/sync'] = { status: 503, body: { error: 'recovering' } };
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length >= 2));

  h.$('msg-input').value = 'must not cross the fence';
  h.$('send-btn').click();
  await h.flush();
  assert.equal(h.callsTo('/api/send').length, 0);
  assert.equal(h.esInstances.length, 1);

  delete h.handlers['/api/sync'];
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length >= 3
    && h.esInstances.length >= 2));
  await h.flush();

  assert.equal(oldSource.readyState, h.win.EventSource.CLOSED);
  assert.equal(h.callsTo('/api/send').length, 0, 'pending intent was purged on epoch change');
  assert.equal(h.win.localStorage.getItem('outbox'), null);
});

test('a failed wake sync retries with backoff instead of waiting for the poller', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-before';
  const h = await loadApp({ status, history: [event(8, 'before')] });
  t.after(() => h.teardown());

  status.server_epoch = 'epoch-after';
  h.history.splice(0, h.history.length, event(1, 'after'));
  let attempts = 0;
  h.handlers['/api/sync'] = (call) => {
    attempts++;
    if (attempts === 1) return { status: 503, body: { error: 'booting' } };
    return h.defaultResponse('/api/sync', call);
  };

  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => attempts === 1));
  await h.flush(); // let the failed response arm its retry timer
  await h.tick(1000);
  assert.ok(await h.waitUntil(() => attempts === 2 && h.esInstances.length >= 2));

  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.equal(cache.serverEpoch, 'epoch-after');
  assert.deepEqual(cache.events.map((e) => [e.id, e.text]), [[1, 'after']]);
});

test('a delayed wake check fences a send synchronously', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  const h = await loadApp({ status, history: [event(3, 'old')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const gate = deferred();
  h.handlers['/api/sync'] = () => gate.promise;
  h.dispatch('visibilitychange');
  h.$('msg-input').value = 'typed while checking';
  h.dispatch('input', h.$('msg-input'));
  h.$('send-btn').click();
  await h.flush();
  assert.equal(h.callsTo('/api/send').length, 0, 'send could not race the pending check');
  assert.equal(h.$('msg-input').value, 'typed while checking');
  assert.equal(h.win.localStorage.getItem('draft-vint'), 'typed while checking');

  status.server_epoch = 'epoch-b';
  gate.resolve(syncResponse(status, [event(1, 'replacement')], true));
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  await h.flush();
  assert.equal(h.callsTo('/api/send').length, 0);
  assert.equal(h.win.localStorage.getItem('outbox'), null);
  assert.equal(h.win.localStorage.getItem('draft-vint'), 'typed while checking',
    'a never-attempted send remains a draft across replacement');
});

test('a wake closes the old event stream before installing its snapshot', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-stream';
  status.contacts[0].away = 'snapshot truth';
  const h = await loadApp({ status, history: [event(3, 'before wake')] });
  t.after(() => h.teardown());
  const oldSource = h.es();
  const gate = deferred();
  h.handlers['/api/sync'] = () => gate.promise;

  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length >= 2));
  assert.equal(oldSource.readyState, h.win.EventSource.CLOSED,
    'the generation boundary synchronously retired the old stream');

  // This event was emitted after the sync endpoint took its event cut. A
  // closed stream may still have a queued browser callback, but it must not
  // consume the id before the replacement stream can replay it.
  oldSource.emitMessage(event(10, 'newer state', { type: 'status' }));
  gate.resolve(syncResponse(status, [], false));
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  assert.match(h.es().url, /since=3&epoch=epoch-stream$/);

  h.es().emitMessage(event(10, 'newer state', { type: 'status' }));
  await h.openContact('vint');
  assert.match(h.$('thread-status').textContent, /newer state/);
});

test('mutations name the daemon generation that rendered them', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-rendered';
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  h.$('msg-input').value = 'bound to this replica';
  h.$('send-btn').click();
  assert.ok(await h.waitUntil(() => h.callsTo('/api/send').length === 1));

  assert.equal(h.lastCallTo('/api/send').headers['X-Bridge-Server-Epoch'],
    'epoch-rendered');
});

test('a stale mutation response immediately purges and resyncs the replica', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-old';
  const h = await loadApp({ status, history: [event(9, 'old generation')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  status.server_epoch = 'epoch-new';
  h.history.splice(0, h.history.length, event(1, 'new generation'));
  h.handlers['/api/send'] = {
    status: 412,
    body: { error: 'server-restarted' },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-new' },
  };

  h.$('msg-input').value = 'must not be retried';
  h.$('send-btn').click();
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length >= 2
    && h.esInstances.length >= 2));
  await h.flush();

  assert.equal(h.callsTo('/api/send').length, 1);
  assert.equal(h.callsTo('/api/send')[0].headers['X-Bridge-Server-Epoch'], 'epoch-old');
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.equal(cache.serverEpoch, 'epoch-new');
  assert.deepEqual(cache.events.map((e) => [e.id, e.text]), [[1, 'new generation']]);
  assert.equal(h.win.localStorage.getItem('outbox'), null);
});

test('a sync response crossing a purge boundary cannot resurrect the old daemon', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  const h = await loadApp({ status, history: [event(9, 'epoch A')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const sendGate = deferred();
  const oldSyncGate = deferred();
  const newSyncGate = deferred();
  let wakeSyncs = 0;
  h.handlers['/api/send'] = () => sendGate.promise;
  h.handlers['/api/sync'] = () => (++wakeSyncs === 1 ? oldSyncGate.promise : newSyncGate.promise);

  // Start a mutation against A, then a wake sync against A. B answers the
  // mutation first and proves the replica must be purged while A's sync is
  // still in flight.
  h.$('msg-input').value = 'in flight against A';
  h.$('send-btn').click();
  assert.ok(await h.waitUntil(() => h.callsTo('/api/send').length === 1));
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => wakeSyncs === 1));

  status.server_epoch = 'epoch-b';
  h.history.splice(0, h.history.length, event(1, 'epoch B'));
  sendGate.resolve({
    status: 412,
    body: { error: 'server-restarted' },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-b' },
  });
  assert.ok(await h.waitUntil(() => h.win.localStorage.getItem('eventCache') === null));

  // A's delayed response arrives after the purge. It must not repaint or cache
  // anything while the catch-up request to B is waiting.
  const oldStatus = { ...defaultStatus(), server_epoch: 'epoch-a' };
  oldSyncGate.resolve(syncResponse(oldStatus, [event(10, 'late epoch A')], false));
  assert.ok(await h.waitUntil(() => wakeSyncs === 2));
  assert.equal(h.win.localStorage.getItem('eventCache'), null);
  assert.equal(h.qsa('#contact-list .row').length, 0);

  newSyncGate.resolve(syncResponse(status, [event(1, 'epoch B')], true));
  assert.ok(await h.waitUntil(() => {
    const raw = h.win.localStorage.getItem('eventCache');
    return raw && JSON.parse(raw).serverEpoch === 'epoch-b';
  }));
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.deepEqual(cache.events.map((e) => [e.id, e.text]), [[1, 'epoch B']]);
});

test('an old send completion cannot mark the replacement replica offline', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  const h = await loadApp({ status, history: [event(9, 'epoch A')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const sendGate = deferred();
  h.handlers['/api/send'] = () => sendGate.promise;
  h.$('msg-input').value = 'accepted by A eventually';
  h.$('send-btn').click();
  assert.ok(await h.waitUntil(() => h.callsTo('/api/send').length === 1));

  status.server_epoch = 'epoch-b';
  h.history.splice(0, h.history.length, event(1, 'epoch B'));
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));

  sendGate.resolve({
    status: 200,
    body: { ok: true },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-a' },
  });
  await h.flush();

  assert.equal(h.$('conn-banner').classList.contains('hidden'), true,
    'the late A continuation did not overwrite B connectivity');
  assert.equal(h.win.localStorage.getItem('outbox'), null,
    'the detached A message stayed purged');
  const cache = JSON.parse(h.win.localStorage.getItem('eventCache'));
  assert.equal(cache.serverEpoch, 'epoch-b');
  assert.deepEqual(cache.events.map((e) => [e.id, e.text]), [[1, 'epoch B']]);
});

test('a detached retry cannot resurrect an ambiguous send after replacement', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  h.handlers['/api/send'] = {
    status: 500,
    body: { error: 'uncertain' },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-a' },
  };
  h.$('msg-input').value = 'do not resurrect me';
  h.$('send-btn').click();
  assert.ok(await h.waitUntil(() => h.qs('#feed .retry')));
  const detachedRetry = h.qs('#feed .retry');
  assert.equal(h.callsTo('/api/send').length, 1);

  status.server_epoch = 'epoch-b';
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  detachedRetry.click();
  await h.flush();

  assert.equal(h.callsTo('/api/send').length, 1,
    'the purged message identity no longer belongs to the live outbox');
  assert.equal(h.win.localStorage.getItem('outbox'), null);
});

test('page notifications enter the worker generation queue with provenance', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  status.server_generation = 1;
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  Object.defineProperty(h.document, 'hidden', { configurable: true, value: true });
  h.win.Notification.permission = 'granted';

  await h.sse(event(9, 'late notification from A'));
  const pageMessageIndex = h.swMessages.findIndex((message) =>
    message.type === 'page-notification');
  assert.notEqual(pageMessageIndex, -1);
  assert.deepEqual(JSON.parse(JSON.stringify(h.swMessages[pageMessageIndex])), {
    type: 'page-notification',
    title: 'vint',
    body: 'late notification from A',
    tag: 'msg-vint',
    contact: 'vint',
    server_epoch: 'epoch-a',
    server_generation: 1,
  });

  status.server_epoch = 'epoch-b';
  status.server_generation = 2;
  h.history.splice(0, h.history.length, event(1, 'epoch B'));
  h.dispatch('online', h.win);
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  const replacementIndex = h.swMessages.findIndex((message, index) =>
    index > pageMessageIndex && message.type === 'server-generation'
      && message.generation === 2 && message.epoch === 'epoch-b');
  assert.notEqual(replacementIndex, -1,
    'the replacement generation followed the old page notification in the same worker queue');
});

test('a stale page never dismisses a notification from a newer generation', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  status.server_generation = 1;
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  let closed = false;
  h.swRegistration.getNotifications = async () => [{
    tag: 'msg-vint',
    data: { server_generation: 2, server_epoch: 'epoch-b', contact: 'vint' },
    close() { closed = true; },
  }];
  await h.openContact('vint');
  await h.flush();

  assert.equal(closed, false,
    'generation-A page cleanup preserved the worker\'s generation-B alert');
});

test('a push-disabled authoritative replica still clears delivered banners', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-push-disabled';
  status.server_generation = 0;
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  let closed = false;
  h.swRegistration.getNotifications = async () => [{
    tag: 'msg-vint',
    data: { server_generation: 9, server_epoch: 'retired', contact: 'vint' },
    close() { closed = true; },
  }];
  await h.openContact('vint');
  await h.flush();

  assert.equal(closed, true,
    'the current epoch remains authoritative even when push generation is unavailable');
});

test('notification navigation is bound to the replica that created it', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-b';
  status.server_generation = 2;
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  h.swMessage({
    type: 'open-contact', contact: 'vint',
    server_generation: 1, server_epoch: 'epoch-a',
  });
  await h.flush();
  assert.equal(h.$('list-view').classList.contains('hidden'), false,
    'a queued click from generation A was ignored by generation B');

  h.swMessage({
    type: 'open-contact', contact: 'vint',
    server_generation: 2, server_epoch: 'epoch-b',
  });
  await h.flush();
  assert.equal(h.$('thread-view').classList.contains('hidden'), false);
  assert.equal(h.$('thread-name').textContent, 'vint');
});

test('a cold notification deep-link is validated after authoritative sync', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-b';
  status.server_generation = 2;
  const h = await loadApp({
    status,
    history: [],
    url: 'http://localhost/?contact=vint&server_generation=1&server_epoch=epoch-a',
  });
  t.after(() => h.teardown());

  assert.equal(h.$('list-view').classList.contains('hidden'), false);
  assert.equal(h.win.location.hash, '#/');
});

test('a newly controlling service worker is taught the authoritative generation', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-current';
  status.server_generation = 7;
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  const replacementMessages = [];
  h.swControllerChange({ postMessage: (message) => replacementMessages.push(message) });
  await h.flush();

  assert.deepEqual(JSON.parse(JSON.stringify(replacementMessages)), [{
    type: 'server-generation', generation: 7, epoch: 'epoch-current',
  }]);
});

test('an old reaction rollback cannot erase a same-id reaction in the replacement daemon', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  const h = await loadApp({ status, history: [event(5, 'epoch A bubble')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const oldReactionGate = deferred();
  let reactionCalls = 0;
  h.handlers['/api/react'] = () => {
    reactionCalls++;
    if (reactionCalls === 1) return oldReactionGate.promise;
    return {
      status: 200,
      body: { ok: true },
      headers: { 'X-Bridge-Server-Epoch': status.server_epoch },
    };
  };
  h.dispatch('dblclick', h.qs('#feed .msg.reply'));
  const oldThumbs = h.qsa('#action-reactions .action-reaction')
    .find((button) => button.textContent === '👍');
  await h.click(oldThumbs);
  assert.equal(reactionCalls, 1);

  status.server_epoch = 'epoch-b';
  h.history.splice(0, h.history.length, event(5, 'epoch B bubble'));
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  await h.openContact('vint');
  h.dispatch('dblclick', h.qs('#feed .msg.reply'));
  const newThumbs = h.qsa('#action-reactions .action-reaction')
    .find((button) => button.textContent === '👍');
  await h.click(newThumbs);
  assert.equal(reactionCalls, 2);

  oldReactionGate.resolve({
    status: 500,
    body: { error: 'late epoch A failure' },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-a' },
  });
  await h.flush();

  h.dispatch('dblclick', h.qs('#feed .msg.reply'));
  const renderedThumbs = h.qsa('#action-reactions .action-reaction')
    .find((button) => button.textContent === '👍');
  assert.equal(renderedThumbs.disabled, true,
    'epoch B reaction state survived the late epoch A rollback');
});

test('an old interrupt completion cannot unlock a replacement interrupt', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const oldStopGate = deferred();
  const newStopGate = deferred();
  let stopCalls = 0;
  h.handlers['/api/interrupt'] = () => (++stopCalls === 1
    ? oldStopGate.promise : newStopGate.promise);
  h.$('stop-btn').click();
  assert.ok(await h.waitUntil(() => stopCalls === 1));

  status.server_epoch = 'epoch-b';
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  await h.openContact('vint');
  h.$('stop-btn').click();
  assert.ok(await h.waitUntil(() => stopCalls === 2));
  assert.equal(h.$('stop-btn').disabled, true);

  oldStopGate.resolve({
    status: 200,
    body: { ok: true },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-a' },
  });
  await h.flush();
  assert.equal(h.$('stop-btn').disabled, true,
    'the epoch A completion did not unlock epoch B\'s operation');

  newStopGate.resolve({
    status: 200,
    body: { ok: true },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-b' },
  });
  await h.flush();
  assert.equal(h.$('stop-btn').disabled, false);
});

test('an old compact failure cannot roll back replacement optimistic state', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-a';
  status.server_generation = 1;
  status.contacts[0].context_pct = 88;
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const oldCompactGate = deferred();
  const newCompactGate = deferred();
  let compactCalls = 0;
  h.handlers['/api/compact'] = () => (++compactCalls === 1
    ? oldCompactGate.promise : newCompactGate.promise);
  await h.click('ctx-gauge');
  await h.click('ctx-confirm-ok');
  assert.ok(await h.waitUntil(() => compactCalls === 1));

  status.server_epoch = 'epoch-b';
  status.server_generation = 2;
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  await h.openContact('vint');
  await h.click('ctx-gauge');
  await h.click('ctx-confirm-ok');
  assert.ok(await h.waitUntil(() => compactCalls === 2));
  assert.equal(h.qs('.ctx-gauge-label').textContent, 'compacting…');

  oldCompactGate.resolve({
    status: 500,
    body: { error: 'late epoch A failure' },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-a' },
  });
  await h.flush();
  assert.equal(h.qs('.ctx-gauge-label').textContent, 'compacting…',
    'the old rollback did not clear epoch B optimistic state');
  assert.equal(h.$('ctx-toast').classList.contains('hidden'), true);

  newCompactGate.resolve({
    status: 200,
    body: { ok: true },
    headers: { 'X-Bridge-Server-Epoch': 'epoch-b' },
  });
  await h.flush();
});

test('detached controls from an old epoch cannot mutate the new daemon', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-old';
  status.contacts[0].attention = true;
  const oldAttention = event(5, '1. Yes\n3. No', { type: 'attention' });
  const h = await loadApp({ status, history: [oldAttention] });
  t.after(() => h.teardown());
  await h.openContact('vint');
  const detached = h.qs('#feed .keys .key-opt');
  assert.ok(detached);

  status.server_epoch = 'epoch-new';
  h.history.splice(0, h.history.length,
    event(5, '1. Approve an unrelated new prompt\n3. No', { type: 'attention' }));
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));

  detached.click();
  await h.flush();
  assert.equal(h.callsTo('/api/approve').length, 0);
});

test('a detached forward picker cannot repost old text into a new daemon', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-old';
  const h = await loadApp({ status, history: [event(5, 'old words')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  h.dispatch('contextmenu', h.qs('#feed .msg.reply'));
  await h.click('action-forward');
  const detached = h.qsa('#forward-list .forward-row').find((row) =>
    row.querySelector('.forward-row-name').textContent === 'ludwig');
  assert.ok(detached);

  status.server_epoch = 'epoch-new';
  h.history.splice(0, h.history.length, event(1, 'new words'));
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));

  detached.click();
  await h.flush();
  assert.equal(h.callsTo('/api/send').length, 0);
  assert.equal(h.qs('#app-toast'), null,
    'a rejected stale forward did not claim success');
});

test('an open settings sheet cannot write stale status into a new daemon', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-old';
  status.my_status = 'old server status';
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  await h.click('settings-btn');
  const input = h.$('mystatus-input');
  input.value = 'unsaved epoch A edit';

  status.server_epoch = 'epoch-new';
  status.my_status = 'epoch B truth';
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.esInstances.length >= 2));
  assert.equal(h.$('settings-sheet').classList.contains('hidden'), true);
  assert.equal(h.$('my-status').textContent, 'You: epoch B truth');

  // Even a delayed/detached change event from the old editor is inert.
  input.value = 'late epoch A edit';
  h.dispatch('change', input);
  await h.flush();
  assert.equal(h.callsTo('/api/mystatus').length, 0);
  assert.equal(h.$('my-status').textContent, 'You: epoch B truth');
});

test('overlapping wake signals share one in-flight sync and request one catch-up pass', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-one';
  const h = await loadApp({ status, history: [] });
  t.after(() => h.teardown());

  const gate = deferred();
  h.handlers['/api/sync'] = () => gate.promise;
  const before = h.callsTo('/api/sync').length;
  h.dispatch('visibilitychange');
  h.dispatch('online', h.win);
  const pageShow = new h.win.Event('pageshow');
  Object.defineProperty(pageShow, 'persisted', { value: true });
  h.win.dispatchEvent(pageShow);
  await h.flush();
  assert.equal(h.callsTo('/api/sync').length, before + 1);

  gate.resolve(syncResponse(status, [], false));
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length === before + 2));
  await h.flush();
});

test('missing epoch invalidates the cache and leaves mutations read-only', async (t) => {
  const status = defaultStatus();
  status.server_epoch = 'epoch-present';
  const h = await loadApp({ status, history: [event(4, 'before')] });
  t.after(() => h.teardown());
  await h.openContact('vint');

  h.handlers['/api/sync'] = {
    status: 200,
    body: { ...status, server_epoch: '', reset: false, events: [event(5, 'untrusted')] },
  };
  h.dispatch('visibilitychange');
  assert.ok(await h.waitUntil(() => h.callsTo('/api/sync').length >= 2));
  await h.flush();

  assert.equal(h.win.localStorage.getItem('eventCache'), null);
  h.$('msg-input').value = 'do not send';
  h.$('send-btn').click();
  await h.flush();
  assert.equal(h.callsTo('/api/send').length, 0);
});

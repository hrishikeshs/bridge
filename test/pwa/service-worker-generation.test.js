'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const workerSource = fs.readFileSync(path.join(__dirname, '../../pwa/sw.js'), 'utf8');

function requestKey(request) {
  return typeof request === 'string' ? request : request.url;
}

function sharedBrowser() {
  const stores = new Map();
  const notifications = [];
  let notificationReadFailure;

  const cacheStorage = {
    async open(name) {
      if (!stores.has(name)) stores.set(name, new Map());
      const store = stores.get(name);
      return {
        async addAll() {},
        async match(request) {
          const value = store.get(requestKey(request));
          return value && value.clone();
        },
        async put(request, response) {
          store.set(requestKey(request), response.clone());
        },
      };
    },
    async keys() { return [...stores.keys()]; },
    async delete(name) { return stores.delete(name); },
    async match(request) {
      for (const store of stores.values()) {
        const value = store.get(requestKey(request));
        if (value) return value.clone();
      }
      return undefined;
    },
  };

  const registration = {
    async getNotifications(options = {}) {
      if (notificationReadFailure) {
        const failure = notificationReadFailure;
        notificationReadFailure = undefined;
        throw failure;
      }
      return notifications.filter((notification) => !notification.closed &&
        (!options.tag || notification.tag === options.tag));
    },
    async showNotification(title, options) {
      // The Notifications API replaces a delivered notification sharing a tag.
      if (options.tag) {
        for (const old of notifications) {
          if (!old.closed && old.tag === options.tag) old.close();
        }
      }
      const notification = {
        title,
        ...options,
        closed: false,
        close() { this.closed = true; },
      };
      notifications.push(notification);
    },
  };

  return {
    cacheStorage,
    notifications,
    stores,
    registration,
    failNextNotificationRead(error = new Error('notification read failed')) {
      notificationReadFailure = error;
    },
    activeNotifications() { return notifications.filter((n) => !n.closed); },
  };
}

function worker(browser = sharedBrowser()) {
  const listeners = new Map();
  const clients = {
    claim: async () => {},
    matchAll: async () => [],
    openWindow: async () => {},
  };
  const self = {
    registration: browser.registration,
    clients,
    skipWaiting() {},
    addEventListener(type, listener) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(listener);
    },
  };
  const context = vm.createContext({
    URL,
    Request,
    Response,
    console,
    fetch: async () => { throw new Error('unexpected fetch'); },
    setTimeout,
    clearTimeout,
    caches: browser.cacheStorage,
    clients,
    self,
  });
  vm.runInContext(workerSource, context, { filename: 'pwa/sw.js' });

  async function dispatch(type, additions = {}) {
    const waits = [];
    const event = {
      waitUntil(promise) { waits.push(Promise.resolve(promise)); },
      ...additions,
    };
    for (const listener of listeners.get(type) || []) listener(event);
    await Promise.all(waits);
  }

  return {
    browser,
    clients,
    dispatch,
    push(payload) {
      return dispatch('push', {
        data: {
          json: () => payload,
          text: () => JSON.stringify(payload),
        },
      });
    },
    generation(generation, epoch) {
      return dispatch('message', {
        data: { type: 'server-generation', generation, epoch },
      });
    },
    page(payload) {
      return dispatch('message', {
        data: { type: 'page-notification', ...payload },
      });
    },
  };
}

function modern(generation, epoch, tag, seq, body = 'body') {
  return {
    title: 'bridge', body, contact: 'agent', tag, seq,
    server_generation: generation,
    server_epoch: epoch,
  };
}

test('a newer generation accepts a low sequence and retires the old high sequence', async () => {
  const w = worker();
  await w.push(modern(7, 'epoch-seven', 'attention:agent', 99, 'old daemon'));
  await w.push(modern(8, 'epoch-eight', 'attention:agent', 1, 'new daemon'));

  const active = w.browser.activeNotifications();
  assert.equal(active.length, 1);
  assert.equal(active[0].body, 'new daemon');
  assert.deepEqual({ ...active[0].data }, {
    contact: 'agent', seq: 1, server_generation: 8, server_epoch: 'epoch-eight',
  });
});

test('a delayed older generation is rejected globally even under another tag', async () => {
  const w = worker();
  await w.push(modern(12, 'current', 'attention:a', 1, 'current'));
  await w.push(modern(11, 'old', 'message:b', 500, 'delayed old'));

  assert.deepEqual(w.browser.activeNotifications().map((n) => n.body), ['current']);
});

test('generation and per-tag high-water marks survive worker recreation and shell upgrades', async () => {
  const browser = sharedBrowser();
  const first = worker(browser);
  await first.push(modern(4, 'stable', 'message:a', 20, 'twenty'));

  // Simulate a replaceable prior shell cache and activation of a fresh worker.
  await browser.cacheStorage.open('bridge-v48');
  const recreated = worker(browser);
  await recreated.dispatch('install');
  assert.equal((await browser.cacheStorage.keys()).includes('bridge-v49'), true);
  await recreated.dispatch('activate');
  assert.equal((await browser.cacheStorage.keys()).includes('bridge-v48'), false);
  assert.equal((await browser.cacheStorage.keys()).includes('bridge-notification-generation-v1'), true);

  await recreated.push(modern(4, 'stable', 'message:a', 19, 'nineteen'));
  assert.deepEqual(browser.activeNotifications().map((n) => n.body), ['twenty']);
});

test('a corrupt durable ledger fails closed until an authoritative PWA message repairs it', async () => {
  const browser = sharedBrowser();
  const cache = await browser.cacheStorage.open('bridge-notification-generation-v1');
  await cache.put('/__bridge_notification_generation__', new Response('{not json'));
  const w = worker(browser);

  await w.push(modern(5, 'current', 'message:a', 1, 'must not leak through'));
  assert.equal(browser.activeNotifications().length, 0);

  await browser.registration.showNotification('bridge', {
    body: 'wrong epoch', tag: 'stale-epoch',
    data: { server_generation: 5, server_epoch: 'wrong', seq: 1, contact: 'a' },
  });
  await browser.registration.showNotification('bridge', {
    body: 'impossible future', tag: 'stale-future',
    data: { server_generation: 99, server_epoch: 'future', seq: 1, contact: 'a' },
  });
  await browser.registration.showNotification('bridge', {
    body: 'current namespace', tag: 'current',
    data: { server_generation: 5, server_epoch: 'current', seq: 1, contact: 'a' },
  });
  await w.generation(5, 'current');
  assert.deepEqual(browser.activeNotifications().map((n) => n.body), ['current namespace']);
  await w.push(modern(5, 'current', 'message:a', 1, 'after repair'));
  assert.deepEqual(browser.activeNotifications().map((n) => n.body),
    ['current namespace', 'after repair']);
});

test('authoritative cleanup retries after adoption survives a failed notification read', async () => {
  const browser = sharedBrowser();
  const first = worker(browser);
  await first.push(modern(30, 'before', 'attention:a', 1, 'old delivered'));

  browser.failNextNotificationRead();
  await assert.rejects(first.generation(31, 'after'), /notification read failed/);
  assert.deepEqual(browser.activeNotifications().map((n) => n.body), ['old delivered']);

  // The generation write completed before cleanup failed. A recreated worker
  // sees generation 31 as unchanged, but must still retry notification cleanup.
  const recreated = worker(browser);
  await recreated.generation(31, 'after');
  assert.equal(browser.activeNotifications().length, 0);
});

test('same-generation positive sequences strictly increase while zero stays unsequenced', async () => {
  const w = worker();
  await w.push(modern(3, 'same', 'message:a', 4, 'four'));
  await w.push(modern(3, 'same', 'message:a', 4, 'equal'));
  await w.push(modern(3, 'same', 'message:a', 0, 'unsequenced'));
  await w.push(modern(3, 'same', 'message:a', 3, 'lower'));
  await w.push(modern(3, 'same', 'message:a', 5, 'five'));

  assert.equal(w.browser.activeNotifications()[0].body, 'five');
  assert.deepEqual(w.browser.notifications.filter((n) => !['equal', 'lower'].includes(n.body))
    .map((n) => n.body), ['four', 'unsequenced', 'five']);
});

test('legacy generation zero is accepted only before a modern generation is adopted', async () => {
  const w = worker();
  await w.push({ title: 'bridge', body: 'legacy', tag: 'message:a', seq: 9 });
  await w.push(modern(1, 'first-modern', 'message:a', 1, 'modern'));
  await w.push({ title: 'bridge', body: 'late legacy', tag: 'message:b', seq: 100 });

  assert.deepEqual(w.browser.activeNotifications().map((n) => n.body), ['modern']);
  assert.equal(w.browser.notifications[0].data.server_generation, 0);
  assert.equal(w.browser.notifications[0].data.server_epoch, '');
});

test('same generation with a missing or different epoch is rejected', async () => {
  const w = worker();
  await w.push(modern(6, 'right', 'message:a', 1, 'right'));
  await w.push(modern(6, 'wrong', 'message:b', 2, 'wrong'));
  const missing = modern(6, '', 'message:c', 3, 'missing');
  await w.push(missing);

  assert.deepEqual(w.browser.activeNotifications().map((n) => n.body), ['right']);
});

test('PWA generation messages advance the ledger and close older delivered notifications', async () => {
  const w = worker();
  await w.push(modern(9, 'before', 'attention:a', 4, 'before restart'));
  await w.generation(10, 'after');

  assert.equal(w.browser.activeNotifications().length, 0);
  await w.push(modern(9, 'before', 'message:b', 100, 'delayed'));
  await w.push(modern(10, 'after', 'message:b', 1, 'current'));
  assert.deepEqual(w.browser.activeNotifications().map((n) => n.body), ['current']);
});

test('overlapping push and PWA messages share one ordering chain', async () => {
  const w = worker();
  const first = w.push(modern(20, 'old', 'attention:a', 1, 'old current'));
  const advance = w.generation(21, 'new');
  const delayed = w.push(modern(20, 'old', 'message:b', 999, 'old delayed'));
  await Promise.all([first, advance, delayed]);

  assert.equal(w.browser.activeNotifications().length, 0);
});

test('an in-flight page notification cannot outlive a newer generation', async () => {
  const w = worker();
  await w.generation(40, 'old');
  const originalShow = w.browser.registration.showNotification.bind(w.browser.registration);
  let releaseDisplay;
  let markStarted;
  const displayGate = new Promise((resolve) => { releaseDisplay = resolve; });
  const displayStarted = new Promise((resolve) => { markStarted = resolve; });
  w.browser.registration.showNotification = async (...args) => {
    markStarted();
    await displayGate;
    return originalShow(...args);
  };

  const stale = w.page(modern(40, 'old', 'message:a', 0, 'late page banner'));
  await displayStarted;
  const advance = w.generation(41, 'new');
  releaseDisplay();
  await Promise.all([stale, advance]);

  assert.equal(w.browser.activeNotifications().length, 0,
    'the serialized generation advancement retired the late old banner');
});

test('notification clicks still deep-link an existing PWA window to the contact', async () => {
  const w = worker();
  await w.push(modern(2, 'click', 'message:a', 1, 'tap me'));
  const notification = w.browser.activeNotifications()[0];
  let focused = false;
  const messages = [];
  w.clients.matchAll = async () => [{
    postMessage(message) { messages.push(message); },
    async focus() { focused = true; },
  }];

  await w.dispatch('notificationclick', { notification });
  assert.equal(notification.closed, true);
  assert.equal(focused, true);
  assert.deepEqual(messages.map((message) => ({ ...message })),
    [{
      type: 'open-contact', contact: 'agent',
      server_generation: 2, server_epoch: 'click',
    }]);

  let opened = '';
  w.clients.matchAll = async () => [];
  w.clients.openWindow = async (url) => { opened = url; };
  await w.dispatch('notificationclick', { notification });
  assert.equal(opened,
    '/?contact=agent&server_generation=2&server_epoch=click');
});


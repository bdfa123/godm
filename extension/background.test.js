"use strict";

// Run with: node --test extension/background.test.js
//
// background.js is a service worker, so it is run here as the browser would run
// it, with a stand-in for chrome.* that remembers what the worker asked of it
// and lets the test decide what the native host answers. Only the paths that
// decide whether a download is lost, doubled or handed back are covered.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { install } = require("./fakechrome.test.js");

const settle = () => new Promise((resolve) => setImmediate(resolve));

// later gives a promise whose answer the test supplies when it is ready.
function later() {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
}

const clone = (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)));

// A storage area as the extension API behaves: values are copied in and out,
// and get() takes a key, a list of keys, an object of defaults or null.
function area(data) {
  return {
    async get(keys) {
      if (keys === null || keys === undefined) return clone(data);
      const out = {};
      if (typeof keys === "string") keys = [keys];
      if (Array.isArray(keys)) {
        for (const k of keys) if (k in data) out[k] = clone(data[k]);
        return out;
      }
      for (const k of Object.keys(keys)) out[k] = k in data ? clone(data[k]) : keys[k];
      return out;
    },
    async set(obj) {
      for (const [k, v] of Object.entries(obj)) data[k] = clone(v);
    },
    async remove(keys) {
      for (const k of [].concat(keys)) delete data[k];
    }
  };
}

function event() {
  const fns = [];
  return { fns, addListener: (fn) => fns.push(fn) };
}

// start loads background.js into a fresh worker. options.host answers what the
// native host is asked, and options.cookies what the cookie jar holds.
function start(options) {
  const opts = Object.assign({ host: () => ({ ok: true }), cookies: () => [], settings: {} }, options);
  const data = { session: {}, sync: Object.assign({}, opts.settings), local: {} };
  const seen = { host: [], cancelled: [], downloaded: [], notices: [], windows: [] };
  const events = {
    determining: event(),
    removed: event(),
    message: event()
  };
  let nextWindow = 100;

  const chrome = Object.assign({}, install("en"), {
    runtime: {
      id: "godm-extension-id",
      getURL: (p) => "chrome-extension://godm-extension-id/" + p,
      lastError: undefined,
      sendNativeMessage(host, message, callback) {
        seen.host.push(message);
        Promise.resolve(opts.host(message)).then(callback);
      },
      onInstalled: event(),
      onStartup: event(),
      onMessage: events.message
    },
    storage: { session: area(data.session), sync: area(data.sync), local: area(data.local) },
    downloads: {
      onDeterminingFilename: events.determining,
      cancel: async (id) => seen.cancelled.push(id),
      erase: async () => {},
      download: async (o) => seen.downloaded.push(o.url)
    },
    windows: {
      onRemoved: events.removed,
      getAll: async () => [],
      create: async (o) => {
        const win = { id: nextWindow++, url: o.url };
        seen.windows.push(win);
        return win;
      }
    },
    action: { setTitle() {}, setBadgeText() {}, setBadgeBackgroundColor() {} },
    tabs: { query: async () => [], get: async () => ({}), onUpdated: event(), onRemoved: event(), onActivated: event() },
    webRequest: { onHeadersReceived: event() },
    contextMenus: { removeAll: (cb) => cb && cb(), create() {}, onClicked: event() },
    cookies: { getAll: async (q) => opts.cookies(q) },
    notifications: { create: (n) => seen.notices.push(n) },
    scripting: {}
  });

  const context = vm.createContext({
    chrome,
    navigator: { userAgent: "test-agent" },
    URL,
    console: { debug() {}, log() {}, warn() {}, error() {} },
    // Nothing waits on a timer in these tests, and a real one would keep the
    // test run from ending.
    setTimeout: () => 0,
    importScripts: (...files) => {
      for (const f of files) {
        vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), context, { filename: f });
      }
    }
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "background.js"), "utf8"), context, { filename: "background.js" });

  return {
    data,
    seen,
    events,
    // The browser has started a download and asks the worker what to do.
    download: (item) => context.handleDownload(item),
    // A page of the extension sends the worker a message and waits for the reply.
    message: (msg) => new Promise((resolve) => events.message.fns[0](msg, {}, resolve)),
    // The window with this id was closed.
    close: (id) => Promise.all(events.removed.fns.map((fn) => fn(id))),
    heldKey: () => Object.keys(data.session).find((k) => k.startsWith("confirm:"))
  };
}

const ITEM = {
  id: 5,
  url: "https://example.test/get/setup.exe",
  finalUrl: "https://cdn.test/abc/setup.exe?sig=old",
  filename: "C:\\Users\\me\\Downloads\\setup.exe",
  referrer: "https://example.test/",
  totalBytes: 5 * 1024 * 1024,
  state: "in_progress"
};

const downloads = (w) => w.seen.host.filter((m) => m.type === "download");
const refuse = (m) => (m.type === "download" ? { ok: false, error: "disk full" } : { ok: true });
const form = (key) => ({ type: "godm-confirm-start", key, filename: "setup.exe", outDir: "", connections: "8", dontAsk: false });

test("a refused download goes back to Chrome as the link the person clicked", async () => {
  const w = start({ host: refuse, settings: { confirmDownload: false } });
  await w.download(ITEM);
  assert.equal(downloads(w).length, 1);
  assert.equal(downloads(w)[0].url, ITEM.url, "godm is given the link that was clicked");
  assert.deepEqual(w.seen.downloaded, [ITEM.url], "and so is Chrome, not an address that may have run out");
});

test("after a dialog has waited, a refusal still hands back the original link", async () => {
  const w = start({ host: refuse });
  await w.download(ITEM);
  assert.equal(w.seen.windows.length, 1, "the dialog opened");
  assert.deepEqual(w.seen.cancelled, [ITEM.id], "Chrome's own copy was cancelled");

  const resp = await w.message(form(w.heldKey()));
  assert.equal(resp.handedBack, true);
  assert.deepEqual(w.seen.downloaded, [ITEM.url]);
});

test("the link handed back is not taken over again, even if it now redirects somewhere new", async () => {
  const w = start({ host: refuse, settings: { confirmDownload: false } });
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [ITEM.id]);

  // Chrome starts the download again; the redirect now ends at a fresh
  // signature. (The refusal also switched takeover off for a minute; clear that
  // so the only thing under test is the memory of the handed-back link.)
  w.data.session.hostState = { ok: true, at: 0, error: "" };
  await w.download(Object.assign({}, ITEM, { id: 6, finalUrl: "https://cdn.test/abc/setup.exe?sig=new" }));
  assert.deepEqual(w.seen.cancelled, [ITEM.id], "the second copy was left to Chrome");
  assert.equal(downloads(w).length, 1, "and godm was not asked again");
});

test("closing the dialog after Start but before the job leaves cancels the download", async () => {
  const jar = later();
  const w = start({ cookies: () => jar.promise });
  await w.download(ITEM);
  const key = w.heldKey();
  const win = w.seen.windows[0];

  const reply = w.message(form(key));
  await settle(); // the worker is looking up cookies
  await w.close(win.id); // the window goes: this is Cancel
  jar.resolve([]);

  const resp = await reply;
  assert.equal(resp.ok, false);
  assert.equal(resp.expired, true);
  assert.equal(downloads(w).length, 0, "nothing was sent to godm");
  assert.deepEqual(w.seen.downloaded, [], "and nothing was handed back to Chrome");
  assert.equal(w.heldKey(), undefined);
});

test("once the job is with godm, closing the window does not undo it or hand anything back", async () => {
  const answer = later();
  const w = start({ host: (m) => (m.type === "download" ? answer.promise : { ok: true }) });
  await w.download(ITEM);
  const win = w.seen.windows[0];

  const reply = w.message(form(w.heldKey()));
  await settle();
  assert.equal(downloads(w).length, 1, "the job has gone to the host");
  await w.close(win.id);
  answer.resolve({ ok: true });

  assert.deepEqual(await reply, { ok: true });
  assert.equal(downloads(w).length, 1);
  assert.deepEqual(w.seen.downloaded, []);
});

test("Cancel before Start leaves nothing behind and nothing downloading", async () => {
  const w = start();
  await w.download(ITEM);
  await w.close(w.seen.windows[0].id);
  assert.equal(w.heldKey(), undefined);
  assert.equal(downloads(w).length, 0);
  assert.deepEqual(w.seen.downloaded, []);
});

test("Start twice at once sends the job once", async () => {
  const answer = later();
  const w = start({ host: (m) => (m.type === "download" ? answer.promise : { ok: true }) });
  await w.download(ITEM);
  const key = w.heldKey();
  const first = w.message(form(key));
  await settle();
  const second = await w.message(form(key));
  assert.equal(second.ok, false);
  answer.resolve({ ok: true });
  assert.deepEqual(await first, { ok: true });
  assert.equal(downloads(w).length, 1);
});

test("Don't ask again is kept when Start goes through, and not when it was cancelled on the way", async () => {
  const kept = start();
  await kept.download(ITEM);
  await kept.message(Object.assign(form(kept.heldKey()), { dontAsk: true }));
  assert.equal(kept.data.sync.confirmDownload, false);

  const jar = later();
  const dropped = start({ cookies: () => jar.promise });
  await dropped.download(ITEM);
  const reply = dropped.message(Object.assign(form(dropped.heldKey()), { dontAsk: true }));
  await settle();
  await dropped.close(dropped.seen.windows[0].id);
  jar.resolve([]);
  assert.equal((await reply).expired, true);
  assert.equal(dropped.data.sync.confirmDownload, undefined, "the person cancelled, so the dialog stays on");
});

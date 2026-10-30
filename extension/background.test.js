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
const { install, ownFiles } = require("./fakechrome.test.js");

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

// Who a message comes from, as Chrome describes the sender: one of the
// extension's own pages, or the content script in a tab showing a web page.
const OWN_PAGE = { id: "godm-extension-id", url: "chrome-extension://godm-extension-id/popup.html" };
const WEB_PAGE = {
  id: "godm-extension-id",
  url: "https://tracker.test/t/42",
  tab: { id: 7, url: "https://tracker.test/t/42" },
  frameId: 0
};

// start loads background.js into a fresh worker. options.host answers what the
// native host is asked, and options.cookies what the cookie jar holds.
// options.browser is the language the browser is set to, options.session what
// the worker's session storage holds already, options.local what its storage
// on this computer holds, options.page what the links in
// the page of a tab are found to be, and options.fetch replaces the worker's
// reading of its own files.
function start(options) {
  const opts = Object.assign({ host: () => ({ ok: true }), cookies: () => [], settings: {}, session: {} }, options);
  const data = { session: clone(opts.session), sync: Object.assign({}, opts.settings), local: clone(opts.local || {}) };
  const seen = { host: [], cancelled: [], downloaded: [], notices: [], windows: [], timeouts: [], opened: [], badges: [], menus: [], menuItems: [], titles: [] };
  const events = {
    determining: event(),
    removed: event(),
    message: event(),
    menu: event(),
    installed: event(),
    storage: event(),
    tabUpdated: event()
  };
  let nextWindow = 100;

  const chrome = Object.assign({}, install(opts.browser || "en"), {
    runtime: {
      id: "godm-extension-id",
      getURL: (p) => "chrome-extension://godm-extension-id/" + p,
      lastError: undefined,
      sendNativeMessage(host, message, callback) {
        seen.host.push(message);
        Promise.resolve(opts.host(message)).then(callback);
      },
      onInstalled: events.installed,
      onStartup: event(),
      onMessage: events.message
    },
    storage: {
      session: area(data.session),
      sync: area(data.sync),
      local: area(data.local),
      onChanged: events.storage
    },
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
    action: { setTitle: (o) => seen.titles.push(o.title), setBadgeText: (o) => seen.badges.push(o.text), setBadgeBackgroundColor() {} },
    tabs: {
      query: async () => [],
      get: async () => ({}),
      // A tab sent to an address: how a magnet goes back to the browser.
      update: async (id, o) => seen.opened.push({ tab: id, url: o.url }),
      onUpdated: events.tabUpdated,
      onRemoved: event(),
      onActivated: event()
    },
    webRequest: { onHeadersReceived: event() },
    contextMenus: {
      removeAll: (cb) => cb && cb(),
      create: (o) => (seen.menus.push(o.title), seen.menuItems.push(o)),
      onClicked: events.menu
    },
    cookies: { getAll: async (q) => opts.cookies(q) },
    notifications: { create: (n) => seen.notices.push(n) },
    scripting: { executeScript: async () => (opts.page ? [{ result: clone(opts.page) }] : []) }
  });

  const context = vm.createContext({
    chrome,
    navigator: { userAgent: "test-agent" },
    URL,
    fetch: opts.fetch || ownFiles("godm-extension-id"),
    console: { debug() {}, log() {}, warn() {}, error() {} },
    // Nothing waits on a timer in these tests, and a real one would keep the
    // test run from ending. The delays asked for are kept, so a test can see
    // how long the worker was willing to wait.
    setTimeout: (fn, ms) => {
      seen.timeouts.push(ms);
      return 0;
    },
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
    context,
    // The browser has started a download and asks the worker what to do.
    download: (item) => context.handleDownload(item),
    // The browser asks the worker to confirm a download's name, as it does for
    // every download; returns what the listener returned.
    determine: (item, suggest) => events.determining.fns[0](item, suggest),
    // A page of the extension sends the worker a message and waits for the reply.
    message: (msg, sender) => new Promise((resolve) => events.message.fns[0](msg, sender || OWN_PAGE, resolve)),
    // The content script in a web page sends the worker a message.
    fromPage: (msg, sender) => new Promise((resolve) => events.message.fns[0](msg, sender || WEB_PAGE, resolve)),
    // The person chose an item of the right-click menu.
    menu: (info, tab) => events.menu.fns[0](info, tab),
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

// What the toolbar badge says once the worker has finished painting it.
async function badge(w) {
  for (let i = 0; i < 10; i++) await settle();
  return w.seen.badges[w.seen.badges.length - 1];
}
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

// godm waits for a new link to a download whose link expired, and says how
// large it is when asked. The same file downloaded again is that link.
const FILM = 60 * 1024 * 1024 * 1024;
const awaiting = (sizes) => (m) => (m.type === "ping" ? { ok: true, awaiting: sizes } : { ok: true });

test("a download as large as one godm waits for a new link for is handed over without asking", async () => {
  const w = start({ host: awaiting([35 * 1024 * 1024 * 1024, FILM]) });
  await w.download(Object.assign({}, ITEM, { totalBytes: FILM }));
  assert.equal(w.seen.host[0].type, "ping");
  assert.equal(w.seen.host[0].awaiting, true, "the ping asks for the sizes");
  assert.deepEqual(w.seen.cancelled, [ITEM.id]);
  assert.equal(w.seen.windows.length, 0, "nobody was asked");
  assert.equal(downloads(w).length, 1);
  assert.equal(downloads(w)[0].url, ITEM.url);
});

test("anything else is asked about as before while godm waits for a new link", async () => {
  const cases = [
    ["a byte larger", Object.assign({}, ITEM, { totalBytes: FILM + 1 })],
    ["of a size not known yet", Object.assign({}, ITEM, { totalBytes: 0 })],
    ["a .torrent of that size", Object.assign({}, TORRENT, { totalBytes: FILM })]
  ];
  for (const [what, item] of cases) {
    const w = start({ host: awaiting([FILM]) });
    await w.download(item);
    assert.equal(w.seen.windows.length, 1, what + " was not asked about");
    assert.deepEqual(downloads(w), [], what + " went to godm unasked");
  }
  // And with nothing waiting, or a godm too old to say, as ever.
  for (const host of [awaiting([]), () => ({ ok: true })]) {
    const w = start({ host });
    await w.download(Object.assign({}, ITEM, { totalBytes: FILM }));
    assert.equal(w.seen.windows.length, 1);
  }
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

// ---------- a refusal is an answer, not an unreachable host ----------

// What the native host says when the daemon was reached and said no to the job.
const refusal = (error) => ({ ok: false, error, rejected: true });
const daemonSays = (error) => (m) => (m.type === "ping" ? { ok: true } : refusal(error));

test("a right-click job the daemon refuses does not switch takeover off", async () => {
  const w = start({
    host: (m) => (m.type === "download" && m.url.startsWith("blob:")
      ? refusal("only http and https URLs are supported")
      : { ok: true }),
    settings: { confirmDownload: false }
  });
  await w.menu({ menuItemId: "godm-link", srcUrl: "blob:https://video.test/1234", pageUrl: "https://video.test/" }, { url: "https://video.test/" });
  assert.equal(w.seen.notices.length, 1, "the person is told");
  assert.equal(w.seen.notices[0].message, "only http and https URLs are supported", "in the daemon's words");

  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [ITEM.id], "the next real download still goes to godm");
});

test("a host that cannot be reached on a right-click still switches takeover off", async () => {
  const w = start({
    host: (m) => (m.type === "download" ? { ok: false, error: "Specified native messaging host not found." } : { ok: true }),
    settings: { confirmDownload: false }
  });
  await w.menu({ menuItemId: "godm-link", linkUrl: "https://example.test/a.zip" }, { url: "https://example.test/" });
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [], "a host that is down leaves downloads to Chrome");
});

test("a job the daemon refuses on the way from Chrome is handed back without switching takeover off", async () => {
  const w = start({ host: (m) => (m.type === "download" ? refusal("disk full") : { ok: true }), settings: { confirmDownload: false } });
  await w.download(ITEM);
  assert.deepEqual(w.seen.downloaded, [ITEM.url], "Chrome gets it back");

  await w.download(Object.assign({}, ITEM, { id: 6, url: "https://example.test/get/other.exe", finalUrl: "https://cdn.test/other.exe" }));
  assert.deepEqual(w.seen.cancelled, [ITEM.id, 6], "the next one is still taken over");
});

test("a refusal of a picked batch, a stream or an inspection leaves takeover on", async () => {
  const w = start({ host: daemonSays("nothing there"), settings: { confirmDownload: false } });
  const batch = await w.message({ type: "godm-batch", items: [{ url: "https://example.test/a.zip" }] });
  assert.equal(batch.ok, false);
  const inspected = await w.message({ type: "godm-media-inspect", url: "https://example.test/a.m3u8", kind: "hls" });
  assert.equal(inspected.error, "nothing there");
  const stream = await w.message({ type: "godm-media-download", url: "https://example.test/a.m3u8", kind: "hls" });
  assert.equal(stream.ok, false);

  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [ITEM.id]);
});

// The host waits up to 35 seconds for the daemon to read a stream, and says so
// when it gives up. The worker has to still be there to hear it.
test("an inspection is given longer than the host gives the daemon", async () => {
  const w = start();
  await w.message({ type: "godm-media-inspect", url: "https://example.test/a.m3u8", kind: "hls" });
  const waits = w.seen.timeouts.filter((ms) => ms !== 4000 && ms !== 15000);
  assert.equal(waits.length, 1, "the inspection asked to be waited for: " + w.seen.timeouts);
  assert.ok(waits[0] > 35000, "waits " + waits[0] + " ms");

  const other = start();
  await other.message({ type: "godm-media-download", url: "https://example.test/a.m3u8", kind: "hls" });
  assert.deepEqual(other.seen.timeouts.filter((ms) => ms > 15000), [], "other requests keep the short wait");
});

test("a host that is down on a stream or an inspection still switches takeover off", async () => {
  const w = start({ host: (m) => (m.type === "ping" ? { ok: true } : { ok: false, error: "host gone" }) });
  await w.message({ type: "godm-media-inspect", url: "https://example.test/a.m3u8", kind: "hls" });
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, []);
});

// ---------- a magnet link clicked in a page ----------

const MAGNET = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Some+Film";
const clicked = (w, url, sender) => w.fromPage({ type: "godm-magnet", url: url === undefined ? MAGNET : url }, sender);
const handedBack = (url) => [{ tab: WEB_PAGE.tab.id, url: url || MAGNET }];

test("with asking off, a magnet clicked in a page goes to godm as a torrent at once", async () => {
  const w = start({ cookies: () => [{ name: "uid", value: "1" }], settings: { confirmDownload: false } });
  assert.deepEqual(await clicked(w), { ok: true });
  const sent = downloads(w);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].url, MAGNET);
  assert.equal(sent[0].kind, "bt", "the daemon is told it is a torrent");
  assert.equal(sent[0].referrer, WEB_PAGE.tab.url);
  assert.equal(sent[0].userAgent, "test-agent");
  assert.equal(sent[0].connections, 8);
  assert.ok(!sent[0].cookie, "a magnet is not fetched over HTTP, so it carries no cookies");
  assert.equal(w.seen.windows.length, 0, "no dialog");
  assert.deepEqual(w.seen.opened, [], "and the browser was not given it as well");
  assert.equal(await badge(w), "1", "the badge counts it");
  // Nobody was asked, and a click can be had by a link laid over something
  // else, so the person is told what was added and from where.
  assert.equal(w.seen.notices.length, 1);
  assert.match(w.seen.notices[0].message, /Some Film/);
  assert.match(w.seen.notices[0].message, /tracker\.test/);
});

test("a clicked magnet with no name is named by its hash, and the frame it was in is the site", async () => {
  const w = start({ settings: { confirmDownload: false } });
  const bare = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567";
  const advert = Object.assign({}, WEB_PAGE, { url: "https://ads.test/frame.html", frameId: 3 });
  await clicked(w, bare, advert);
  assert.equal(w.seen.notices.length, 1);
  assert.match(w.seen.notices[0].message, /0123456789abcdef0123456789abcdef01234567/);
  assert.match(w.seen.notices[0].message, /ads\.test/);
});

test("with magnets or takeover switched off, a clicked magnet goes back to the browser", async () => {
  // takeTorrents is the switch magnets shared with .torrent files before each
  // had its own; switched off and never replaced, it still keeps magnets away.
  for (const settings of [{ takeMagnets: false }, { takeTorrents: false }, { enabled: false }]) {
    const w = start({ settings });
    const resp = await clicked(w);
    assert.equal(resp.handedBack, true, JSON.stringify(settings));
    assert.equal(downloads(w).length, 0, "godm was not asked");
    assert.deepEqual(w.seen.opened, handedBack(), "the tab was sent to the link, which opens it as the click would have");
  }
});

test("a magnet the daemon could not read goes back to the browser without asking godm", async () => {
  for (const link of ["magnet:?dn=no-hash", "magnet:?xt=urn:btmh:1220" + "ab".repeat(32), "magnet:?xt=urn:btih:abc"]) {
    const w = start();
    assert.equal((await clicked(w, link)).handedBack, true, link);
    assert.deepEqual(w.seen.host, [], link);
    assert.deepEqual(w.seen.opened, handedBack(link), link);
  }
});

test("a page cannot have anything but a magnet opened or downloaded", async () => {
  for (const link of ["https://evil.test/a.exe", "javascript:alert(1)", "", null, 42]) {
    const w = start();
    const resp = await clicked(w, link);
    assert.equal(resp.ok, false, String(link));
    assert.deepEqual(w.seen.host, [], String(link));
    assert.deepEqual(w.seen.opened, [], String(link));
  }
});

test("a host known to be down hands a clicked magnet straight back", async () => {
  const w = start();
  w.data.session.hostState = { ok: false, at: Date.now(), error: "host gone" };
  assert.equal((await clicked(w)).handedBack, true);
  assert.deepEqual(w.seen.host, [], "the host was not asked again");
  assert.deepEqual(w.seen.opened, handedBack());
});

test("a host that cannot be reached hands the magnet back, says so, and switches takeover off", async () => {
  const w = start({
    host: (m) => (m.type === "download" ? { ok: false, error: "Specified native messaging host not found." } : { ok: true }),
    settings: { confirmDownload: false }
  });
  assert.equal((await clicked(w)).handedBack, true);
  assert.deepEqual(w.seen.opened, handedBack());
  assert.equal(w.seen.notices.length, 1, "the person is told");
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [], "downloads are left to Chrome for a while");
});

test("a magnet the daemon refuses is reported in its words and leaves takeover on", async () => {
  const w = start({ host: (m) => (m.type === "download" ? refusal("disk full") : { ok: true }), settings: { confirmDownload: false } });
  const resp = await clicked(w);
  assert.equal(resp.ok, false);
  assert.equal(w.seen.notices.length, 1);
  assert.equal(w.seen.notices[0].message, "disk full");
  assert.deepEqual(w.seen.opened, [], "godm has seen it, so the browser is not given it too");
  assert.notEqual(await badge(w), "1", "and the badge does not count it");
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [ITEM.id], "the next download still goes to godm");
});

// ---------- a magnet, asked about ----------

const magnetForm = (key, outDir) => ({ type: "godm-confirm-start", key, filename: "", outDir: outDir || "", connections: "", dontAsk: false });

test("with asking on, a clicked magnet waits in the dialog, and Start adds it to the folder chosen", async () => {
  const w = start({ cookies: () => [{ name: "uid", value: "1" }] });
  assert.deepEqual(await clicked(w), { ok: true, asked: true });
  assert.deepEqual(w.seen.host.map((m) => m.type), ["ping"], "godm was only asked whether it is there");
  assert.equal(w.seen.windows.length, 1, "the dialog opened");
  assert.deepEqual(w.seen.opened, [], "the browser was not given it");
  assert.deepEqual(w.seen.notices, [], "and nobody is told about what has not been added");
  const held = w.data.session[w.heldKey()];
  assert.equal(held.kind, "bt");
  assert.equal(held.magnet, true);
  assert.equal(held.filename, "Some Film", "the dialog shows it by its name");
  assert.equal(held.referrer, WEB_PAGE.tab.url);

  assert.deepEqual(await w.message(magnetForm(w.heldKey(), "E:\\Films")), { ok: true });
  const sent = downloads(w);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].url, MAGNET);
  assert.equal(sent[0].kind, "bt");
  assert.equal(sent[0].outDir, "E:\\Films");
  assert.equal(sent[0].filename, "", "the daemon names it from the link, as it does unasked");
  assert.ok(!sent[0].cookie, "a magnet carries no cookies");
  assert.deepEqual(w.seen.notices, [], "the person pressed Start, so there is nothing to tell them");
  assert.equal(w.heldKey(), undefined, "nothing is left waiting");
  assert.equal(w.data.local.lastDir, "E:\\Films", "the folder is offered next time");
});

test("a magnet godm refuses after asking stays in the dialog to be tried again", async () => {
  let refusing = true;
  const w = start({ host: (m) => (m.type === "download" && refusing ? refusal("cannot write to E:\\Films") : { ok: true }) });
  await clicked(w);
  const key = w.heldKey();
  const resp = await w.message(magnetForm(key, "E:\\Films"));
  assert.equal(resp.ok, false);
  assert.equal(resp.error, "cannot write to E:\\Films", "in the daemon's words");
  assert.ok(!resp.handedBack, "the dialog keeps it");
  assert.deepEqual(w.seen.downloaded, [], "Chrome cannot fetch a magnet, so it is not given one");
  assert.deepEqual(w.seen.opened, []);
  assert.equal(w.data.local.lastDir, undefined, "a folder that did not take is not remembered");

  refusing = false;
  assert.deepEqual(await w.message(magnetForm(key, "D:\\Films")), { ok: true }, "Start again, with another folder");
  assert.equal(downloads(w).pop().outDir, "D:\\Films");
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [ITEM.id], "and takeover stayed on");
});

test("a magnet that cannot reach godm after asking goes back to the tab it was clicked in", async () => {
  const w = start({ host: (m) => (m.type === "download" ? { ok: false, error: "host gone" } : { ok: true }) });
  await clicked(w);
  const resp = await w.message(magnetForm(w.heldKey()));
  assert.equal(resp.handedBack, true);
  assert.deepEqual(w.seen.opened, handedBack(), "the tab is sent to the link, which opens it as the click would have");
  assert.deepEqual(w.seen.downloaded, []);
  assert.equal(w.heldKey(), undefined);
  assert.equal(w.seen.notices.length, 1, "the person is told");
});

test("with asking on, a host that cannot be reached hands a clicked magnet straight back", async () => {
  const w = start({ host: () => ({ ok: false, error: "Specified native messaging host not found." }) });
  assert.equal((await clicked(w)).handedBack, true);
  assert.deepEqual(w.seen.windows, [], "no dialog that could only fail");
  assert.deepEqual(w.seen.opened, handedBack());
  assert.equal(w.seen.notices.length, 1);
});

test("a clicked magnet with no window to ask in is added as if asking were off", async () => {
  const w = start();
  w.context.chrome.windows.create = async () => {
    throw new Error("no window");
  };
  assert.deepEqual(await clicked(w), { ok: true });
  assert.equal(downloads(w).length, 1);
  assert.equal(w.seen.notices.length, 1, "and the notice says what was added");
});

test("a magnet from the right-click menu is asked about too, and kept when godm cannot be reached", async () => {
  const w = start({ host: (m) => (m.type === "download" ? { ok: false, error: "host gone" } : { ok: true }) });
  await w.menu({ menuItemId: "godm-link", linkUrl: MAGNET, pageUrl: "https://tracker.test/t/42" }, { id: 7, url: "https://tracker.test/t/42" });
  assert.equal(w.seen.windows.length, 1);
  assert.deepEqual(downloads(w), []);
  const resp = await w.message(magnetForm(w.heldKey()));
  assert.equal(resp.ok, false);
  assert.ok(!resp.handedBack, "a right-click has no click to give back");
  assert.deepEqual(w.seen.opened, []);
  assert.notEqual(w.heldKey(), undefined, "so the dialog can try again");

  // A link of another kind goes straight in, as it always has.
  const other = start();
  await other.menu({ menuItemId: "godm-link", linkUrl: "https://example.test/a.zip" }, { id: 7, url: "https://example.test/" });
  assert.equal(other.seen.windows.length, 0);
  assert.equal(downloads(other).length, 1);
  // And with asking off, so does a magnet.
  const off = start({ settings: { confirmDownload: false } });
  await off.menu({ menuItemId: "godm-link", linkUrl: MAGNET }, { id: 7, url: "https://tracker.test/" });
  assert.equal(off.seen.windows.length, 0);
  assert.equal(downloads(off)[0].kind, "bt");
});

test("the folder chosen in the dialog is remembered when the download goes through, and an emptied one is forgotten", async () => {
  let refusing = false;
  const w = start({ host: (m) => (m.type === "download" && refusing ? refusal("disk full") : { ok: true }) });
  await w.download(ITEM);
  await w.message(Object.assign(form(w.heldKey()), { outDir: "E:\\Stuff" }));
  assert.equal(w.data.local.lastDir, "E:\\Stuff");

  refusing = true;
  await w.download(Object.assign({}, ITEM, { id: 6, url: "https://example.test/get/b.exe", finalUrl: "https://cdn.test/b.exe" }));
  await w.message(Object.assign(form(w.heldKey()), { outDir: "F:\\Elsewhere" }));
  assert.equal(w.data.local.lastDir, "E:\\Stuff", "a download handed back to Chrome does not change it");

  refusing = false;
  await w.download(Object.assign({}, ITEM, { id: 8, url: "https://example.test/get/c.exe", finalUrl: "https://cdn.test/c.exe" }));
  await w.message(form(w.heldKey()));
  assert.equal("lastDir" in w.data.local, false, "an empty folder goes back to godm's own");
});

test("a web page may send nothing but a clicked magnet", async () => {
  const asks = [
    { type: "ping" },
    { type: "tasks" },
    { type: "open" },
    { type: "download", url: "https://example.test/a.zip" },
    { type: "godm-batch", items: [{ url: "https://example.test/a.zip" }] },
    { type: "godm-media-download", url: "https://example.test/a.m3u8", kind: "hls" },
    { type: "godm-confirm-start", key: "confirm:1" },
    { type: "godm-confirm-save-file", key: "confirm:1" },
    { type: "godm-browse" },
    { type: "godm-grab-active" },
    null
  ];
  const w = start();
  for (const msg of asks) {
    const resp = await w.fromPage(msg);
    assert.equal(resp.ok, false, JSON.stringify(msg));
  }
  // A magnet from something that is not a tab, or not this extension, is not a click.
  for (const sender of [{ id: "godm-extension-id", url: "https://tracker.test/" }, Object.assign({}, WEB_PAGE, { id: "other" })]) {
    assert.equal((await clicked(w, MAGNET, sender)).ok, false);
  }
  assert.deepEqual(w.seen.host, [], "nothing reached the native host");
  assert.deepEqual(w.seen.opened, []);
  assert.deepEqual(w.seen.windows, []);
});

test("the extension's own pages still reach the host, an options page in a tab too", async () => {
  const w = start();
  await w.message({ type: "ping" });
  const options = { id: "godm-extension-id", url: "chrome-extension://godm-extension-id/options.html", tab: { id: 3, url: "chrome-extension://godm-extension-id/options.html" } };
  await w.message({ type: "tasks" }, options);
  assert.deepEqual(w.seen.host.map((m) => m.type), ["ping", "tasks"]);
});

test("the worker and the content script start from the same settings", () => {
  const w = start();
  const [all, magnet] = vm.runInContext("[DEFAULTS, MAGNET_DEFAULTS]", w.context);
  assert.equal(all.takeMagnets, true);
  assert.equal(all.takeTorrentFiles, true);
  for (const key of Object.keys(magnet)) assert.strictEqual(magnet[key], all[key], key);
});

// ---------- a .torrent the browser starts to download ----------

const TORRENT = {
  id: 9,
  url: "https://tracker.test/download.php?id=42",
  finalUrl: "https://tracker.test/download.php?id=42",
  filename: "C:\\Users\\me\\Downloads\\Some Film.torrent",
  mime: "application/x-bittorrent",
  referrer: "https://tracker.test/t/42",
  totalBytes: 40 * 1024,
  state: "in_progress"
};

test("a small .torrent is taken over as a torrent, with its cookies and without asking when asking is off", async () => {
  const w = start({ cookies: () => [{ name: "uid", value: "1" }], settings: { confirmDownload: false } });
  await w.download(TORRENT);
  assert.deepEqual(w.seen.cancelled, [TORRENT.id], "Chrome's copy was cancelled, below the size floor and off the type list");
  assert.equal(w.seen.windows.length, 0, "nobody was asked");
  const sent = downloads(w);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].url, TORRENT.url, "the daemon fetches the .torrent itself");
  assert.equal(sent[0].kind, "bt", "and is told it is one, though the link does not end in .torrent");
  assert.equal(sent[0].cookie, "uid=1");
  assert.equal(sent[0].referrer, TORRENT.referrer);
  assert.equal(await badge(w), "1");
  // A page can start a download with no click, so a torrent that went to godm
  // unasked is named, with where it came from.
  assert.equal(w.seen.notices.length, 1);
  assert.match(w.seen.notices[0].message, /Some Film\.torrent/);
  assert.match(w.seen.notices[0].message, /tracker\.test/);
});

test("with asking on, a .torrent a page started waits for Start, and Start sends it as a torrent", async () => {
  const w = start({ cookies: () => [{ name: "uid", value: "1" }] });
  await w.download(TORRENT); // nothing says a person clicked anything
  assert.deepEqual(w.seen.cancelled, [TORRENT.id]);
  assert.equal(w.seen.windows.length, 1, "the dialog opened");
  assert.deepEqual(downloads(w), [], "and godm has been given nothing yet");

  assert.deepEqual(await w.message(form(w.heldKey())), { ok: true });
  const sent = downloads(w);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].url, TORRENT.url);
  assert.equal(sent[0].kind, "bt");
  assert.equal(sent[0].cookie, "uid=1");
  assert.deepEqual(w.seen.notices, [], "the person pressed Start, so there is nothing to tell them");

  const closed = start();
  await closed.download(TORRENT);
  await closed.close(closed.seen.windows[0].id);
  assert.deepEqual(downloads(closed), [], "closing the dialog sends nothing");
});

test("a .torrent godm refuses after asking goes back to Chrome", async () => {
  // The daemon fetches the .torrent before it answers, so a tracker that will
  // not give it to godm comes back as a refusal, while Chrome can still fetch it.
  const w = start({ host: daemonSays("fetching the torrent file: 403 Forbidden") });
  await w.download(TORRENT);
  const resp = await w.message(form(w.heldKey()));
  assert.equal(resp.handedBack, true);
  assert.deepEqual(w.seen.downloaded, [TORRENT.url]);
  w.data.session.hostState = { ok: true, at: 0, error: "" };
  await w.download(Object.assign({}, TORRENT, { id: 10 }));
  assert.deepEqual(w.seen.cancelled, [TORRENT.id], "and Chrome's new copy is left to it");
});

test("a .torrent is known by its name, its link or the type the server sent", async () => {
  const cases = [
    { filename: "C:\\Users\\me\\Downloads\\download", mime: "application/x-bittorrent" },
    { filename: "", mime: "", url: "https://files.test/a/Film.torrent", finalUrl: "https://files.test/a/Film.torrent" },
    { filename: "C:\\Users\\me\\Downloads\\Film.TORRENT", mime: "application/octet-stream" }
  ];
  for (const c of cases) {
    const w = start({ settings: { confirmDownload: false } });
    await w.download(Object.assign({}, TORRENT, c));
    assert.equal(downloads(w).length, 1, JSON.stringify(c));
    assert.equal(downloads(w)[0].kind, "bt", JSON.stringify(c));
  }
});

test("with the .torrent switch off a .torrent is left alone, or taken as a plain file as before", async () => {
  // The switch .torrent files shared with magnets before each had its own
  // counts too while the new one has never been saved.
  for (const settings of [{ takeTorrentFiles: false }, { takeTorrents: false }]) {
    const left = start({ settings });
    await left.download(TORRENT);
    assert.deepEqual(left.seen.cancelled, [], JSON.stringify(settings));
    assert.deepEqual(left.seen.host, [], "not even a ping");
    assert.deepEqual(left.seen.windows, []);
  }

  const listed = start({ settings: { takeTorrentFiles: false, extensions: "torrent", minSize: 0, confirmDownload: false } });
  await listed.download(TORRENT);
  assert.equal(downloads(listed).length, 1, "the type list still takes it");
  assert.equal(downloads(listed)[0].kind, "", "as an ordinary file");
});

test("the blocklist and the hand-back memory still keep a .torrent from godm", async () => {
  const blocked = start({ settings: { blocklist: "tracker.test" } });
  await blocked.download(TORRENT);
  assert.deepEqual(blocked.seen.cancelled, []);

  const w = start({ host: refuse, settings: { confirmDownload: false } });
  await w.download(TORRENT);
  assert.deepEqual(w.seen.downloaded, [TORRENT.url], "a refused .torrent goes back to Chrome");
  assert.deepEqual(w.seen.notices.map((n) => n.title), ["godm could not take over"], "and is not reported as added");
  w.data.session.hostState = { ok: true, at: 0, error: "" };
  await w.download(Object.assign({}, TORRENT, { id: 10 }));
  assert.deepEqual(w.seen.cancelled, [TORRENT.id], "and the copy Chrome starts again is left to it");
});

test("Chrome is not told a .torrent's name until its own copy is cancelled", async () => {
  const ping = later();
  const w = start({ host: (m) => (m.type === "ping" ? ping.promise : { ok: true }) });
  let cancelledWhenAnswered = null;
  const returned = w.determine(TORRENT, () => (cancelledWhenAnswered = w.seen.cancelled.slice()));
  assert.equal(returned, true, "the listener says it will answer later");
  for (let i = 0; i < 10; i++) await settle();
  assert.equal(cancelledWhenAnswered, null, "no answer while the host is asked, so the download cannot finish");

  ping.resolve({ ok: true });
  for (let i = 0; i < 50 && cancelledWhenAnswered === null; i++) await settle();
  assert.deepEqual(cancelledWhenAnswered, [TORRENT.id], "answered once Chrome's copy was cancelled");
});

test("other downloads, and a .torrent left to Chrome, are answered without waiting on godm", async () => {
  const w = start({ host: () => new Promise(() => {}) }); // a host that never answers
  let answered = 0;
  assert.notEqual(w.determine(ITEM, () => answered++), true);
  assert.equal(answered, 1, "an ordinary download is answered at once");

  const off = start({ settings: { takeTorrentFiles: false } });
  let torrentAnswered = 0;
  off.determine(TORRENT, () => torrentAnswered++);
  for (let i = 0; i < 50 && !torrentAnswered; i++) await settle();
  assert.equal(torrentAnswered, 1);
  assert.deepEqual(off.seen.cancelled, []);
});

test("magnets and .torrent files each follow their own switch", async () => {
  const noMagnets = start({ settings: { takeMagnets: false, confirmDownload: false } });
  await noMagnets.download(TORRENT);
  assert.equal(downloads(noMagnets).length, 1, "a .torrent still goes to godm");
  assert.equal((await clicked(noMagnets)).handedBack, true, "a clicked magnet goes back to the browser");
  assert.equal(downloads(noMagnets).length, 1);

  const noFiles = start({ settings: { takeTorrentFiles: false, confirmDownload: false } });
  await noFiles.download(TORRENT);
  assert.deepEqual(noFiles.seen.cancelled, [], "a .torrent is left to Chrome");
  assert.deepEqual(await clicked(noFiles), { ok: true });
  assert.deepEqual(downloads(noFiles).map((m) => m.url), [MAGNET], "a clicked magnet still goes to godm");

  // A switch that has been saved wins over the old one it replaced.
  const mixed = start({ settings: { takeTorrents: false, takeTorrentFiles: true, confirmDownload: false } });
  await mixed.download(TORRENT);
  assert.equal(downloads(mixed).length, 1);
  assert.equal((await clicked(mixed)).handedBack, true);
});

// ---------- a .torrent kept as a file ----------

const saveFile = (key) => ({ type: "godm-confirm-save-file", key });

test("Save the .torrent only gives the file back to Chrome, and the copy Chrome starts is left to it", async () => {
  const w = start();
  await w.download(TORRENT);
  assert.deepEqual(w.seen.cancelled, [TORRENT.id]);
  assert.deepEqual(await w.message(saveFile(w.heldKey())), { ok: true });
  assert.deepEqual(downloads(w), [], "godm was given nothing to download");
  assert.deepEqual(w.seen.downloaded, [TORRENT.url], "Chrome was asked for the link that was clicked");
  assert.equal(w.heldKey(), undefined, "nothing is left waiting");
  assert.deepEqual(w.seen.notices, [], "and there is nothing to tell anyone");

  await w.download(Object.assign({}, TORRENT, { id: 10 }));
  assert.deepEqual(w.seen.cancelled, [TORRENT.id], "Chrome keeps the copy it started again");
  assert.equal(w.seen.windows.length, 1, "without another dialog");
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [TORRENT.id, ITEM.id], "and other downloads still go to godm");
});

test("only a .torrent waiting in its dialog can be kept as a file", async () => {
  // A magnet is no file Chrome could fetch.
  const m = start();
  await clicked(m);
  const magnetKey = m.heldKey();
  assert.equal((await m.message(saveFile(magnetKey))).ok, false);
  assert.deepEqual(m.seen.downloaded, []);
  assert.deepEqual(m.seen.opened, []);
  assert.equal(m.heldKey(), magnetKey, "it is still in its dialog");

  // An ordinary download has Start for that.
  const f = start();
  await f.download(ITEM);
  assert.equal((await f.message(saveFile(f.heldKey()))).ok, false);
  assert.deepEqual(f.seen.downloaded, []);

  // A dialog that has gone has expired, and a key that is no dialog's asks for nothing.
  const gone = start();
  assert.equal((await gone.message(saveFile("confirm:1-gone"))).expired, true);
  assert.equal((await gone.message(saveFile("pick-1"))).ok, false);
  assert.deepEqual(gone.seen.downloaded, []);

  // Once Start has been pressed the .torrent is on its way to godm.
  const answer = later();
  const s = start({ host: (msg) => (msg.type === "download" ? answer.promise : { ok: true }) });
  await s.download(TORRENT);
  const key = s.heldKey();
  const starting = s.message(form(key));
  await settle();
  assert.equal((await s.message(saveFile(key))).ok, false);
  answer.resolve({ ok: true });
  await starting;
  assert.deepEqual(s.seen.downloaded, [], "so Chrome is not given it as well");
});

test("a .torrent Chrome will not download again stays in its dialog", async () => {
  const w = start();
  await w.download(TORRENT);
  w.context.chrome.downloads.download = async () => {
    throw new Error("Invalid URL");
  };
  const resp = await w.message(saveFile(w.heldKey()));
  assert.equal(resp.ok, false);
  assert.match(resp.error, /Invalid URL/);
  assert.notEqual(w.heldKey(), undefined, "so Start, or keeping the file again, can still be chosen");
});

// ---------- a download with no extension at all ----------

// A film a file host names by its hash and serves as plain bytes.
const HASHED = {
  id: 21,
  url: "https://dl.files.test/download/?fid=1&sign=x",
  finalUrl: "https://dl.files.test/download/?fid=1&sign=x",
  filename: "C:\\Users\\me\\Downloads\\A63FC880ABF65C1D80CF37F769B2224320A7FF88",
  mime: "binary/octet-stream",
  referrer: "https://files.test/drive",
  totalBytes: 80 * 1024 * 1024 * 1024,
  state: "in_progress"
};

test("a large download with no extension at all is taken over, a small one or one of unknown size is not", async () => {
  const big = start({ settings: { confirmDownload: false } });
  await big.download(HASHED);
  assert.deepEqual(big.seen.cancelled, [HASHED.id]);
  assert.equal(downloads(big)[0].filename, "A63FC880ABF65C1D80CF37F769B2224320A7FF88", "under the name it came with; godm adds what it holds");

  for (const totalBytes of [99 * 1024 * 1024, 0]) {
    const w = start({ settings: { confirmDownload: false } });
    await w.download(Object.assign({}, HASHED, { totalBytes }));
    assert.deepEqual(w.seen.cancelled, [], "size " + totalBytes);
    assert.deepEqual(w.seen.host, [], "godm is not even asked");
  }
});

test("a dot in a folder's name is no extension", async () => {
  const dotted = { filename: "C:\\Users\\john.doe\\Downloads\\A63FC880ABF65C1D80CF37F769B2224320A7FF88", url: "https://files.test/v1.2/get", finalUrl: "https://files.test/v1.2/get" };
  const w = start({ settings: { confirmDownload: false } });
  await w.download(Object.assign({}, HASHED, dotted));
  assert.deepEqual(w.seen.cancelled, [HASHED.id], "it has none, and is large");

  // A name's own extension is still what counts.
  const film = start({ settings: { confirmDownload: false } });
  await film.download(Object.assign({}, ITEM, dotted, { filename: "C:\\Users\\john.doe\\Downloads\\film.mkv" }));
  assert.deepEqual(film.seen.cancelled, [ITEM.id]);
  const page = start({ settings: { confirmDownload: false } });
  await page.download(Object.assign({}, ITEM, dotted, { filename: "C:\\Users\\john.doe\\Downloads\\notes.txt" }));
  assert.deepEqual(page.seen.cancelled, [], "one off the list stays with the browser");
});

// ---------- the videos a page played, in the link picker ----------

const { mediaEntry } = require("./sniff.js");
const { messagesFor, ownFiles: ownFilesFor } = require("./fakechrome.test.js");

const PLAYER_TAB = { id: 7, url: "https://video.test/watch/1", title: "The Film - video.test" };
const PAGE_LINKS = {
  page: PLAYER_TAB.url,
  title: PLAYER_TAB.title,
  links: [{ url: "https://video.test/files/extras.zip", text: "Extras", kind: "link" }]
};

// found is what the sniffer keeps for a tab: an entry as sniff.js makes it.
const found = (url, kind, type, size, title) => mediaEntry(url, kind, type, size, title || "", Date.now());

const SNIFFED = [
  found("https://cdn.test/hls/master.m3u8", "hls", "application/vnd.apple.mpegurl", 0, PLAYER_TAB.title),
  found("https://cdn.test/dash/manifest.mpd", "dash", "application/dash+xml", 0, PLAYER_TAB.title),
  found("https://files.test/clip.mp4", "file", "video/mp4", 80 * 1024 * 1024, PLAYER_TAB.title)
];

// pickedList is the list the worker left for the picker window, once it has
// opened one.
const pickedList = (w) => {
  const key = Object.keys(w.data.session).find((k) => k.startsWith("pick-"));
  return key && w.data.session[key];
};

test("the picker is given the videos the page played, the files and the streams but not DASH", async () => {
  const w = start({
    session: { "media:7": SNIFFED, "media:8": [found("https://other.test/x.mp4", "file", "video/mp4", 5e6)] },
    page: PAGE_LINKS
  });
  const res = await w.context.pickLinksFromTab(PLAYER_TAB, false);
  assert.equal(res.ok, true);
  assert.equal(res.count, 3, "the page's link and the two videos");
  assert.equal(w.seen.windows.length, 1, "the picker opened");
  const list = pickedList(w);
  assert.deepEqual(list.links, PAGE_LINKS.links, "the links in the page's HTML are as they were");
  assert.deepEqual(
    list.videos.map((v) => [v.kind, v.url]),
    [["hls", "https://cdn.test/hls/master.m3u8"], ["file", "https://files.test/clip.mp4"]],
    "another tab's video is not this page's"
  );
  assert.equal(list.videos[1].size, 80 * 1024 * 1024);
  assert.equal(list.videos[0].title, PLAYER_TAB.title);
});

test("a page whose HTML has no links is still worth picking from when it played a video", async () => {
  const w = start({ session: { "media:7": SNIFFED }, page: { page: PLAYER_TAB.url, title: PLAYER_TAB.title, links: [] } });
  const res = await w.context.pickLinksFromTab(PLAYER_TAB, false);
  assert.equal(res.ok, true);
  assert.equal(w.seen.windows.length, 1);
  assert.equal(pickedList(w).videos.length, 2);
  assert.deepEqual(w.seen.notices, [], "nothing is said about links there are none of");

  const nothing = start({ page: { page: PLAYER_TAB.url, title: "", links: [] } });
  assert.equal((await nothing.context.pickLinksFromTab(PLAYER_TAB, false)).ok, false);
  assert.equal(nothing.seen.windows.length, 0);
  assert.equal(nothing.seen.notices.length, 1, "told there is nothing to pick");
});

test("a page whose scan came back with nothing at all is still picked from, by what the tab says it is", async () => {
  // The scan has nothing to report on a page it could not read the document of
  // (a plain video file in a tab, say), but the sniffer saw what it played.
  const w = start({ session: { "media:7": SNIFFED } }); // no page: executeScript answers []
  const res = await w.context.pickLinksFromTab(PLAYER_TAB, false);
  assert.equal(res.ok, true);
  assert.equal(w.seen.windows.length, 1);
  const list = pickedList(w);
  assert.equal(list.page, PLAYER_TAB.url);
  assert.equal(list.title, PLAYER_TAB.title);
  assert.deepEqual(list.links, []);
  assert.equal(list.videos.length, 2);

  const bare = start({ session: { "media:7": SNIFFED } });
  await bare.context.pickLinksFromTab({ id: 7 }, false);
  assert.equal(pickedList(bare).page, "", "a tab that says nothing of itself is not made up for");
  assert.equal(pickedList(bare).title, "");
});

test("a selection asks for the links inside it and is not given the page's videos", async () => {
  const w = start({ session: { "media:7": SNIFFED }, page: PAGE_LINKS });
  const res = await w.context.pickLinksFromTab(PLAYER_TAB, true);
  assert.equal(res.ok, true);
  assert.deepEqual(pickedList(w).videos, []);

  const none = start({ session: { "media:7": SNIFFED }, page: { page: PLAYER_TAB.url, title: "", links: [] } });
  assert.equal((await none.context.pickLinksFromTab(PLAYER_TAB, true)).ok, false, "no links selected is still nothing to pick");
});

test("the right-click menu brings the videos as the popup's button does", async () => {
  const w = start({ session: { "media:7": SNIFFED }, page: PAGE_LINKS });
  await w.menu({ menuItemId: "godm-page-links" }, PLAYER_TAB);
  assert.equal(pickedList(w).videos.length, 2);
});

// ---------- streams in a picked batch ----------

const batches = (w) => w.seen.host.filter((m) => m.type === "batch");

test("a stream that was picked goes to the host as one, and a file as before", async () => {
  const w = start({ cookies: (q) => [{ name: "who", value: new URL(q.url).hostname }] });
  const resp = await w.message({
    type: "godm-batch",
    referrer: PLAYER_TAB.url,
    connections: 6,
    items: [
      { url: "https://cdn.test/play?id=7", kind: "hls", filename: "The Film", variant: 2 },
      { url: "https://files.test/clip.mp4" },
      { url: "https://video.test/files/extras.zip", filename: "" }
    ]
  });
  assert.equal(resp.ok, true);
  assert.equal(batches(w).length, 1);
  const sent = batches(w)[0];
  assert.equal(sent.referrer, PLAYER_TAB.url);
  assert.equal(sent.connections, 6);
  assert.deepEqual(sent.items, [
    { url: "https://cdn.test/play?id=7", filename: "The Film", cookie: "who=cdn.test", kind: "hls", variant: 2 },
    { url: "https://files.test/clip.mp4", filename: "", cookie: "who=files.test" },
    { url: "https://video.test/files/extras.zip", filename: "", cookie: "who=video.test" }
  ]);
  assert.ok(!("variant" in sent), "a quality belongs to an item, not to the batch");
});

test("a quality goes with a stream that has one, as a number, and with nothing else", async () => {
  const w = start();
  await w.message({
    type: "godm-batch",
    items: [
      { url: "https://v.test/a", kind: "hls", variant: 0 },
      { url: "https://v.test/b", kind: "hls", variant: 3 },
      { url: "https://v.test/c", kind: "hls", variant: -1 },
      { url: "https://v.test/d", kind: "hls", variant: "2" },
      { url: "https://v.test/e", kind: "hls", variant: 1.5 },
      { url: "https://v.test/f", kind: "hls" },
      { url: "https://v.test/g.zip", variant: 2 },
      { url: "https://v.test/h", kind: "yt-dlp", variant: 720 }
    ]
  });
  assert.deepEqual(
    batches(w)[0].items.map((i) => [i.url.split("/").pop(), i.kind, i.variant]),
    [
      ["a", "hls", 0],
      ["b", "hls", 3],
      ["c", "hls", undefined],
      ["d", "hls", undefined],
      ["e", "hls", undefined],
      ["f", "hls", undefined],
      ["g.zip", undefined, undefined],
      ["h", undefined, undefined]
    ],
    "the first quality is a quality; a file or a page for yt-dlp has none to name"
  );
});

test("a picked batch can say a stream, and nothing else about what an item is", async () => {
  const w = start();
  await w.message({
    type: "godm-batch",
    items: [
      { url: "https://t.test/get?id=1", kind: "bt" },
      { url: "https://v.test/watch", kind: "yt-dlp" },
      { url: "https://v.test/x", kind: "HLS" },
      { url: "ftp://v.test/y.m3u8", kind: "hls" },
      { url: "https://v.test/z", kind: "hls" }
    ]
  });
  assert.deepEqual(
    batches(w)[0].items.map((i) => [i.url, i.kind]),
    [
      ["https://t.test/get?id=1", undefined],
      ["https://v.test/watch", undefined],
      ["https://v.test/x", undefined],
      ["https://v.test/z", "hls"]
    ],
    "a torrent or a page for yt-dlp is not what a batch is for; an address that is not http is dropped"
  );
});

// ---------- the language ----------

const messageIn = (locale, key) => messagesFor(locale)[key].message;

// hostDown makes the host fail its ping, which a download answers with a notice.
const hostDown = (m) => (m.type === "ping" ? { ok: false, error: "gone" } : { ok: true });

test("the notices are in the browser's language until another is chosen", async () => {
  for (const [browser, language] of [["en", undefined], ["zh_CN", undefined], ["en", "auto"], ["zh_CN", "auto"]]) {
    const w = start({ host: hostDown, browser, settings: language ? { language } : {} });
    await w.download(ITEM);
    assert.equal(w.seen.notices.length, 1);
    assert.equal(w.seen.notices[0].title, messageIn(browser, "notify_host_down_title"), browser + " " + language);
  }
});

test("a language that is chosen is the language of the notices, whatever the browser says", async () => {
  for (const [browser, language] of [["en", "zh_CN"], ["zh_CN", "en"], ["en", "en"], ["zh_CN", "zh_CN"]]) {
    const w = start({ host: hostDown, browser, settings: { language } });
    await w.download(ITEM);
    assert.equal(w.seen.notices[0].title, messageIn(language, "notify_host_down_title"), browser + " " + language);
  }
});

test("a pick with nothing in it, and an unknown request, are answered in the chosen language", async () => {
  const w = start({ browser: "en", settings: { language: "zh_CN" } });
  const empty = await w.message({ type: "godm-batch", items: [] });
  assert.equal(empty.error, messageIn("zh_CN", "err_nothing_selected"));
  const stranger = await w.fromPage({ type: "ping" });
  assert.equal(stranger.error, messageIn("zh_CN", "err_unknown_request"));
});

// The language is read from a file when the worker wakes, and an event can be
// there before it is. Nothing may answer, or take a download over, in the
// meantime and in the wrong language.
test("nothing is said before the chosen language has been read", async () => {
  const release = later();
  const real = ownFilesFor("godm-extension-id");
  const w = start({
    host: hostDown,
    settings: { language: "zh_CN" },
    fetch: async (url) => {
      await release.promise;
      return real(url);
    }
  });
  let answered = false;
  const asked = w.message({ type: "godm-batch", items: [] }).then((r) => ((answered = true), r));
  const downloading = w.download(ITEM);
  const clickedMenu = w.menu({ menuItemId: "godm-link", linkUrl: "https://example.test/a.zip" }, { url: "https://example.test/" });
  for (let i = 0; i < 20; i++) await settle();
  assert.equal(answered, false, "a page asking is made to wait");
  assert.deepEqual(w.seen.notices, [], "and so is a notice");
  assert.deepEqual(w.seen.host, [], "and nothing reached the host");

  release.resolve();
  const reply = await asked;
  await downloading;
  await clickedMenu;
  assert.equal(reply.error, messageIn("zh_CN", "err_nothing_selected"));
  assert.ok(w.seen.notices.length >= 1);
  for (const n of w.seen.notices) assert.match(n.title + n.message, /[一-鿿]/, JSON.stringify(n));
});

test("the right-click menu is made in the language that is spoken, and again when it is changed", async () => {
  const titles = (locale) => ["menu_link", "menu_link_later", "menu_page_links", "menu_selection_links"].map((k) => messageIn(locale, k));
  const rest = async () => {
    for (let i = 0; i < 5; i++) await settle();
  };
  const w = start({ browser: "en" });
  await w.events.installed.fns[0]();
  await rest();
  assert.deepEqual(w.seen.menus, titles("en"));

  // The options page saves a language; the running worker is told.
  w.data.sync.language = "zh_CN";
  w.seen.menus.length = 0;
  w.events.storage.fns[0]({ language: { oldValue: "auto", newValue: "zh_CN" } }, "sync");
  await rest();
  assert.deepEqual(w.seen.menus, titles("zh_CN"), "the menu follows the choice");

  // And what the worker says from then on.
  const down = start({ host: hostDown, browser: "en" });
  down.data.sync.language = "zh_CN";
  down.events.storage.fns[0]({ language: { newValue: "zh_CN" } }, "sync");
  await rest();
  await down.download(ITEM);
  assert.equal(down.seen.notices[0].title, messageIn("zh_CN", "notify_host_down_title"));

  // Back to following the browser.
  w.seen.menus.length = 0;
  w.events.storage.fns[0]({ language: { oldValue: "zh_CN" } }, "sync");
  await rest();
  assert.deepEqual(w.seen.menus, titles("en"));
});

test("only a change to the language, in the settings that sync, remakes the menu", async () => {
  const w = start();
  for (const [changes, area] of [
    [{ enabled: { newValue: false } }, "sync"],
    [{ language: { newValue: "zh_CN" } }, "local"],
    [{ language: { newValue: "zh_CN" } }, "session"]
  ]) {
    w.events.storage.fns[0](changes, area);
    for (let i = 0; i < 5; i++) await settle();
  }
  assert.deepEqual(w.seen.menus, []);
});

test("the toolbar button's tooltip is written again in the language that is chosen", async () => {
  const rest = async () => {
    for (let i = 0; i < 5; i++) await settle();
  };
  const last = (w) => w.seen.titles[w.seen.titles.length - 1];
  const w = start({ host: hostDown, browser: "en" });
  await w.download(ITEM); // the host is found down, and the tooltip says so
  assert.equal(last(w), messageIn("en", "action_title_down"));

  w.data.sync.language = "zh_CN";
  w.events.storage.fns[0]({ language: { oldValue: "auto", newValue: "zh_CN" } }, "sync");
  await rest();
  assert.equal(last(w), messageIn("zh_CN", "action_title_down"));

  // Back to the browser's language.
  delete w.data.sync.language;
  w.events.storage.fns[0]({ language: { oldValue: "zh_CN" } }, "sync");
  await rest();
  assert.equal(last(w), messageIn("en", "action_title_down"));

  // A host that is up has the same name whatever the language, and a change to
  // some other setting leaves the tooltip alone.
  const up = start({ browser: "en" });
  await up.download(ITEM);
  up.data.sync.language = "zh_CN";
  up.events.storage.fns[0]({ language: { newValue: "zh_CN" } }, "sync");
  await rest();
  assert.equal(last(up), "godm");
  const before = up.seen.titles.length;
  up.events.storage.fns[0]({ enabled: { newValue: false } }, "sync");
  await rest();
  assert.equal(up.seen.titles.length, before);
});

// ---------- what a page played belongs to the page ----------

// Chrome reports a navigation, one made by history.pushState with no reload as
// much as any other, as a status of "loading" with the new address (seen in
// Chrome 155: a pushState gives loading with the url, then complete, then the
// title), and a change of the fragment alone the same.
const navigate = (w, tabId, url) => w.events.tabUpdated.fns[0](tabId, { status: "loading", url }, { id: tabId, url });

test("a page that is navigated to, in the page or not, starts with no videos", async () => {
  const w = start({ session: { "media:7": SNIFFED, "media:8": SNIFFED.slice(0, 1) } });
  navigate(w, 7, "https://video.test/watch/2");
  await settle();
  assert.equal(w.data.session["media:7"], undefined, "what the page before played is not this page's");
  assert.equal(w.data.session["media:8"].length, 1, "another tab's is its own");

  // What follows, the end of the loading and the new title, says nothing about
  // which page it is.
  w.data.session["media:7"] = SNIFFED;
  w.events.tabUpdated.fns[0](7, { status: "complete" }, PLAYER_TAB);
  w.events.tabUpdated.fns[0](7, { title: "Video Two" }, PLAYER_TAB);
  await settle();
  assert.equal(w.data.session["media:7"].length, SNIFFED.length);
});

// ---------- adding to godm's download list ----------

test("the right-click menu has an item for the download list, where the download item is", async () => {
  const w = start();
  await w.events.installed.fns[0]();
  for (let i = 0; i < 5; i++) await settle();
  const link = w.seen.menuItems.find((m) => m.id === "godm-link");
  const list = w.seen.menuItems.find((m) => m.id === "godm-link-later");
  assert.ok(list, "there is an item for it");
  assert.deepEqual(list.contexts, link.contexts, "on the same links, videos, sounds and pictures");
  assert.equal(list.title, messageIn("en", "menu_link_later"));
  assert.notEqual(list.title, link.title);
});

test("the right-click item for the download list sends a link at once, marked as for the list", async () => {
  const w = start({ cookies: () => [{ name: "uid", value: "1" }] });
  await w.menu({ menuItemId: "godm-link-later", linkUrl: "https://example.test/a.zip", pageUrl: "https://example.test/" }, { id: 7, url: "https://example.test/" });
  assert.equal(w.seen.windows.length, 0, "with asking on, no dialog: picking the item was the asking");
  assert.equal(downloads(w).length, 1);
  assert.equal(downloads(w)[0].later, true);
  assert.equal(downloads(w)[0].url, "https://example.test/a.zip");
  assert.equal(downloads(w)[0].kind, "");
  assert.equal(downloads(w)[0].cookie, "uid=1", "a link is sent as the other item sends it");
  assert.equal(downloads(w)[0].referrer, "https://example.test/");
  assert.equal(await badge(w), "1");

  // The other item says nothing of the list.
  const plain = start();
  await plain.menu({ menuItemId: "godm-link", linkUrl: "https://example.test/a.zip" }, { id: 7, url: "https://example.test/" });
  assert.notEqual(downloads(plain)[0].later, true);
});

test("a magnet from the right-click item for the download list is not asked about either", async () => {
  const w = start();
  await w.menu({ menuItemId: "godm-link-later", linkUrl: MAGNET, pageUrl: "https://tracker.test/t/42" }, { id: 7, url: "https://tracker.test/t/42" });
  assert.equal(w.seen.windows.length, 0, "the other item asks about a magnet; this one goes in");
  assert.equal(downloads(w).length, 1);
  assert.equal(downloads(w)[0].url, MAGNET);
  assert.equal(downloads(w)[0].kind, "bt");
  assert.equal(downloads(w)[0].later, true);
  assert.ok(!downloads(w)[0].cookie, "a magnet carries no cookies");
});

test("a right-click job for the download list that fails is reported as the other item's is", async () => {
  const w = start({ host: (m) => (m.type === "download" ? refusal("disk full") : { ok: true }), settings: { confirmDownload: false } });
  await w.menu({ menuItemId: "godm-link-later", linkUrl: "https://example.test/a.zip" }, { url: "https://example.test/" });
  assert.equal(w.seen.notices.length, 1, "the person is told at once");
  assert.equal(w.seen.notices[0].message, "disk full");
  assert.notEqual(await badge(w), "1", "and nothing is counted");
  await w.download(ITEM);
  assert.deepEqual(w.seen.cancelled, [ITEM.id], "a refusal by the daemon leaves takeover on");

  const down = start({
    host: (m) => (m.type === "download" ? { ok: false, error: "Specified native messaging host not found." } : { ok: true }),
    settings: { confirmDownload: false }
  });
  await down.menu({ menuItemId: "godm-link-later", linkUrl: "https://example.test/a.zip" }, { url: "https://example.test/" });
  await down.download(ITEM);
  assert.deepEqual(down.seen.cancelled, [], "a host that cannot be reached switches takeover off");
});

test("Add to list in the dialog sends the download marked as for the list, and Start does not", async () => {
  const listed = start();
  await listed.download(ITEM);
  assert.deepEqual(await listed.message(Object.assign(form(listed.heldKey()), { later: true })), { ok: true });
  assert.equal(downloads(listed).length, 1);
  assert.equal(downloads(listed)[0].later, true);
  assert.equal(downloads(listed)[0].url, ITEM.url, "it is the same job");
  assert.equal(listed.heldKey(), undefined, "and nothing is left waiting");
  assert.equal(await badge(listed), "1");

  const started = start();
  await started.download(ITEM);
  await started.message(form(started.heldKey()));
  assert.notEqual(downloads(started)[0].later, true);

  // The dialog is only a form: nothing but a true counts.
  const odd = start();
  await odd.download(ITEM);
  await odd.message(Object.assign(form(odd.heldKey()), { later: "true" }));
  assert.notEqual(downloads(odd)[0].later, true);
});

test("adding to the list in the dialog keeps the folder and Don't ask again as Start does", async () => {
  const w = start();
  await w.download(ITEM);
  await w.message(Object.assign(form(w.heldKey()), { later: true, dontAsk: true, outDir: "E:\\Stuff" }));
  assert.equal(w.data.local.lastDir, "E:\\Stuff");
  assert.equal(w.data.sync.confirmDownload, false);
  assert.equal(downloads(w)[0].outDir, "E:\\Stuff");
});

test("a download godm refuses for the list goes back to Chrome as one refused for Start does", async () => {
  const w = start({ host: refuse });
  await w.download(ITEM);
  const resp = await w.message(Object.assign(form(w.heldKey()), { later: true }));
  assert.equal(resp.handedBack, true);
  assert.deepEqual(w.seen.downloaded, [ITEM.url]);
  assert.equal(w.data.local.lastDir, undefined, "and the folder is not remembered");
});

test("a magnet is added to the list from the dialog too, and stays there when godm refuses it", async () => {
  let refusing = true;
  const w = start({ host: (m) => (m.type === "download" && refusing ? refusal("cannot write to E:\\Films") : { ok: true }) });
  await clicked(w);
  const key = w.heldKey();
  const refused = await w.message(Object.assign(magnetForm(key, "E:\\Films"), { later: true }));
  assert.equal(refused.ok, false);
  assert.ok(!refused.handedBack, "the dialog keeps it");
  assert.equal(downloads(w)[0].later, true, "it was sent for the list");

  refusing = false;
  assert.deepEqual(await w.message(Object.assign(magnetForm(key, "D:\\Films"), { later: true })), { ok: true });
  const sent = downloads(w).pop();
  assert.equal(sent.later, true);
  assert.equal(sent.kind, "bt");
  assert.equal(sent.outDir, "D:\\Films");
  assert.equal(w.heldKey(), undefined);
});

test("a picked batch goes onto the download list only when the picker says so with true", async () => {
  const w = start();
  const items = [{ url: "https://example.test/a.zip" }, { url: "https://example.test/b.zip" }];
  await w.message({ type: "godm-batch", items, later: true });
  await w.message({ type: "godm-batch", items });
  await w.message({ type: "godm-batch", items, later: "true" });
  assert.deepEqual(batches(w).map((m) => m.later === true), [true, false, false]);
  assert.equal(batches(w)[0].items.length, 2, "the whole batch is for the list");
});

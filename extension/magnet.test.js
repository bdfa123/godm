"use strict";

// Run with: node --test extension/magnet.test.js
//
// The rules are checked against a table of links and clicks. The content
// script is then run as a page would run it, against a stand-in for the window
// and for the parts of chrome.* it touches, to see what a click does.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { magnetLink, magnetName, magnetToTake } = require("./magnet.js");

const HEX = "0123456789abcdef0123456789abcdef01234567";
const MAGNET = "magnet:?xt=urn:btih:" + HEX + "&dn=Some+Film";
const ON = { enabled: true, takeTorrents: true };

const settle = () => new Promise((resolve) => setImmediate(resolve));

test("a magnet link counts when it has one v1 info hash the daemon can read", () => {
  const taken = [
    MAGNET,
    "magnet:?xt=urn:btih:" + HEX.toUpperCase(),
    "MAGNET:?xt=urn:btih:" + HEX,
    "magnet:?xt=urn:btih:ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", // base32
    "magnet:?xt=urn%3Abtih%3A" + HEX, // the colons spelled out
    "magnet:?dn=Film&tr=udp%3A%2F%2Ftracker.test%3A80&xt=urn:btih:" + HEX,
    // A hybrid torrent names its v2 hash as well; the v1 one is what is used.
    "magnet:?xt=urn:btih:" + HEX + "&xt=urn:btmh:1220" + "ab".repeat(32),
    "  " + MAGNET + " "
  ];
  for (const link of taken) assert.equal(magnetLink(link), link.trim(), link);

  const left = [
    "magnet:?dn=no-hash",
    "magnet:?xt=urn:btmh:1220" + "ab".repeat(32), // v2 only
    "magnet:?xt=urn:btih:abc",
    "magnet:?xt=urn:btih:" + HEX + "0",
    "magnet:?xt=urn:btih:abcdefghijklmnopqrstuvwxyz234567", // base32 the daemon cannot decode
    "magnet:?xt=urn:BTIH:" + HEX,
    "magnet:?xt=urn:btih:" + HEX + "&xt=urn:btih:" + HEX.replace("0", "f"),
    "magnet:xt=urn:btih:" + HEX,
    "https://tracker.test/Film.torrent",
    "https://tracker.test/?u=magnet:?xt=urn:btih:" + HEX,
    "javascript:alert(1)",
    "",
    null,
    undefined,
    42
  ];
  for (const link of left) assert.equal(magnetLink(link), "", String(link));
});

// ---------- which clicks count ----------

// An element as a click's path holds it. An HTML link reflects its address as
// a string; an SVG one does not.
function el(localName, attrs, props) {
  const a = attrs || {};
  return Object.assign(
    {
      localName,
      hasAttribute: (n) => n in a,
      getAttribute: (n) => (n in a ? a[n] : null)
    },
    "href" in a ? { href: a.href } : {},
    props
  );
}
const link = (href) => el("a", { href });
const span = () => el("span");

// click is a click the person made with the main button on the first thing in
// the path, unless the overrides say otherwise.
function click(pathList, over) {
  return Object.assign(
    {
      isTrusted: true,
      button: 0,
      ctrlKey: false,
      shiftKey: false,
      altKey: false,
      metaKey: false,
      defaultPrevented: false,
      target: pathList[0],
      composedPath: () => pathList
    },
    over
  );
}

test("a plain click on or inside a magnet link is taken", () => {
  const a = link(MAGNET);
  assert.equal(magnetToTake(click([a, el("body")]), ON), MAGNET);
  assert.equal(magnetToTake(click([span(), el("b"), a, el("body"), {}, {}]), ON), MAGNET, "on text inside it");
  assert.equal(magnetToTake(click([el("area", { href: MAGNET })]), ON), MAGNET, "an image map's area");
  // An a without an href is no link; the one around it is.
  assert.equal(magnetToTake(click([span(), el("a", {}), a]), ON), MAGNET);
});

test("a click the page made, or one with another button or a modifier, is left to the browser", () => {
  const path = [span(), link(MAGNET)];
  const leave = [
    { isTrusted: false },
    { button: 1 },
    { button: 2 },
    { ctrlKey: true },
    { shiftKey: true },
    { altKey: true },
    { metaKey: true },
    { defaultPrevented: true }
  ];
  for (const over of leave) assert.equal(magnetToTake(click(path, over), ON), "", JSON.stringify(over));
  assert.equal(magnetToTake(null, ON), "");
});

test("nothing is taken before the settings are read, or with either switch off", () => {
  const ev = click([link(MAGNET)]);
  assert.equal(magnetToTake(ev, null), "");
  assert.equal(magnetToTake(ev, { enabled: false, takeTorrents: true }), "");
  assert.equal(magnetToTake(ev, { enabled: true, takeTorrents: false }), "");
});

test("only the link nearest the click counts, and it must be a magnet godm can read", () => {
  assert.equal(magnetToTake(click([span(), link("https://tracker.test/t/1"), link(MAGNET)]), ON), "");
  assert.equal(magnetToTake(click([link("magnet:?dn=no-hash")]), ON), "");
  assert.equal(magnetToTake(click([span(), el("div"), el("body")]), ON), "");
});

test("a magnet link inside a shadow root is found through the composed path", () => {
  // Seen from the window the event's target is the shadow host, not the link.
  const host = el("film-card");
  const path = [span(), link(MAGNET), { nodeType: 11 }, host, el("body")];
  assert.equal(magnetToTake(click(path, { target: host }), ON), MAGNET);
  // Which is why the target alone would not do.
  assert.equal(magnetToTake(click(path, { target: host, composedPath: undefined }), ON), "");
});

test("an SVG link is read from its attribute", () => {
  const svgLink = el("a", { href: MAGNET }, { href: { baseVal: MAGNET, animVal: MAGNET } });
  assert.equal(magnetToTake(click([el("text"), svgLink]), ON), MAGNET);
});

test("a magnet link in something being edited is left to the editor", () => {
  // A link inside an editor is editable itself, and the browser only puts the
  // caret there; so does a click on editable text inside a link.
  const editing = { isContentEditable: true };
  const editableLink = el("a", { href: MAGNET }, editing);
  assert.equal(magnetToTake(click([span(), editableLink, el("div", {}, editing), el("body")]), ON), "");
  assert.equal(magnetToTake(click([el("span", {}, editing), link(MAGNET)]), ON), "");
  // What is around the link being editable or not says nothing on its own.
  assert.equal(magnetToTake(click([link(MAGNET), el("div", {}, { isContentEditable: false })]), ON), MAGNET);
});

test("a magnet link is known by the name it carries, or by its hash", () => {
  assert.equal(magnetName(MAGNET), "Some Film");
  assert.equal(magnetName("magnet:?xt=urn:btih:" + HEX), HEX);
  assert.equal(magnetName("magnet:?xt=urn:btih:" + HEX + "&dn=+"), HEX, "a blank name is no name");
  assert.equal(magnetName("not a link"), "");
});

// ---------- the script in a page ----------

const SOURCE = fs.readFileSync(path.join(__dirname, "magnet.js"), "utf8");

// page runs magnet.js as a frame would, with opts.stored in sync storage and
// opts.reply answering what it sends the worker. The storage answer is held
// until the test calls loaded(), as the real one arrives a moment after start.
function page(opts) {
  const o = Object.assign({ stored: {}, reply: () => Promise.resolve({ ok: true }) }, opts);
  const listeners = [];
  const storageListeners = [];
  const observers = [];
  const sent = [];
  const opened = [];
  let answer = null;
  const chrome = {
    runtime: {
      id: "godm-extension-id",
      lastError: undefined,
      sendMessage(msg) {
        sent.push(msg);
        return o.reply(msg);
      }
    },
    storage: {
      sync: { get: (defaults, cb) => (answer = () => cb(Object.assign({}, defaults, o.stored))) },
      onChanged: { addListener: (fn) => storageListeners.push(fn) }
    }
  };
  // As a browser does, the same listener added twice is there once.
  const window = {
    addEventListener(type, fn, capture) {
      if (!listeners.some((l) => l.type === type && l.fn === fn && l.capture === capture)) {
        listeners.push({ type, fn, capture });
      }
    },
    location: { assign: (u) => opened.push(u) }
  };
  const document = {};
  // Only the document's own children are watched; that is where reopening it
  // shows.
  class MutationObserver {
    constructor(fn) {
      this.fn = fn;
    }
    observe(target, options) {
      observers.push({ target, options, fn: this.fn });
    }
  }
  const context = vm.createContext({ chrome, window, document, URL, MutationObserver });
  vm.runInContext(SOURCE, context, { filename: "magnet.js" });
  return {
    chrome,
    listeners,
    sent,
    opened,
    loaded: () => answer(),
    change: (changes, area) => storageListeners.forEach((fn) => fn(changes, area || "sync")),
    // The page's own script calls document.open(): every listener on the
    // window goes, and the document's children are replaced.
    reopen() {
      listeners.length = 0;
      for (const ob of observers) {
        if (ob.target === document && ob.options.childList) ob.fn([{ type: "childList", target: document }]);
      }
    },
    // The parser adds the page's elements to the document as it loads.
    grow() {
      for (const ob of observers) if (ob.target === document) ob.fn([{ type: "childList", target: document }]);
    },
    // The person clicks the magnet link; returns the event as the page's own
    // handlers would then find it.
    click(over) {
      const ev = click([span(), link(MAGNET), el("body")], over);
      ev.prevented = false;
      ev.stopped = false;
      ev.preventDefault = () => (ev.prevented = true);
      ev.stopImmediatePropagation = () => (ev.stopped = true);
      for (const l of listeners) if (l.type === "click") l.fn(ev);
      return ev;
    }
  };
}

test("the page listens for clicks on the window, before the page's own handlers", () => {
  const p = page();
  assert.deepEqual(p.listeners.map((l) => [l.type, l.capture]), [["click", true]]);
  p.grow();
  assert.equal(p.listeners.length, 1, "and once, however the document changes while it loads");
});

test("a page that reopens its document still has its magnet clicks taken", () => {
  const p = page();
  p.loaded();
  p.reopen();
  const ev = p.click();
  assert.equal(ev.prevented, true, "the listener the page wiped is back");
  assert.deepEqual(p.sent, [{ type: "godm-magnet", url: MAGNET }], "and the click is sent once");
});

test("a click is taken once the settings are in, and the page never sees it", () => {
  const p = page();
  const early = p.click();
  assert.equal(early.prevented, false, "before the settings answer the browser keeps the click");
  assert.deepEqual(p.sent, []);

  p.loaded();
  const ev = p.click();
  assert.equal(ev.prevented, true, "the browser does not open the link");
  assert.equal(ev.stopped, true, "and the page's handlers do not run");
  assert.deepEqual(p.sent, [{ type: "godm-magnet", url: MAGNET }]);
});

test("the switch is followed as it changes, including a change made before the settings arrived", () => {
  const p = page();
  p.loaded();
  p.change({ takeTorrents: { oldValue: true, newValue: false } });
  assert.equal(p.click().prevented, false, "switched off");
  p.change({ takeTorrents: { oldValue: false } });
  assert.equal(p.click().prevented, true, "a setting removed is back at its default, which is on");
  p.change({ enabled: { oldValue: true, newValue: false } }, "local");
  assert.equal(p.click().prevented, true, "another storage area is not the settings");

  const early = page();
  early.change({ enabled: { oldValue: true, newValue: false } });
  early.loaded(); // an answer read before the change
  assert.equal(early.click().prevented, false);

  const off = page({ stored: { takeTorrents: false } });
  off.loaded();
  assert.equal(off.click().prevented, false);
});

test("a page the extension has left behind leaves every click to the browser", () => {
  const p = page();
  p.loaded();
  p.chrome.runtime.id = undefined; // the extension was reloaded
  const ev = p.click();
  assert.equal(ev.prevented, false);
  assert.deepEqual(p.sent, []);

  const cut = page({ reply: () => { throw new Error("Extension context invalidated."); } });
  cut.loaded();
  assert.equal(cut.click().prevented, false, "sending failed, so the browser keeps the click");
});

test("if the worker never gets the link the page opens it as the click would have", async () => {
  const lost = page({ reply: () => Promise.reject(new Error("Could not establish connection. Receiving end does not exist.")) });
  lost.loaded();
  assert.equal(lost.click().prevented, true);
  await settle();
  assert.deepEqual(lost.opened, [MAGNET]);

  // The worker answers every magnet and hands it back itself when it has to.
  const answered = page({ reply: () => Promise.resolve({ ok: false, handedBack: true }) });
  answered.loaded();
  answered.click();
  await settle();
  assert.deepEqual(answered.opened, []);
});

test("the manifest runs the script in every frame of every page from the start", () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, "manifest.json"), "utf8"));
  const entry = (manifest.content_scripts || []).find((c) => (c.js || []).includes("magnet.js"));
  assert.ok(entry, "magnet.js is a content script");
  assert.deepEqual(entry.matches, ["<all_urls>"]);
  assert.equal(entry.all_frames, true);
  assert.equal(entry.run_at, "document_start");
  // A page script has no tr: i18n.js is not loaded alongside it.
  assert.deepEqual(entry.js, ["magnet.js"]);
  assert.doesNotMatch(SOURCE, /\btr\(/);
});

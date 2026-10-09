"use strict";

// Run with: node --test extension/confirm.test.js
//
// confirm.js is a page script, so it is run here as the browser would run it,
// against a stand-in for the few parts of the document and of chrome.* that it
// touches. The stand-in does what a browser does where it matters to these
// tests: a disabled button does not click, and listeners run in the order they
// were added.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { install } = require("./fakechrome.test.js");

const IDS = ["form", "url", "name", "size", "dir", "browse", "conns", "dontAsk", "status", "cancel", "start"];

class FakeElement {
  constructor() {
    this.value = "";
    this.textContent = "";
    this.className = "";
    this.title = "";
    this.placeholder = "";
    this.disabled = false;
    this.hidden = false;
    this.checked = false;
    this.listeners = {};
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  // fire is what the browser does when the person acts on the element.
  fire(type, event) {
    if (this.disabled && type === "click") return;
    for (const fn of this.listeners[type] || []) fn(Object.assign({ preventDefault() {} }, event));
  }
  focus() {}
  setSelectionRange() {}
}

const HELD = {
  url: "https://x.test/files/setup.exe",
  backUrl: "https://cdn.test/abc/setup.exe?sig=1",
  referrer: "",
  filename: "setup.exe",
  size: 5 * 1024 * 1024,
  connections: 8
};

const settle = () => new Promise((resolve) => setImmediate(resolve));

// A promise whose answer the test gives later, to hold Start in "sending".
function later() {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
}

// open runs the dialog page. reply answers the messages it sends to the worker.
async function open(reply) {
  const ui = {};
  for (const id of IDS) ui[id] = new FakeElement();
  const doc = new FakeElement();
  doc.getElementById = (id) => ui[id];
  doc.querySelectorAll = () => [];
  doc.documentElement = {};

  const sent = [];
  const closed = [];
  const timers = [];
  const chrome = Object.assign({}, install("en"), {
    storage: { session: { get: async (key) => ({ [key]: HELD }) } },
    runtime: {
      sendMessage: async (message) => {
        sent.push(message);
        return reply(message);
      }
    },
    windows: { getCurrent: async () => ({ id: 7 }), remove: async (id) => closed.push(id) }
  });
  const context = vm.createContext({
    document: doc,
    location: { search: "?k=" + encodeURIComponent("confirm:1-abc") },
    URLSearchParams,
    chrome,
    window: { close: () => closed.push("window.close") },
    setTimeout: (fn, ms) => timers.push({ fn, ms })
  });
  for (const file of ["i18n.js", "handoff.js", "confirm.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), "utf8"), context, { filename: file });
  }
  await settle();
  return { ui, doc, sent, closed, timers };
}

const config = { ok: true, config: { out_dir: "C:\\Users\\me\\Downloads", sort_by_type: false, can_browse_folders: true } };
const starts = (d) => d.sent.filter((m) => m.type === "godm-confirm-start");

test("a folder typed or pasted with a mistake in it is flagged at once, not a keystroke later", async () => {
  const d = await open((m) => (m.type === "godm-config" ? config : { ok: true }));
  assert.equal(d.ui.start.disabled, false);

  d.ui.dir.value = "relative\\folder";
  d.ui.dir.fire("input");
  assert.equal(d.ui.start.disabled, true);
  assert.equal(d.ui.status.className, "err");
  assert.match(d.ui.status.textContent, /full folder path/);

  // Fixing it clears the complaint without another key.
  d.ui.dir.value = "C:\\Downloads";
  d.ui.dir.fire("input");
  assert.equal(d.ui.start.disabled, false);
  assert.equal(d.ui.status.textContent, "");

  // A folder that was typed is a folder that was chosen: it goes to godm.
  d.ui.form.fire("submit");
  await settle();
  assert.equal(starts(d)[0].outDir, "C:\\Downloads");
});

test("a folder left alone is not sent, so godm's own choice (and sorting) stands", async () => {
  const d = await open((m) => (m.type === "godm-config" ? config : { ok: true }));
  d.ui.form.fire("submit");
  await settle();
  assert.equal(starts(d)[0].outDir, "");
});

test("a submit the form cannot pass says why instead of doing nothing", async () => {
  const d = await open((m) => (m.type === "godm-config" ? config : { ok: true }));
  // The page can be a step behind the field (autofill, say). Submitting with an
  // empty name must not be silent.
  d.ui.name.value = "";
  d.ui.form.fire("submit");
  await settle();
  assert.equal(starts(d).length, 0);
  assert.equal(d.ui.status.className, "err");
  assert.match(d.ui.status.textContent, /Enter a file name/);
  assert.equal(d.ui.start.disabled, true);
});

test("Cancel is out of reach while Start is being sent, and so is Escape", async () => {
  const answer = later();
  const d = await open((m) => (m.type === "godm-config" ? config : answer.promise));
  d.ui.form.fire("submit");
  await settle();

  assert.equal(starts(d).length, 1);
  assert.equal(d.ui.cancel.disabled, true);
  assert.equal(d.ui.start.disabled, true);
  d.ui.cancel.fire("click");
  d.doc.fire("keydown", { key: "Escape" });
  assert.deepEqual(d.closed, [], "closing now would not stop the download, so it must not be offered");

  answer.resolve({ ok: true });
  await settle();
  assert.deepEqual(d.closed, [7], "once godm has the job the window closes itself");
});

test("Cancel is not blocked before Start, and closes the window", async () => {
  const d = await open((m) => (m.type === "godm-config" ? config : { ok: true }));
  assert.equal(d.ui.cancel.disabled, false);
  d.ui.cancel.fire("click");
  await settle();
  assert.deepEqual(d.closed, [7]);
});

test("when godm cannot take the job the dialog says so and can be closed straight away", async () => {
  const d = await open((m) =>
    m.type === "godm-config" ? config : { ok: false, handedBack: true, error: "disk full" }
  );
  d.ui.form.fire("submit");
  await settle();

  assert.match(d.ui.status.textContent, /disk full.*handed back to Chrome/);
  assert.equal(d.ui.cancel.disabled, false);
  assert.equal(d.ui.cancel.textContent, "Close");
  assert.equal(d.ui.start.disabled, true, "the download is already back with Chrome; Start would do nothing");
  assert.equal(d.ui.name.disabled, true);

  d.ui.cancel.fire("click");
  await settle();
  assert.deepEqual(d.closed, [7]);

  // Left alone, the window goes by itself.
  const wait = d.timers.find((t) => t.ms === 4000);
  assert.ok(wait, "no timer closes the window");
});

test("a request that has expired is closed, not retried", async () => {
  const d = await open((m) => (m.type === "godm-config" ? config : { ok: false, expired: true, error: "gone" }));
  d.ui.form.fire("submit");
  await settle();
  assert.match(d.ui.status.textContent, /expired/);
  assert.equal(d.ui.cancel.textContent, "Close");
  assert.equal(d.ui.cancel.disabled, false);
  assert.equal(d.ui.start.disabled, true);
});

test("a failed send leaves the dialog usable: Start again, or Cancel", async () => {
  const d = await open((m) => (m.type === "godm-config" ? config : { ok: false, error: "try again" }));
  d.ui.form.fire("submit");
  await settle();
  assert.equal(d.ui.status.textContent, "try again");
  assert.equal(d.ui.start.disabled, false);
  assert.equal(d.ui.cancel.disabled, false);
});

"use strict";

// Run with: node --test extension/options.test.js
//
// options.js is a page script, so it is run here as the browser would run it,
// against a stand-in for the few parts of the document and of chrome.* that it
// touches. What is under test is what a press of Save stores, and shows.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { install } = require("./fakechrome.test.js");

const FIELDS = ["enabled", "takeAll", "confirmDownload", "minSize", "connections", "extensions", "blocklist"];
const NUMBERS = ["minSize", "connections"];
const OTHERS = ["save", "status", "extid", "cmd", "probe", "lastping"];

class FakeElement {
  constructor(id) {
    this.id = id;
    this.type = id === "enabled" || id === "takeAll" || id === "confirmDownload" ? "checkbox"
      : NUMBERS.includes(id) ? "number" : "text";
    this.value = "";
    this.checked = false;
    this.textContent = "";
    this.className = "";
    this.listeners = {};
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  fire(type) {
    for (const fn of this.listeners[type] || []) fn({ target: this });
  }
}

// open runs the options page with these values in storage already.
function open(stored) {
  const ui = {};
  for (const id of FIELDS.concat(OTHERS)) ui[id] = new FakeElement(id);
  const doc = { getElementById: (id) => ui[id], querySelectorAll: () => [], documentElement: {} };
  const saved = {};
  const chrome = Object.assign({}, install("en"), {
    storage: {
      sync: {
        get: (defaults, cb) => cb(Object.assign({}, defaults, stored)),
        set: (obj, cb) => {
          Object.assign(saved, obj);
          if (cb) cb();
        }
      },
      local: { get: (defaults, cb) => cb(defaults) }
    },
    runtime: { id: "abcdefghijklmnop", sendMessage: (m, cb) => cb({ ok: true }) }
  });
  const context = vm.createContext({ document: doc, chrome, setTimeout: () => 0 });
  for (const file of ["i18n.js", "options.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), "utf8"), context, { filename: file });
  }
  return { ui, saved };
}

// press types into a number field and presses Save.
function press(field, text) {
  const p = open({});
  p.ui[field].value = text;
  p.ui.save.fire("click");
  return p;
}

test("a minimum size of 0 is saved as 0, and the field keeps saying 0", () => {
  const p = press("minSize", "0");
  assert.strictEqual(p.saved.minSize, 0);
  assert.strictEqual(String(p.ui.minSize.value), "0");
});

test("other minimum sizes are saved as typed", () => {
  for (const [typed, stored] of [["1", 1], ["4096", 4096], ["2097152", 2097152], [" 512 ", 512]]) {
    assert.strictEqual(press("minSize", typed).saved.minSize, stored, "typed " + typed);
  }
});

test("a minimum size that is not a size falls back to the default and the field shows it", () => {
  for (const typed of ["", "abc", "-5", "-1"]) {
    const p = press("minSize", typed);
    assert.strictEqual(p.saved.minSize, 1048576, "typed '" + typed + "'");
    assert.strictEqual(String(p.ui.minSize.value), "1048576", "the field says what was stored");
  }
});

test("the number of connections stays between 1 and 32", () => {
  for (const [typed, stored] of [["16", 16], ["1", 1], ["32", 32], ["0", 1], ["-3", 1], ["999", 32], ["", 8], ["x", 8]]) {
    const p = press("connections", typed);
    assert.strictEqual(p.saved.connections, stored, "typed '" + typed + "'");
    assert.strictEqual(String(p.ui.connections.value), String(stored), "the field says what was stored");
  }
});

test("saving leaves the text and tick-box fields as they were", () => {
  const p = open({ extensions: "zip,iso", blocklist: "a.test", takeAll: true });
  p.ui.blocklist.value = "  b.test  ";
  p.ui.save.fire("click");
  assert.strictEqual(p.saved.extensions, "zip,iso");
  assert.strictEqual(p.saved.blocklist, "b.test");
  assert.strictEqual(p.saved.takeAll, true);
});

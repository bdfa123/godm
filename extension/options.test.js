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
const { install, ownFiles } = require("./fakechrome.test.js");

const FIELDS = ["enabled", "takeAll", "confirmDownload", "takeMagnets", "takeTorrentFiles", "minSize", "connections", "extensions", "blocklist", "language"];
const BOXES = ["enabled", "takeAll", "confirmDownload", "takeMagnets", "takeTorrentFiles"];
const NUMBERS = ["minSize", "connections"];
const OTHERS = ["save", "status", "extid", "cmd", "probe", "lastping"];

class FakeElement {
  constructor(id) {
    this.id = id;
    this.type = BOXES.includes(id) ? "checkbox" : NUMBERS.includes(id) ? "number" : "text";
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

const settle = () => new Promise((resolve) => setImmediate(resolve));

// open runs the options page with these values in storage already. The page
// has one element that is worded from the messages, as the real one has many.
function open(stored) {
  const ui = {};
  for (const id of FIELDS.concat(OTHERS)) ui[id] = new FakeElement(id);
  const worded = { dataset: { i18n: "options_save" }, textContent: "" };
  const doc = {
    getElementById: (id) => ui[id],
    querySelectorAll: (sel) => (sel === "[data-i18n]" ? [worded] : []),
    documentElement: {}
  };
  const saved = {};
  const chrome = Object.assign({}, install("en"), {
    storage: {
      sync: {
        // As the extension API does, only the keys asked for come back.
        get: (defaults, cb) => {
          const got = {};
          for (const k of Object.keys(defaults)) got[k] = k in stored ? stored[k] : defaults[k];
          if (cb) cb(got);
          return Promise.resolve(got);
        },
        set: (obj, cb) => {
          Object.assign(saved, obj);
          if (cb) cb();
        }
      },
      local: { get: (defaults, cb) => cb(defaults) }
    },
    runtime: {
      id: "abcdefghijklmnop",
      getURL: (p) => "chrome-extension://abcdefghijklmnop/" + p,
      sendMessage: (m, cb) => cb({ ok: true })
    }
  });
  const context = vm.createContext({
    document: doc,
    chrome,
    fetch: ownFiles("abcdefghijklmnop"),
    console: { warn() {} },
    setTimeout: () => 0
  });
  for (const file of ["i18n.js", "options.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), "utf8"), context, { filename: file });
  }
  return { ui, saved, doc, worded };
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

test("magnet links and .torrent files each have a box, and are taken over until it is cleared", () => {
  for (const [box, other] of [["takeMagnets", "takeTorrentFiles"], ["takeTorrentFiles", "takeMagnets"]]) {
    const fresh = open({});
    assert.strictEqual(fresh.ui[box].checked, true, box + " is on when nothing has been saved");
    fresh.ui[box].checked = false;
    fresh.ui.save.fire("click");
    assert.strictEqual(fresh.saved[box], false, "Save keeps the cleared box");
    assert.strictEqual(fresh.saved[other], true, "and the other one as it was");

    const off = open({ [box]: false });
    assert.strictEqual(off.ui[box].checked, false, "the page shows what was saved");
    assert.strictEqual(off.ui[other].checked, true);
    off.ui[box].checked = true;
    off.ui.save.fire("click");
    assert.strictEqual(off.saved[box], true);
  }
});

test("the one switch the two boxes replaced shows in each until it is saved", () => {
  const old = open({ takeTorrents: false });
  assert.strictEqual(old.ui.takeMagnets.checked, false);
  assert.strictEqual(old.ui.takeTorrentFiles.checked, false);
  old.ui.save.fire("click");
  assert.strictEqual(old.saved.takeMagnets, false, "and Save writes it into both");
  assert.strictEqual(old.saved.takeTorrentFiles, false);

  const mixed = open({ takeTorrents: false, takeMagnets: true });
  assert.strictEqual(mixed.ui.takeMagnets.checked, true, "a box that was saved shows its own value");
  assert.strictEqual(mixed.ui.takeTorrentFiles.checked, false);
});

test("the options page has a box for every setting it saves", () => {
  const html = fs.readFileSync(path.join(__dirname, "options.html"), "utf8");
  for (const id of FIELDS) assert.match(html, new RegExp(`id="${id}"`), id);
  assert.match(html, /<input type="checkbox" id="takeMagnets">/);
  assert.match(html, /<input type="checkbox" id="takeTorrentFiles">/);
});

test("saving leaves the text and tick-box fields as they were", () => {
  const p = open({ extensions: "zip,iso", blocklist: "a.test", takeAll: true });
  p.ui.blocklist.value = "  b.test  ";
  p.ui.save.fire("click");
  assert.strictEqual(p.saved.extensions, "zip,iso");
  assert.strictEqual(p.saved.blocklist, "b.test");
  assert.strictEqual(p.saved.takeAll, true);
});

test("the extension follows the browser's language until another is chosen", async () => {
  const fresh = open({});
  assert.strictEqual(fresh.ui.language.value, "auto", "the box says so when nothing has been saved");
  fresh.ui.save.fire("click");
  assert.strictEqual(fresh.saved.language, "auto");
  await settle();
  assert.strictEqual(fresh.ui.probe.textContent, "native host and daemon are reachable");
  assert.strictEqual(fresh.doc.documentElement.lang, "en");
});

test("a language that is chosen is the one the page speaks, and Save keeps it", async () => {
  const p = open({ language: "zh_CN" });
  assert.strictEqual(p.ui.language.value, "zh_CN", "the page shows what was saved");
  await settle();
  assert.match(p.ui.probe.textContent, /可连接/, "the page's own words, not the browser's");
  assert.strictEqual(p.worded.textContent, "保存");
  assert.strictEqual(p.doc.documentElement.lang, "zh-CN");
  p.ui.save.fire("click");
  assert.strictEqual(p.saved.language, "zh_CN");
});

test("choosing another language changes this page at once, and the Saved notice is in it", async () => {
  const p = open({});
  await settle();
  assert.strictEqual(p.worded.textContent, "Save");
  p.ui.language.value = "zh_CN";
  p.ui.save.fire("click");
  assert.strictEqual(p.saved.language, "zh_CN");
  await settle();
  assert.strictEqual(p.ui.status.textContent, "已保存");
  assert.strictEqual(p.worded.textContent, "保存");
  assert.match(p.ui.probe.textContent, /可连接/, "what the page worked out in English is worked out again");

  p.ui.language.value = "en";
  p.ui.save.fire("click");
  await settle();
  assert.strictEqual(p.saved.language, "en");
  assert.strictEqual(p.ui.status.textContent, "Saved");
  assert.strictEqual(p.ui.probe.textContent, "native host and daemon are reachable");
});

test("a language this version does not have is shown and saved as automatic", () => {
  for (const stored of ["fr", "", 7, null]) {
    const p = open({ language: stored });
    assert.strictEqual(p.ui.language.value, "auto", "stored " + JSON.stringify(stored));
    p.ui.language.value = "fr";
    p.ui.save.fire("click");
    assert.strictEqual(p.saved.language, "auto", "chosen fr");
  }
});

test("the options page offers automatic and every language there is", () => {
  const html = fs.readFileSync(path.join(__dirname, "options.html"), "utf8");
  const select = /<select id="language">([\s\S]*?)<\/select>/.exec(html);
  assert.ok(select, "no box for the language");
  const values = [...select[1].matchAll(/<option value="([^"]+)"/g)].map((m) => m[1]);
  const { LANGUAGES, LANGUAGE_DEFAULT } = require("./i18n.js");
  assert.deepEqual(values, [LANGUAGE_DEFAULT].concat(LANGUAGES));
  for (const lang of LANGUAGES) {
    assert.ok(fs.existsSync(path.join(__dirname, "_locales", lang, "messages.json")), lang);
  }
});

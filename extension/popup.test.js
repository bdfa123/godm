"use strict";

// Run with: node --test extension/popup.test.js
//
// popup.js is a page script, so it is run here as the browser would run it,
// against a stand-in for the few parts of the document and of chrome.* that it
// touches.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { install, ownFiles } = require("./fakechrome.test.js");

const settle = () => new Promise((resolve) => setImmediate(resolve));

const IDS = ["body", "media", "open", "opts", "grab", "enabled"];

class FakeElement {
  constructor() {
    this.innerHTML = "";
    this.title = "";
    this.checked = false;
    this.dataset = {};
    this.listeners = {};
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  fire(type) {
    for (const fn of this.listeners[type] || []) fn({ target: this });
  }
}

// open runs the popup. reply answers the messages it sends to the worker,
// which passes the ones it does not know on to the native host. language is
// what the options page saved, if anything.
function open(reply, language) {
  const ui = {};
  for (const id of IDS) ui[id] = new FakeElement();
  const doc = new FakeElement();
  doc.getElementById = (id) => ui[id];
  doc.querySelectorAll = () => [];
  doc.documentElement = {};

  const sent = [];
  const tabs = [];
  const closed = [];
  const chrome = Object.assign({}, install("en"), {
    storage: {
      sync: {
        get: (defaults, cb) => {
          const got = Object.assign({}, defaults, language ? { language } : {});
          if (cb) cb(got);
          return Promise.resolve(got);
        },
        set: () => {}
      }
    },
    runtime: {
      id: "abcdefghijklmnop",
      getURL: (p) => "chrome-extension://abcdefghijklmnop/" + p,
      openOptionsPage: () => {},
      sendMessage: (message, cb) => {
        sent.push(message);
        if (cb) cb(reply(message));
      }
    },
    tabs: { create: (props) => tabs.push(props) }
  });
  const context = vm.createContext({
    document: doc,
    chrome,
    window: { close: () => closed.push("window.close") },
    setInterval: () => 0,
    fetch: ownFiles("abcdefghijklmnop"),
    console: { warn() {} },
    URL
  });
  for (const file of ["i18n.js", "popup.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), "utf8"), context, { filename: file });
  }
  return { ui, sent, tabs, closed };
}

const TOKEN_URL = "http://127.0.0.1:16801/?token=SECRET-DAEMON-TOKEN";

// What the worker and the host answer. A host from before the popup stopped
// being given the manager's address still sends it with the task list; ui is
// that address.
function host(over) {
  return (message) => {
    if (over && message.type in over) return over[message.type];
    if (message.type === "tasks") return { ok: true, tasks: [], ui: TOKEN_URL };
    return { ok: true };
  };
}

test("the manager link asks godm to open the manager and opens no tab itself", () => {
  const p = open(host());
  p.ui.open.fire("click");

  assert.deepEqual(p.sent.filter((m) => m.type === "open"), [{ type: "open" }]);
  assert.deepEqual(p.tabs, [], "the popup opened a tab, whose address any other extension can read");
  assert.deepEqual(p.closed, ["window.close"], "the popup stays in front of the window it asked for");
});

test("the popup keeps no address with the token in it, even when it is sent one", () => {
  const p = open(host());
  p.ui.open.fire("click");
  p.ui.open.fire("click");
  assert.deepEqual(p.tabs, []);
  assert.doesNotMatch(fs.readFileSync(path.join(__dirname, "popup.js"), "utf8"), /tabs\.create|resp\.ui\b/);
});

test("when godm cannot open the manager the popup stays and says why", () => {
  const p = open(host({ open: { ok: false, error: "unknown message type: open" } }));
  p.ui.open.fire("click");

  assert.deepEqual(p.closed, []);
  assert.deepEqual(p.tabs, []);
  assert.equal(p.ui.open.title, "unknown message type: open");
});

test("no answer at all is said in the popup's own words", () => {
  const p = open(host({ open: undefined }));
  p.ui.open.fire("click");

  assert.deepEqual(p.closed, []);
  assert.equal(p.ui.open.title, "no response");
});

test("the popup speaks the language that was chosen, and asks nothing before it knows it", async () => {
  const p = open(host(), "zh_CN");
  assert.deepEqual(p.sent, [], "nothing was drawn, or asked for, in the browser's language");
  await settle();
  assert.ok(p.sent.some((m) => m.type === "tasks"), "then the list is asked for");
  assert.equal(p.ui.body.innerHTML.includes("还没有下载任务。"), true, p.ui.body.innerHTML);

  const plain = open(host());
  await settle();
  assert.equal(plain.ui.body.innerHTML.includes("No downloads yet."), true, plain.ui.body.innerHTML);
});

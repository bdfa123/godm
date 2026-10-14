package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The page is one script inside a Go string, and nothing else exercises it.
// These tests run it in node against a small stand-in for the browser: enough
// of a document to load the page, answer its requests, and tell a test how
// often the list was rewritten and what the dialogs were doing. What they
// cannot show is what only a real browser does (a menu staying open while the
// list around it is swapped); that was checked in the browser.

// runPage runs one scenario of the harness below against the page's script,
// in the given language, and fails the test with its report if it does not
// hold.
func runPage(t *testing.T, scenario, lang string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	m := regexp.MustCompile(`(?s)<script>\n(.*?)\n</script>`).FindStringSubmatch(uiHTML)
	if m == nil {
		t.Fatal("no script in the page")
	}
	dir := t.TempDir()
	page := filepath.Join(dir, "page.js")
	script := strings.Replace(m[1], "__LANG__", lang, 1)
	if err := os.WriteFile(page, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(harness, []byte(pageHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, harness, page, scenario)
	cmd.Env = append(os.Environ(), "MAX_PARALLEL="+strconv.Itoa(maxParallel))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s (%s):\n%s", scenario, lang, out)
	}
}

// A press that straddles a poll must still be a click: the list is not
// rewritten under the pointer, and what was held back is shown afterwards.
func TestThePageDoesNotRewriteTheListUnderAPressedButton(t *testing.T) {
	runPage(t, "press", "en")
}

// Text selected in a card is what the user is about to copy.
func TestThePageDoesNotRewriteTheListUnderASelection(t *testing.T) {
	runPage(t, "selection", "en")
}

func TestAddLinksKeepsAndExplainsWhatTheDaemonRejects(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "rejected", lang) })
	}
}

// pageHarness is node code with no backtick in it. A scenario is an async
// function; one that throws fails the run.
const pageHarness = `
"use strict";
const fs = require("fs"), vm = require("vm");
const src = fs.readFileSync(process.argv[2], "utf8");
const which = process.argv[3];

function assert(c, msg) { if (!c) throw new Error(msg); }
function same(a, b, msg) { if (a !== b) throw new Error(msg + ": got " + JSON.stringify(a) + ", want " + JSON.stringify(b)); }

function matches(n, part) {
  part = part.trim();
  let m;
  if ((m = /^#([\w-]+)$/.exec(part))) return n.id === m[1];
  if ((m = /^(\w*)\[([\w-]+)\]$/.exec(part))) return (!m[1] || n.tagName.toLowerCase() === m[1]) && n.hasAttribute(m[2]);
  return false;
}

const SELECTS = ["limit", "speed", "setSeed", "setLang", "setAfter"];
const DIALOGS = ["addDlg", "rmDlg", "addrDlg", "setDlg"];

class El {
  constructor(id, tag) {
    this.id = id || "";
    this.tagName = (tag || "div").toUpperCase();
    this.parent = null;
    this.listeners = {};
    this.attrs = {};
    this.hidden = false;
    this.textContent = "";
    this.title = "";
    this.placeholder = "";
    this.className = "";
    this.checked = false;
    this.disabled = false;
    this.writes = 0;
    this._html = "";
    this._value = "";
    this.style = {};
    this.classList = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
  }
  get innerHTML() { return this._html; }
  set innerHTML(v) { this._html = v; this.writes++; }
  get value() { return this._value; }
  set value(v) {
    // A select holds only a value it has an option for.
    if (this.tagName === "SELECT" && this._html.indexOf('value="' + v + '"') < 0) v = "";
    this._value = String(v);
  }
  get parentElement() { return this.parent || { hidden: false }; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  removeEventListener() {}
  getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; }
  setAttribute(n, v) { this.attrs[n] = v; }
  hasAttribute(n) { return n in this.attrs; }
  closest(sel) {
    for (let n = this; n; n = n.parent) for (const p of sel.split(",")) if (matches(n, p)) return n;
    return null;
  }
  contains(o) { for (let n = o; n; n = n.parent) if (n === this) return true; return false; }
  focus() { doc.activeElement = this; }
  blur() { if (doc.activeElement === this) doc.activeElement = doc.body; }
  insertAdjacentHTML(_, h) { this._html += h; }
  querySelectorAll() { return []; }
  querySelector() { return null; }
  appendChild() {}
  removeChild() {}
}

const els = {};
const doc = {
  body: new El("", "body"),
  documentElement: {},
  listeners: {},
  getElementById(id) {
    if (!els[id]) {
      els[id] = new El(id, SELECTS.indexOf(id) >= 0 ? "select" : "div");
      if (DIALOGS.indexOf(id) >= 0) els[id].hidden = true;
    }
    return els[id];
  },
  createElement(tag) { return new El("", tag); },
  querySelectorAll(sel) {
    if (sel.indexOf(".scrim") < 0) return [];
    return DIALOGS.map(function (id) { return doc.getElementById(id); })
      .filter(function (d) { return sel.indexOf(":not([hidden])") < 0 || !d.hidden; });
  },
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); },
};
doc.activeElement = doc.body;

// What the page takes of the browser.
let clock = 1000, tasks = [], limit = 3;
const timers = [], intervals = [], calls = [];
const routes = {
  "/api/tasks": () => ({ tasks: tasks, limit: limit, after_all: "nothing", speed_limit: 0, language: "" }),
  "/api/pause": () => ({ ok: true }),
};
function fetch(path, opts) {
  calls.push({ path: path, body: opts && typeof opts.body === "string" ? JSON.parse(opts.body) : null });
  const h = routes[path.split("?")[0]];
  if (!h) return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("no route " + path) });
  const out = h(calls[calls.length - 1].body, path);
  return Promise.resolve({ ok: true, json: () => Promise.resolve(out), text: () => Promise.resolve(JSON.stringify(out)) });
}
const sel = {
  rangeCount: 0, isCollapsed: true, anchorNode: null, focusNode: null,
  removeAllRanges() { this.rangeCount = 0; this.isCollapsed = true; this.anchorNode = this.focusNode = null; fire("selectionchange"); },
  toString() { return this.isCollapsed ? "" : "text"; },
};
function select(node) { sel.rangeCount = 1; sel.isCollapsed = false; sel.anchorNode = sel.focusNode = node; fire("selectionchange"); }

const ctx = vm.createContext({
  document: doc, console: console, fetch: fetch,
  navigator: { language: "en-US", languages: ["en-US"] },
  sessionStorage: { setItem() {} }, history: { replaceState() {} }, location: { pathname: "/" },
  performance: { now: () => clock },
  setTimeout(f) { timers.push(f); return timers.length; }, clearTimeout() {},
  setInterval(f) { intervals.push(f); return 1; }, clearInterval() {},
  getSelection: () => sel,
});
vm.runInContext("var window = globalThis;", ctx);

function page(expr) { return vm.runInContext(expr, ctx); }
async function tick() { for (let i = 0; i < 6; i++) await new Promise((r) => setImmediate(r)); }
function runTimers() { const t = timers.splice(0); t.forEach((f) => f()); }
async function poll() { intervals[0](); await tick(); }
function fire(type, extra) {
  const e = Object.assign({ type: type, target: doc, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, extra);
  (doc.listeners[type] || []).slice().forEach((f) => f.call(doc, e));
  return e;
}
function dispatch(target, type, extra) {
  const e = Object.assign({ type: type, target: target, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, extra);
  for (let n = target; n; n = n.parent) ((n.listeners && n.listeners[type]) || []).slice().forEach((f) => f.call(n, e));
  (doc.listeners[type] || []).slice().forEach((f) => f.call(doc, e));
  return e;
}
function child(parent, tag, attrs) {
  const e = new El("", tag);
  e.parent = parent;
  e.attrs = attrs || {};
  return e;
}
function boot() {
  vm.runInContext(src, ctx);
  return tick();
}
function task(id, extra) {
  return Object.assign({ id: id, url: "http://h/" + id + ".zip", filename: id + ".zip", state: "running", size: 1000, received: 100, speed: 10, conns: 4, active: 1, resumable: true, added_at: "2026-01-01T00:00:00Z" }, extra);
}
function toast() { return doc.getElementById("toast"); }

const scenarios = {};

scenarios.press = async function () {
  tasks = [task("a")];
  routes["/api/pause"] = () => ({ ok: true });
  await boot();
  const list = els.list, tabs = els.tabs;
  assert(list.writes > 0 && list.innerHTML.indexOf("100 B") >= 0, "the first poll should have drawn the list");

  // A button is pressed; a poll lands before the pointer is let go.
  const listBefore = list.writes, tabsBefore = tabs.writes;
  const btn = child(list, "button", { "data-act": "pause", "data-id": "a" });
  dispatch(btn, "mousedown");
  tasks = [task("a", { received: 200 })];
  await poll();
  same(list.writes, listBefore, "the list was rewritten under a pressed button");
  same(tabs.writes, tabsBefore, "the tabs were rewritten under a pressed button");

  dispatch(btn, "mouseup");
  dispatch(btn, "click");
  assert(calls.some((c) => c.path.indexOf("/api/pause") === 0), "the press did not become a click");
  runTimers();
  await tick();
  assert(list.writes > listBefore && list.innerHTML.indexOf("200 B") >= 0, "what a poll held back should be shown once the press is over");

  // A press on a tab. Its own click must show the other tab at once, even
  // though the pointer has only just been let go.
  const tab = child(tabs, "button", { "data-tab": "done" });
  dispatch(tab, "mousedown");
  dispatch(tab, "mouseup");
  dispatch(tab, "click");
  assert(list.innerHTML.indexOf("empty") >= 0 && list.innerHTML.indexOf("a.zip") < 0, "a click on a tab should switch the list at once");
  runTimers();
  await tick();
  assert(tabs.innerHTML.indexOf('data-tab="done"') >= 0, "tabs should be drawn after the press");

  // A press elsewhere does not hold anything back.
  const w = list.writes;
  dispatch(child(els.addText, "div"), "mousedown");
  await poll();
  assert(list.writes > w, "a press outside the list held the list back");
  dispatch(doc, "mouseup");
  runTimers();

  // A mouse-up that never arrives must not freeze the list for good.
  page('S.tab = "all"');
  const stuck = child(list, "button", { "data-act": "pause", "data-id": "a" });
  dispatch(stuck, "mousedown");
  clock += 60000;
  const w2 = list.writes;
  await poll();
  assert(list.writes > w2, "a press with no release froze the list");
};

scenarios.selection = async function () {
  tasks = [task("a")];
  await boot();
  const list = els.list;
  const card = child(list, "div");
  const text = child(card, "span");

  select(text);
  const before = list.writes;
  tasks = [task("a", { received: 300 })];
  await poll();
  same(list.writes, before, "the list was rewritten under selected text");

  sel.removeAllRanges();
  runTimers();
  await tick();
  assert(list.writes > before && list.innerHTML.indexOf("300 B") >= 0, "the held-back list should be shown once nothing is selected");

  // Text selected somewhere else is not the list's business.
  select(els.addText);
  const w = list.writes;
  tasks = [task("a", { received: 400 })];
  await poll();
  assert(list.writes > w, "a selection outside the list held the list back");
  sel.removeAllRanges();

  // A selection that is left alone does not freeze the list for good.
  select(text);
  clock += 120000;
  const w2 = list.writes;
  tasks = [task("a", { received: 500 })];
  await poll();
  assert(list.writes > w2, "an old selection froze the list");
};

scenarios.rejected = async function () {
  tasks = [];
  await boot();
  const text = els.addText;
  routes["/api/batch"] = (body) => {
    const bad = body.items.filter((i) => /^magnet:/.test(i.url));
    return { ok: true, ids: body.items.filter((i) => !/^magnet:/.test(i.url)).map((i, n) => "id" + n),
      errors: bad.map((i) => i.url + ": this magnet link has no BitTorrent v1 info hash (urn:btih)") };
  };

  // Some links are taken and one is not.
  dispatch(els.addBtn, "click");
  text.value = "HTTP://h/upper.zip\nmagnet:?xt=urn:btih:zzzz\nhttp://h/ok.zip";
  dispatch(text, "input");
  dispatch(els.addGo, "click");
  await tick();
  const sent = calls.filter((c) => c.path === "/api/batch")[0].body.items.map((i) => i.url);
  assert(sent.indexOf("http://h/upper.zip") >= 0, "a link in capitals should reach the daemon in the form it takes: " + sent);
  same(text.value, "magnet:?xt=urn:btih:zzzz", "only the rejected link should stay in the box");
  same(els.addDlg.hidden, false, "the dialog should stay open over what was rejected");
  assert(/ err/.test(toast().className), "a rejection should be shown as an error, not as news");
  assert(toast().textContent.indexOf("urn:btih") >= 0, "the reason should be shown: " + toast().textContent);
  assert(/^[^\n]*1/.test(els.addCount.textContent), "the count should be of what is left: " + els.addCount.textContent);

  // Everything taken: the box is emptied and the dialog goes away.
  text.value = "http://h/a.zip\nhttp://h/b.zip";
  dispatch(text, "input");
  dispatch(els.addGo, "click");
  await tick();
  same(text.value, "", "a box of accepted links should be emptied");
  same(els.addDlg.hidden, true, "the dialog should close once everything was accepted");
  assert(!/ err/.test(toast().className), "an accepted batch is not an error");

  // Nothing taken: the reason is shown even though no download was added.
  text.value = "magnet:?xt=urn:btih:nope";
  dispatch(text, "input");
  dispatch(els.addGo, "click");
  await tick();
  same(text.value, "magnet:?xt=urn:btih:nope", "a link nobody took should stay");
  assert(toast().textContent.indexOf("urn:btih") >= 0 && /err/.test(toast().className), "the reason for a total rejection should be shown");
};

const run = scenarios[which];
if (!run) { console.log("no scenario " + which); process.exit(2); }
run().then(() => console.log("ok " + which), (e) => { console.log("FAIL " + which + ": " + (e && e.message)); process.exit(1); });
`

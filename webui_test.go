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
	cmd.Env = append(os.Environ(), "MAX_PARALLEL="+strconv.Itoa(maxParallel), "PAGE_LANG="+lang)
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

// A yt-dlp download is named by its address until yt-dlp has read the page, is
// offered neither a speed limit nor connections from the start, and says when
// it is putting its streams together.
func TestTheCardOfAYTDLPDownload(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "ytdlp", lang) })
	}
}

func TestSpeedsUnderOneKilobyteAreWholeNumbers(t *testing.T) {
	runPage(t, "slow", "en")
}

func TestDownloadsAtOnceShowsEveryLimitTheDaemonAllows(t *testing.T) {
	runPage(t, "limit", "en")
}

func TestPastingALinkWithADialogOpenLeavesTheDialogAlone(t *testing.T) {
	runPage(t, "paste", "en")
}

// The card of a download in progress draws the file to scale from its
// segments: the runs on disk, where the connections work, and where a player
// reads, with how much it has ahead of it.
func TestTheCardMapsWhatIsDownloaded(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "map", lang) })
	}
}

// A server that takes fewer connections than were chosen must not make the
// choice look undone: the menu keeps it, and the card and the answer to the
// change say what the server allows.
func TestTheConnectionsChosenStayChosenUnderAServersCap(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "cap", lang) })
	}
}

// The yt-dlp part of the Settings dialog says what the daemon reports, asks for
// a look and an update, follows an update while it runs, and never shows a
// look that failed as being up to date.
func TestTheSettingsDialogShowsAndUpdatesYTDLP(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "ytsettings", lang) })
	}
}

// The Settings dialog chooses the folder new downloads go to: browsing for one,
// keeping it, and going back to the default.
func TestTheSettingsDialogChoosesTheDownloadFolder(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "downloaddir", lang) })
	}
}

// What a failed download does next: the Settings dialog shows the choices that
// go with each and saves only what changed, and a failed card and the line at
// the top say when it is tried again.
func TestThePageSaysWhatAFailedDownloadDoesNext(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "onfailure", lang) })
	}
}

// The manager page's download list: its tab, its bar and Add to list.
func TestThePageShowsTheDownloadList(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { runPage(t, "downloadlist", lang) })
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

const SELECTS = ["limit", "listLimit", "speed", "setSeed", "setLang", "setAfter", "setFail", "setEvery", "setTimes"];
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
  if (out && out.__http) return Promise.resolve({ ok: false, status: out.__http, text: () => Promise.resolve("HTTP " + out.__http) });
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

  // A press on a button outside the list leaves the selection in place, but
  // what the button does has to show: the list is no longer held for it.
  select(text);
  const held = list.writes;
  tasks = [task("a", { received: 450 })];
  await poll();
  same(list.writes, held, "the list was rewritten under selected text");
  dispatch(child(doc.getElementById("pauseAll"), "button"), "mousedown");
  runTimers();
  await tick();
  assert(list.writes > held && list.innerHTML.indexOf("450 B") >= 0, "a press outside the list should end the hold the selection puts on it");
  tasks = [task("a", { received: 460 })];
  await poll();
  assert(list.innerHTML.indexOf("460 B") >= 0, "the selection held the list again after a press elsewhere");
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

  // A long list of refused links does not turn the toast into a wall, and
  // every one of them is still in the box.
  const many = [];
  for (let i = 0; i < 40; i++) many.push("magnet:?xt=urn:btih:bad" + i);
  text.value = many.join("\n");
  dispatch(text, "input");
  dispatch(els.addGo, "click");
  await tick();
  assert(toast().textContent.length < 600, "the toast for 40 rejections is " + toast().textContent.length + " characters long");
  assert(toast().textContent.indexOf("40") >= 0, "the toast should say how many: " + toast().textContent);
  same(text.value.split("\n").length, 40, "every refused link should stay in the box");
};

scenarios.slow = async function () {
  await boot();
  const want = { "0": "0 B", "666.4550936210727": "666 B", "813.4765625": "813 B", "3.814697265625e-6": "0 B", "1023": "1023 B", "1023.6": "1.0 KB", "1536": "1.5 KB", "1048576": "1.0 MB" };
  Object.keys(want).forEach((k) => same(page("human(" + k + ")"), want[k], "human(" + k + ")"));

  // The average of a finished download is a float.
  const done = task("d", { state: "done", size: 2100, received: 2100, started_at: "2026-01-01T00:00:00Z", ended_at: "2026-01-01T00:00:03.151Z", path: "C:\\d.zip" });
  const html = page("card(" + JSON.stringify(done) + ")");
  assert(html.indexOf("666 B/s") >= 0, "the average of a finished download should be whole bytes: " + html.replace(/<[^>]*>/g, " "));
  assert(!/\d\.\d{3,}/.test(html), "a number with a long fraction is on the card");

  // So is the speed of a connection, and one that has all but stopped shows nothing.
  const slow = task("s", { size: 4000, segments: [
    { start: 0, end: 999, done: 10, state: "active" }, { start: 1000, end: 1999, done: 10, state: "active" } ] });
  page("S.open.s = true; S.prev.s = { at: 0, n: 2, done: [0, 0], speed: [959.0267951815679, 3.814697265625e-6] };");
  clock += 1000;
  const conn = page("connTable(" + JSON.stringify(slow) + ")");
  const cells = conn.replace(/<\/td>/g, "\n").replace(/<[^>]*>/g, "").split("\n").filter((c) => /B\/s/.test(c));
  assert(cells.length > 0 && cells.every((c) => !/\d\.\d{3,}/.test(c)), "a connection speed with a long fraction: " + cells);
  assert(cells.every((c) => !/^0 B/.test(c)), "a connection that sends nothing should not read 0 B/s: " + cells);
};

scenarios.limit = async function () {
  for (const n of [1, 3, 8, 9, 12, Number(process.env.MAX_PARALLEL)]) {
    for (const k of Object.keys(els)) delete els[k];
    limit = n;
    intervals.length = 0;
    await boot();
    same(els.limit.value, String(n), "Downloads at once with a limit of " + n);
  }
  // Moving between values the daemon allows.
  limit = 5;
  await poll();
  same(els.limit.value, "5", "the box should follow the daemon");
};

scenarios.paste = async function () {
  await boot();
  const clip = { clipboardData: { getData: () => "http://h/pasted.zip" } };

  // With nothing open, a pasted link opens the Add dialog.
  els.addBtn.focus();
  let e = dispatch(els.addBtn, "paste", clip);
  same(els.addDlg.hidden, false, "a pasted link should open the Add dialog");
  same(els.addText.value, "http://h/pasted.zip", "the link should be in the box");
  same(e.defaultPrevented, true, "the paste should be taken");
  els.addDlg.hidden = true;
  els.addText.value = "";

  // With Settings open it belongs to Settings: nothing opens underneath it.
  els.setDlg.hidden = false;
  els.setSeed.focus();
  e = dispatch(els.setSeed, "paste", clip);
  same(els.addDlg.hidden, true, "the Add dialog opened underneath Settings");
  same(els.addText.value, "", "a link went into a box nobody can see");
  same(e.defaultPrevented, false, "a paste into Settings should be left alone");
  els.setDlg.hidden = true;

  // The same for the other dialogs.
  for (const d of ["rmDlg", "addrDlg"]) {
    els[d].hidden = false;
    dispatch(els.setSeed, "paste", clip);
    same(els.addDlg.hidden, true, "the Add dialog opened over " + d);
    els[d].hidden = true;
  }

  // With the Add dialog itself open, a link pasted outside its box is added to it.
  els.addDlg.hidden = false;
  els.addText.value = "http://h/first.zip";
  els.addBtn.focus();
  dispatch(els.addBtn, "paste", clip);
  same(els.addText.value, "http://h/first.zip\nhttp://h/pasted.zip", "a link pasted into the open Add dialog should join the others");
};

// The words a scenario looks for, in the language the page was loaded in.
const words = (en, zh) => (process.env.PAGE_LANG === "zh" ? zh : en);

scenarios.map = async function () {
  await boot();
  // A download a quarter of the way through a file of 1000 bytes, with two
  // connections at work and the first two segments run together on disk.
  const segs = [
    { start: 0, end: 99, done: 100, state: "done" },
    { start: 100, end: 399, done: 150, state: "active" },
    { start: 400, end: 699, done: 0, state: "waiting" },
    { start: 700, end: 999, done: 50, state: "active" },
  ];
  const t = task("m", { size: 1000, received: 300, segments: segs, play_at: 120 });
  same(JSON.stringify(page("haveRuns(" + JSON.stringify(segs) + ")")), "[[0,250],[700,750]]", "the runs on disk");
  const html = page("card(" + JSON.stringify(t) + ")");
  const map = (/<div class="bytemap"[^>]*>(.*?)<\/div>/.exec(html) || [])[1];
  assert(map !== undefined, "no map on the card: " + html);
  same((map.match(/<i /g) || []).length, 2, "one shape to a run on disk");
  assert(map.indexOf('left:0.000%;width:25.000%') >= 0 && map.indexOf('left:70.000%;width:5.000%') >= 0, "the runs are drawn to scale: " + map);
  same((map.match(/<b /g) || []).length, 2, "one mark to a connection at work");
  assert(map.indexOf('<b style="left:25.000%">') >= 0 && map.indexOf('<b style="left:75.000%">') >= 0, "the connections are marked where they write: " + map);
  assert(map.indexOf('<u style="left:12.000%">') >= 0, "the player is marked where it reads: " + map);
  // The player's mark is the colour of the text, dark in the light theme and
  // light in the dark one, so the tip tells it by its shape.
  const tip = (/<div class="bytemap" title="([^"]*)"/.exec(html) || [])[1] || "";
  assert(tip.indexOf(words("thicker line", "较粗的竖线")) >= 0 && !/dark|white|深色|白色/.test(tip),
    "the tip should tell the player's mark by its shape, not a colour: " + tip);
  const note = (/<div class="mapnote">(.*?)<\/div>/.exec(html) || [])[1] || "";
  const plain = note.replace(/<[^>]*>/g, "");
  assert(plain.indexOf("12.0%") >= 0 && plain.indexOf("130 B") >= 0 &&
    plain.indexOf(words("has read up to", "已读取到")) >= 0 && plain.indexOf(words("downloaded after that", "此后已下载")) >= 0,
    "the note should say how far the player has read and what is downloaded after that: " + plain);
  // The blocks of the connections are with the table of them now.
  assert(html.indexOf('class="segmap"') < 0, "the card still has the blocks of the connections");
  page("S.open.m = true");
  assert(page("card(" + JSON.stringify(t) + ")").indexOf('class="segmap"') >= 0, "the table of connections should have their blocks");

  // A player that has read to where nothing more is on disk. It reads ahead
  // into its own buffer, so that is where it sits whenever the download is
  // slower than the film, with the film playing on: the note must not say
  // that playback waits.
  const edge = page("card(" + JSON.stringify(Object.assign({}, t, { play_at: 500 })) + ")");
  const enote = ((/<div class="mapnote">(.*?)<\/div>/.exec(edge) || [])[1] || "").replace(/<[^>]*>/g, "");
  assert(enote.indexOf("50.0%") >= 0 && enote.indexOf(words("nothing after that is downloaded yet", "再往后还没有下载")) >= 0,
    "a player with nothing on disk after where it has read: " + enote);
  assert(enote.indexOf(words("wait", "等待")) < 0, "the note says playback waits: " + enote);

  // Connections that are retrying or have failed are marked where they
  // stopped, as the blocks of the connections show them.
  const troubled = page("card(" + JSON.stringify(Object.assign({}, t, { segments: segs.concat([
    { start: 1000, end: 1099, done: 10, state: "retrying" }, { start: 1100, end: 1199, done: 0, state: "failed" },
    { start: 1200, end: 1299, done: 0, state: "connecting" },
  ]), size: 1300 })) + ")");
  const tmap = (/<div class="bytemap"[^>]*>(.*?)<\/div>/.exec(troubled) || [])[1] || "";
  assert(/<b class="retrying" style="left:77\.692%">/.test(tmap) && /<b class="failed" style="left:84\.615%">/.test(tmap),
    "the retrying and failed connections should be marked: " + tmap);
  same((tmap.match(/<b /g) || []).length, 4, "a mark to each connection at work, retrying or failed, and none to one connecting");

  // No player: no mark and no note.
  const noPlayer = Object.assign({}, t);
  delete noPlayer.play_at;
  const quiet = page("card(" + JSON.stringify(noPlayer) + ")");
  assert(quiet.indexOf("<u ") < 0 && quiet.indexOf("mapnote") < 0, "a player was shown where none reads");
  // Nothing to map: a finished file, a stream, a torrent, a download that cannot resume.
  for (const other of [
    { state: "done", path: "C:\\m.zip" }, { kind: "hls" }, { kind: "bt" }, { resumable: false },
  ]) {
    const h = page("card(" + JSON.stringify(Object.assign({}, t, other)) + ")");
    assert(h.indexOf("bytemap") < 0, "a map on " + JSON.stringify(other));
  }

  // The map is drawn by the poll that redraws the list.
  tasks = [t];
  await poll();
  assert(els.list.innerHTML.indexOf("bytemap") >= 0, "the list was drawn without the map");
};

scenarios.cap = async function () {
  // Twelve chosen, eight allowed: the engine's count is eight.
  tasks = [task("c", { conns: 8, conns_wanted: 12, server_cap: 8, active: 8, segments: [{ start: 0, end: 999, done: 100, state: "active" }] })];
  await boot();
  const html = els.list.innerHTML;
  assert(/<option value="12" selected>/.test(html), "the menu should show the 12 chosen: " + (/<select data-conns[^]*?<\/select>/.exec(html) || [""])[0]);
  const meta = html.replace(/<[^>]*>/g, "");
  assert(meta.indexOf(words("8 / 12 connections active · the server allows only 8", "8 / 12 个连接活动中 · 服务器只允许 8 个")) >= 0,
    "the card should say what the server allows: " + meta);

  // No cap: as before, and a cap above the choice holds nothing back.
  for (const extra of [{ conns: 4, conns_wanted: 4, server_cap: 0 }, { conns: 4, conns_wanted: 4, server_cap: 8 }]) {
    const h = page("card(" + JSON.stringify(task("d", Object.assign({ active: 3 }, extra))) + ")").replace(/<[^>]*>/g, "");
    assert(h.indexOf(words("3 of 4 connections active", "3 / 4 个连接活动中")) >= 0 && h.indexOf(words("server", "服务器")) < 0,
      "without a cap holding it back: " + h);
  }
  // A daemon that sends only conns still gets its menu.
  const older = page("connSelect(" + JSON.stringify(task("e", { conns: 6 })) + ")");
  assert(/<option value="6" selected>/.test(older), "a view without conns_wanted: " + older);

  // Choosing 12 again: the answer says what the server allows.
  routes["/api/connections"] = () => ({ ok: true, connections: 12, server_cap: 8 });
  const sel = child(els.list, "select", { "data-conns": "c" });
  sel.innerHTML = '<option value="12">12</option>';
  sel.value = "12";
  dispatch(sel, "change");
  await tick();
  const said = toast().textContent;
  assert(said.indexOf("12") >= 0 && said.indexOf(words("allows only 8", "只允许 8 个")) >= 0 && said.indexOf(words("Saved", "已保存")) >= 0,
    "the answer to choosing 12 of a server that takes 8: " + said);
  // A server that takes one at a time is said in words that fit one.
  routes["/api/connections"] = () => ({ ok: true, connections: 12, server_cap: 1 });
  dispatch(sel, "change");
  await tick();
  const one = toast().textContent;
  assert(one.indexOf(words("allows only 1 at a time", "只允许 1 个")) >= 0 && !/\b1 are\b/.test(one),
    "the answer to choosing 12 of a server that takes 1: " + one);
  // And when the server takes them all, only that the choice was saved.
  routes["/api/connections"] = () => ({ ok: true, connections: 6, server_cap: 8 });
  dispatch(sel, "change");
  await tick();
  const plain = toast().textContent;
  assert(plain.indexOf("6") >= 0 && plain.indexOf(words("Saved", "已保存")) >= 0 && plain.indexOf(words("allows", "允许")) < 0,
    "the answer to choosing 6 of a server that takes 8: " + plain);
};

scenarios.ytdlp = async function () {
  await boot();
  // The stand-in for the browser has no URL, and a card falls back to the whole
  // address without one: the last piece of an address is what a browser gives.
  ctx.URL = URL;
  const html = (t) => page("card(" + JSON.stringify(t) + ")");
  const text = (t) => html(t).replace(/<[^>]*>/g, " ");
  const nameOnCard = (t) => (/<span class="name"[^>]*>(.*?)<\/span>/.exec(html(t)) || [])[1];

  // Before yt-dlp has read the page it has no title, and the last piece of the
  // address says nothing about the video.
  const waiting = task("y", { kind: "yt-dlp", url: "https://www.youtube.com/watch?v=abc", filename: "", size: -1, received: 0, speed: 0, conns: 0, resumable: false });
  same(nameOnCard(waiting), "https://www.youtube.com/watch?v=abc", "a yt-dlp download without a title is named by its address");
  // Neither does it take a speed limit or a number of connections.
  assert(html(waiting).indexOf("data-speed") < 0 && html(waiting).indexOf("data-conns") < 0,
    "a yt-dlp download is offered a speed limit or a number of connections");

  // Once the title is known the card goes by it.
  const titled = task("t", { kind: "yt-dlp", url: "https://www.youtube.com/watch?v=abc", filename: "A Film", size: 1000, received: 500, speed: 10, conns: 0, stage: "video", resumable: false });
  same(nameOnCard(titled), "A Film", "the title should replace the address");
  assert(text(titled).indexOf(words("video track", "视频轨道")) >= 0, "the stream should be named: " + text(titled));

  // With every stream in and the merge under way, the card says so.
  const merging = task("m", { kind: "yt-dlp", url: "https://www.youtube.com/watch?v=abc", filename: "A Film", size: 1000, received: 1000, speed: 0, conns: 0, stage: "processing", resumable: false });
  assert(text(merging).indexOf(words("putting it together", "正在合并处理")) >= 0, "the card of a merge should say so: " + text(merging));
  assert(text(merging).indexOf(words("video track", "视频轨道")) < 0 && text(merging).indexOf(words("audio track", "音频轨道")) < 0,
    "the card of a merge should not name a stream: " + text(merging));
};

// The yt-dlp part of the Settings dialog: what it shows for each state the
// daemon can report, what the buttons ask for, and that a look that failed is
// never shown as all being well.
scenarios.ytsettings = async function () {
  tasks = [];
  const status = (extra) => Object.assign({ ok: true, found: true, path: "C:\\y\\yt-dlp.exe", install: "winget", can_update: true,
    version: "2026.06.01", latest: "2026.09.15", outdated: true, busy: false, updating: false }, extra);
  let st = status({}), updateReply = status({ updating: true, step: "yt-dlp" }), down = false;
  routes["/api/settings"] = () => ({ ok: true, keep_awake: true, can_keep_awake: true, after_all: "nothing",
    after_all_options: ["nothing"], sort_by_type: false, folders: {}, out_dir: "C:\\d", language: "auto" });
  routes["/api/ytdlp"] = () => (down ? { __http: 500 } : st);
  routes["/api/ytdlp/check"] = () => st;
  routes["/api/ytdlp/update"] = () => updateReply;
  await boot();
  const said = () => els.ytStatus.textContent;
  const count = (p) => calls.filter((c) => c.path === p || c.path.indexOf(p + "?") === 0).length;
  const open = async () => { els.setDlg.hidden = true; dispatch(els.setBtn, "click"); await tick(); };
  const kind = () => els.ytStatus.className;

  // Opening the dialog shows what the daemon has and asks it to look, from what
  // it found lately.
  await open();
  same(els.setDlg.hidden, false, "the dialog should open");
  same(els.ytHave.textContent, "2026.06.01", "the installed version");
  same(els.ytLatest.textContent, "2026.09.15", "the newest release");
  same(count("/api/ytdlp"), 1, "the dialog asks what is known once");
  same(count("/api/ytdlp/check"), 1, "and has it look");
  assert(calls.filter((c) => c.path.indexOf("/api/ytdlp/check") === 0)[0].path.indexOf("force") < 0, "opening the dialog forces a look");
  assert(said().indexOf(words("A newer yt-dlp is available: 2026.06.01 → 2026.09.15.", "有新版 yt-dlp 可用：2026.06.01 → 2026.09.15。")) >= 0, "a newer release: " + said());
  same(kind(), "warn", "a newer release is a notice, not an error");
  same(els.ytUpdate.disabled, false, "there is something to update");
  same(els.ytCheck.disabled, false, "the check button");

  // The check button looks again whatever is remembered.
  dispatch(els.ytCheck, "click");
  await tick();
  assert(calls.some((c) => c.path === "/api/ytdlp/check?force=1"), "the check button should force a look");

  // Up to date: nothing to update.
  st = status({ version: "2026.09.15", outdated: false });
  dispatch(els.ytCheck, "click");
  await tick();
  assert(said().indexOf(words("yt-dlp is up to date.", "yt-dlp 已是最新版本。")) >= 0, "up to date: " + said());
  same(kind(), "ok", "up to date is good news");
  same(els.ytUpdate.disabled, true, "nothing to update when it is the newest");

  // A look that failed is said so, and never taken for being up to date, even
  // when an older answer says the installed version is the newest.
  for (const bad of [
    status({ latest: "", outdated: null, latest_error: "could not reach GitHub" }),
    status({ version: "2026.09.15", latest: "2026.09.15", outdated: false, latest_error: "GitHub answered 502 Bad Gateway" }),
  ]) {
    st = bad;
    dispatch(els.ytCheck, "click");
    await tick();
    assert(said().indexOf(bad.latest_error) >= 0, "the reason the look failed: " + said());
    assert(said().indexOf("up to date") < 0 && said().indexOf("已是最新") < 0, "a failed look was called up to date: " + said());
    same(kind(), "err", "a failed look is an error");
    same(els.ytUpdate.disabled, false, "when it cannot be told, updating is still possible");
  }
  // And one that cannot read the installed version.
  st = status({ version: "", outdated: null, version_error: "yt-dlp --version failed: no version" });
  dispatch(els.ytCheck, "click");
  await tick();
  assert(said().indexOf("no version") >= 0 && kind() === "err", "an unreadable version: " + said());
  same(els.ytHave.textContent, words("unknown", "未知"), "no version to show");

  // A version that is no date cannot be compared, and is said so.
  st = status({ version: "custom-7", outdated: null });
  dispatch(els.ytCheck, "click");
  await tick();
  assert(said().indexOf("custom-7") >= 0 && said().indexOf("up to date") < 0 && said().indexOf("已是最新") < 0, "an uncomparable version: " + said());

  // Not installed: how to get it, and nothing to update.
  st = status({ found: false, version: "", latest: "", outdated: null, can_update: false, install: "" });
  await open();
  assert(said().indexOf("winget install yt-dlp.yt-dlp") >= 0, "how to install: " + said());
  same(els.ytHave.textContent, words("not installed", "未安装"), "the installed version of nothing");
  same(els.ytUpdate.disabled, true, "nothing to update");

  // Installed in a way godm does not run: the command to run by hand.
  st = status({ install: "pip", can_update: false, command: 'python -m pip install -U "yt-dlp[default]"' });
  await open();
  assert(said().indexOf('python -m pip install -U "yt-dlp[default]"') >= 0, "the command for pip: " + said());
  same(els.ytUpdate.disabled, true, "godm does not update this one");
  st = status({ install: "scoop", install_name: "Scoop", can_update: false, command: "scoop update yt-dlp" });
  await open();
  assert(said().indexOf("Scoop") >= 0 && said().indexOf("scoop update yt-dlp") >= 0, "named: " + said());

  // An update: asked for, followed while it runs, and its result shown.
  st = status({});
  updateReply = status({ updating: true, step: "yt-dlp" });
  await open();
  dispatch(els.ytUpdate, "click");
  await tick();
  same(count("/api/ytdlp/update"), 1, "the update button asks for an update");
  assert(said().indexOf(words("Updating yt-dlp", "正在更新 yt-dlp")) >= 0, "while it runs: " + said());
  same(els.ytUpdate.disabled, true, "no second update while one runs");
  same(els.ytCheck.disabled, true, "nor a check");
  const polls = count("/api/ytdlp");
  st = status({ updating: true, step: "Deno" });
  runTimers();
  await tick();
  assert(count("/api/ytdlp") > polls, "the page should ask how it is getting on");
  assert(said().indexOf(words("Updating Deno", "正在更新 Deno")) >= 0, "the second step: " + said());
  st = status({ version: "2026.09.15", outdated: false, result: { ok: true, from: "2026.06.01", to: "2026.09.15", changed: true, behind: false, deno: "updated" } });
  runTimers();
  await tick();
  assert(said().indexOf(words("Updated yt-dlp: 2026.06.01 → 2026.09.15.", "已更新 yt-dlp：2026.06.01 → 2026.09.15。")) >= 0, "the result: " + said());
  assert(said().indexOf(words("Deno was updated too.", "Deno 也已更新。")) >= 0, "Deno's part of it: " + said());
  same(kind(), "ok", "an update that worked");
  same(els.ytHave.textContent, "2026.09.15", "the new version");
  const after = count("/api/ytdlp");
  runTimers();
  await tick();
  same(count("/api/ytdlp"), after, "the page goes on asking after the update is over");

  // Other results: nothing new to be had, behind winget's list, failed.
  const shown = async (result, extra) => {
    st = status(Object.assign({ version: result.to || "2026.06.01", outdated: false, result: result }, extra || {}));
    dispatch(els.ytCheck, "click"); // the daemon's answer carries the result
    await tick();
    return said();
  };
  let r = await shown({ ok: true, from: "2026.08.19", to: "2026.08.19", changed: false, behind: true }, { latest: "2026.09.15", outdated: true });
  assert(r.indexOf(words("already the newest version available (2026.08.19)", "已是可获得的最新版本（2026.08.19）")) >= 0, "nothing newer: " + r);
  assert(r.indexOf(words("The newest release is 2026.09.15, which winget does not have yet.", "最新版本 2026.09.15 已发布，但 winget 暂时还没有。")) >= 0, "behind winget: " + r);
  r = await shown({ ok: false, from: "2026.06.01", error: "Installer hash does not match" });
  assert(r.indexOf(words("Update failed: Installer hash does not match", "更新失败：Installer hash does not match")) >= 0 && kind() === "err", "a failure: " + r);
  r = await shown({ ok: true, from: "2026.06.01", to: "2026.09.15", changed: true, behind: false, deno: "failed", deno_error: "exit 1603" });
  assert(r.indexOf(words("Deno could not be updated: exit 1603", "Deno 无法更新：exit 1603")) >= 0 && kind() === "warn", "Deno failing: " + r);

  // Refused: the reason in the page's own words, in red.
  st = status({});
  await open();
  for (const [reason, want] of [
    ["busy", words("in use by a download", "正被下载任务或网页解析占用")],
    ["winget", words("winget was not found", "找不到 winget")],
    ["updating", words("already running", "已有一个更新正在进行")],
    ["somethingnew", "the daemon's own words"],
  ]) {
    updateReply = { ok: false, reason: reason, error: "the daemon's own words" };
    dispatch(els.ytUpdate, "click");
    await tick();
    assert(said().indexOf(want) >= 0 && kind() === "err", reason + ": " + said());
  }

  // The reasons godm worded itself come in the page's language, and what another
  // program said is shown as it came.
  const checked = async (extra) => {
    st = status(Object.assign({ latest: "", outdated: null }, extra));
    dispatch(els.ytCheck, "click");
    await tick();
    return said();
  };
  r = await checked({ latest_error: "GitHub answered 502 Bad Gateway", latest_error_code: "gh-status", latest_error_arg: "502 Bad Gateway" });
  assert(r.indexOf(words("GitHub answered 502 Bad Gateway", "GitHub 返回了 502 Bad Gateway")) >= 0, "a reason in the page's language: " + r);
  r = await checked({ latest_error: "GitHub's limit; try again in 7 minutes", latest_error_code: "gh-limit", latest_error_arg: "7" });
  assert(r.indexOf(words("try again in 7 minutes", "请 7 分钟后再试")) >= 0, "a reason with a number: " + r);
  r = await checked({ latest_error: "GitHub's limit", latest_error_code: "gh-limit" });
  assert(r.indexOf(words("limit on requests", "限制了此地址的请求次数")) >= 0 && r.indexOf("{arg}") < 0, "the same without the number: " + r);
  r = await checked({ latest_error: "something new that godm worded", latest_error_code: "no-such-code-yet" });
  assert(r.indexOf("something new that godm worded") >= 0, "a code the page has no words for falls back to the text: " + r);
  r = await checked({ version: "", version_error: "yt-dlp did not say its version in time", version_error_code: "version-timeout" });
  assert(r.indexOf(words("did not say its version in time", "没有及时给出版本号")) >= 0, "the version timing out: " + r);
  r = await shown({ ok: false, from: "2026.06.01", error: "yt-dlp is in use by another program: close it, then try again", error_code: "winget-inuse" });
  assert(r.indexOf(words("close it, then try again", "请先关闭它，再重试")) >= 0 && kind() === "err", "winget's refusal in the page's language: " + r);
  r = await shown({ ok: false, from: "2026.06.01", error: "winget did not finish in 10 minutes", error_code: "timeout-winget", error_arg: "10" });
  assert(r.indexOf(words("did not finish in 10 minutes", "没有在 10 分钟内完成")) >= 0, "a time out: " + r);
  r = await shown({ ok: false, from: "2026.06.01", error: "after", error_code: "after-version", error_arg: "ModuleNotFoundError: x" });
  assert(r.indexOf("ModuleNotFoundError: x") >= 0 && r.indexOf("{arg}") < 0, "the reason inside the wording: " + r);
  r = await shown({ ok: true, from: "2026.06.01", to: "2026.09.15", changed: true, deno: "failed", deno_error: "Deno is in use", deno_error_code: "winget-inuse" });
  assert(r.indexOf(words("close it, then try again", "请先关闭它，再重试")) >= 0 && kind() === "warn", "Deno's refusal in the page's language: " + r);

  // yt-dlp being the newest is not all there is to update where winget brings
  // Deno too, and the page says what the button would do.
  st = status({ version: "2026.09.15", outdated: false, deno_via_winget: true });
  await open();
  same(els.ytUpdate.disabled, false, "Deno may be behind while yt-dlp is not");
  assert(said().indexOf(words("Update also checks Deno", "还会一并检查 Deno")) >= 0, "what the update would do: " + said());
  r = await shown({ ok: true, from: "2026.09.15", to: "2026.09.15", changed: false, behind: false, deno: "current" }, { deno_via_winget: true });
  assert(r.indexOf(words("Deno is up to date too.", "Deno 也已是最新版本。")) >= 0, "Deno's part of an update that changed nothing: " + r);
  st = status({ version: "2026.09.15", outdated: false });
  await open();
  same(els.ytUpdate.disabled, true, "with no Deno of winget's there is nothing to update");

  // A request for how the update is getting on that fails once is asked again,
  // and one that keeps failing is said so and gives the buttons back, rather
  // than leaving the page on "updating" with both of them off.
  st = status({});
  updateReply = status({ updating: true, step: "yt-dlp" });
  await open();
  dispatch(els.ytUpdate, "click");
  await tick();
  down = true;
  runTimers();
  await tick();
  assert(said().indexOf(words("Updating yt-dlp", "正在更新 yt-dlp")) >= 0, "one lost answer: " + said());
  down = false;
  st = status({ version: "2026.09.15", outdated: false, result: { ok: true, from: "2026.06.01", to: "2026.09.15", changed: true, behind: false } });
  runTimers();
  await tick();
  assert(said().indexOf(words("Updated yt-dlp", "已更新 yt-dlp")) >= 0, "the update was followed on after the lost answer: " + said());
  st = status({});
  await open();
  dispatch(els.ytUpdate, "click");
  await tick();
  down = true;
  for (let i = 0; i < 5; i++) { runTimers(); await tick(); }
  assert(said().indexOf(words("Lost touch with godm", "与 godm 失去联系")) >= 0 && kind() === "err", "answers that keep failing: " + said());
  same(els.ytCheck.disabled, false, "the check button comes back");
  down = false;

  // Opening the dialog during an update follows it instead of looking.
  st = status({ updating: true, step: "yt-dlp" });
  const looks = count("/api/ytdlp/check");
  await open();
  same(count("/api/ytdlp/check"), looks, "a look was made while the file is being replaced");
  assert(said().indexOf(words("Updating yt-dlp", "正在更新 yt-dlp")) >= 0, "following an update that was already running: " + said());

  // Closing the dialog ends the following.
  els.setDlg.hidden = true;
  const quiet = count("/api/ytdlp");
  runTimers();
  await tick();
  same(count("/api/ytdlp"), quiet, "the page asked after the dialog was closed");
};

// The download folder in the Settings dialog: a folder never chosen shows as an
// empty field with the one in use behind it, saving without touching it sends
// nothing, Browse fills in what the chooser answered, and Use the default
// empties the field so that saving goes back to it.
scenarios.downloaddir = async function () {
  tasks = [];
  let saved = null, browseAsked = "", chosen = "";
  const started = "C:\\Users\\me\\Downloads";
  const view = () => ({ ok: true, keep_awake: true, can_keep_awake: true, after_all: "nothing", after_all_options: ["nothing"],
    sort_by_type: false, folders: {}, out_dir: chosen || started, download_dir: chosen, started_dir: started, language: process.env.PAGE_LANG });
  routes["/api/settings"] = (body) => {
    if (body) {
      saved = body;
      if ("download_dir" in body) chosen = body.download_dir;
    }
    return view();
  };
  routes["/api/browse"] = (body, path) => { browseAsked = path; return { ok: true, path: "E:\\Films" }; };
  routes["/api/ytdlp"] = () => ({ ok: true, found: false });
  routes["/api/ytdlp/check"] = () => ({ ok: true, found: false });
  await boot();
  const open = async () => { els.setDlg.hidden = true; dispatch(els.setBtn, "click"); await tick(); };

  await open();
  same(els.setDir.value, "", "a folder never chosen shows as an empty field");
  same(els.setDir.placeholder, started, "with the folder in use behind it");
  assert(els.setDirHelp.innerHTML.indexOf(started) >= 0, "the help says where the default is: " + els.setDirHelp.innerHTML);

  dispatch(els.setGo, "click");
  await tick();
  same(saved, null, "saving an untouched dialog sent settings");

  // Browse asks the daemon's chooser, starting at the folder in use, and fills
  // in its answer; saving sends it.
  await open();
  dispatch(els.setDirBrowse, "click");
  await tick();
  assert(browseAsked.indexOf("/api/browse?current=" + encodeURIComponent(started)) === 0, "the chooser should start at the folder in use: " + browseAsked);
  same(els.setDir.value, "E:\\Films", "the folder chosen is filled in");
  same(els.setDirBrowse.disabled, false, "Browse can be pressed again");
  dispatch(els.setGo, "click");
  await tick();
  same(saved && saved.download_dir, "E:\\Films", "saving sends the folder chosen");

  // Opening again shows it; Use the default empties it, and saving goes back.
  saved = null;
  await open();
  same(els.setDir.value, "E:\\Films", "the folder chosen shows when the dialog opens again");
  dispatch(els.setDirReset, "click");
  same(els.setDir.value, "", "Use the default empties the field");
  dispatch(els.setGo, "click");
  await tick();
  same(saved && saved.download_dir, "", "saving an emptied field goes back to the default");

  // A folder that is not a full path is refused on the page, in the page's
  // language, and the dialog stays open to fix it.
  saved = null;
  await open();
  els.setDir.value = "Films";
  dispatch(els.setGo, "click");
  await tick();
  same(saved, null, "a folder that is not a full path was sent");
  same(els.setDlg.hidden, false, "the dialog closed on a mistake");
  assert(/ err/.test(toast().className), "the mistake should be shown as one");
  assert(toast().textContent.indexOf(words("full path", "完整路径")) >= 0, "the toast says why: " + toast().textContent);
};

scenarios.onfailure = async function () {
  let saved = null, every = 5;
  const view = () => ({ ok: true, keep_awake: true, can_keep_awake: true, after_all: "nothing", after_all_options: ["nothing", "shutdown"],
    sort_by_type: false, folders: {}, out_dir: "C:\\d", language: process.env.PAGE_LANG, on_failure: "retry", retry_every: every, retry_times: 3 });
  routes["/api/settings"] = (body) => { if (body) saved = body; return view(); };
  routes["/api/ytdlp"] = () => ({ ok: true, found: false });
  routes["/api/ytdlp/check"] = () => ({ ok: true, found: false });
  routes["/api/tasks"] = () => ({ tasks: tasks, limit: limit, after_all: "nothing", speed_limit: 0, language: "", on_failure: "retry", retry_every: 5, retry_times: 3 });
  tasks = [
    task("f", { state: "error", error: "HTTP 500", speed: 0, retries: 1, retry_at: new Date(Date.now() + 240400).toISOString() }),
    task("e", { state: "needs_refresh", speed: 0, retry_at: new Date(Date.now() + 60400).toISOString() }),
    task("g", { state: "error", error: "HTTP 500", speed: 0, retries: 3 }),
    task("r", { state: "running", retry_at: new Date(Date.now() + 60400).toISOString() }), // left over, not waiting
  ];
  await boot();

  // The cards say when, and which try, and the one given up on says so.
  const list = els.list.innerHTML;
  assert(list.indexOf(words("godm tries again in 4m 0s (try 2 of 3)", "4分0秒后自动重试（第 2/3 次）")) >= 0, "the failed card should say when it is tried again: " + list);
  assert(list.indexOf(words("godm tries again in 1m 0s (try 1 of 3)", "1分0秒后自动重试（第 1/3 次）")) >= 0, "so should the expired one: " + list);
  assert(list.indexOf(words("tried again 3 times, then given up", "已自动重试 3 次，不再重试")) >= 0, "the card given up on should say so: " + list);
  same(list.split(words("Stop trying", "不再重试") + "</button>").length - 1, 2, "the two to be tried again can be stopped, and only they");
  assert(els.stats.innerHTML.indexOf(words("2 failed, to be tried again", "2 个失败的等待自动重试")) >= 0, "the top line should count them: " + els.stats.innerHTML);

  const open = async () => { els.setDlg.hidden = true; dispatch(els.setBtn, "click"); await tick(); };
  const shown = () => [els.everyBox.hidden, els.timesBox.hidden];
  await open();
  same(els.failRow.hidden, false, "the choice should be in the dialog");
  same(els.setFail.value, "retry", "the choice made");
  same(els.setEvery.value, "5", "the minutes");
  same(els.setTimes.value, "3", "the tries");
  same(JSON.stringify(shown()), "[false,false]", "Try again a few times shows the minutes and the tries");
  assert(els.setTimes.innerHTML.indexOf(">" + words("once", "1 次") + "<") >= 0, "one try is said as one: " + els.setTimes.innerHTML);
  dispatch(els.setGo, "click");
  await tick();
  same(saved, null, "saving an untouched dialog sent settings");

  await open();
  els.setFail.value = "forever";
  dispatch(els.setFail, "change");
  same(JSON.stringify(shown()), "[false,true]", "Keep trying shows the minutes only");
  els.setEvery.value = "30";
  dispatch(els.setGo, "click");
  await tick();
  same(JSON.stringify(saved), JSON.stringify({ on_failure: "forever", retry_every: 30 }), "saving sends what changed");

  saved = null;
  await open();
  els.setFail.value = "give_up";
  dispatch(els.setFail, "change");
  same(JSON.stringify(shown()), "[true,true]", "Don't try again shows neither");
  dispatch(els.setGo, "click");
  await tick();
  same(JSON.stringify(saved), JSON.stringify({ on_failure: "give_up" }), "saving sends the choice alone");

  // Minutes set some other way are offered as they are.
  every = 7;
  saved = null;
  await open();
  same(els.setEvery.value, "7", "minutes the dialog does not offer should still show");
  assert(els.setEvery.innerHTML.indexOf(words("7 min", "7 分钟")) >= 0, "and be named: " + els.setEvery.innerHTML);
  dispatch(els.setGo, "click");
  await tick();
  same(saved, null, "saving them as they are sent settings");
};

// The download list: a tab of its own that the Unfinished tab leaves out, what
// a try found, Start rather than Resume, starting the whole list, the time it
// starts by itself, removing what a try left, and Add to list in Add links.
scenarios.downloadlist = async function () {
  const onList = (id, state, extra) => task(id, Object.assign({ state: state, size: 1 << 30, received: 8 << 20, later: true }, extra));
  // One slot, taken by r; s was started from the list and waits for it.
  limit = 1;
  tasks = [
    onList("w", "listed", { try_speed: 5 << 20, path: "C:\\d\\w.zip", speed: 0 }),
    onList("t", "running"),
    onList("q", "queued", { received: 0, size: -1 }),
    task("p", { state: "paused" }),
    task("r", { state: "running" }),
    task("s", { state: "queued", from_list: true, received: 8 << 20, size: 1 << 30 }),
  ];
  let started = 0, saved = null, batch = null, limited = null;
  routes["/api/list/start"] = () => { started++; return { ok: true, started: 3 }; };
  routes["/api/limit"] = (_, path) => { limited = path; limit = 4; return { ok: true, limit: 4 }; };
  routes["/api/settings"] = (body) => {
    saved = body;
    return { ok: true, list_at: body.list_at, list_daily: body.list_daily, list_next: body.list_at ? "2030-01-02T03:15:00Z" : "0001-01-01T00:00:00Z" };
  };
  routes["/api/batch"] = (body) => { batch = body; return { ok: true, ids: body.items.map((_, i) => "n" + i) }; };
  await boot();

  const tabs = els.tabs.innerHTML;
  assert(/data-tab="list">[^<]*<span class="n">4</.test(tabs), "the list's tab should count what is on it and what was started from it: " + tabs);
  assert(/data-tab="active">[^<]*<span class="n">3</.test(tabs), "Unfinished should leave out what waits on the list, not what was started from it: " + tabs);
  same(els.listBar.hidden, true, "the list's bar shows on another tab");

  dispatch(child(els.tabs, "button", { "data-tab": "list" }), "click");
  same(els.listBar.hidden, false, "the list's bar is missing on its tab");
  const html = els.list.innerHTML;
  assert(html.indexOf('data-task="w"') >= 0 && html.indexOf('data-task="t"') >= 0 && html.indexOf('data-task="q"') >= 0, "the list's tab should show what is on it");
  assert(html.indexOf('data-task="s"') >= 0, "the list's tab should go on showing what was started from it until it is done");
  assert(html.indexOf(words("#1 in line", "排队第 1 位")) >= 0, "and where it waits in the queue");
  assert(html.indexOf('data-task="p"') < 0 && html.indexOf('data-task="r"') < 0, "the list's tab shows a download that is not on it");

  // s waits because every slot is taken, and the bar says so beside the limit.
  same(els.listQueue.hidden, false, "the bar does not say the list waits for a slot");
  assert(els.listQueueText.textContent.indexOf(words("1 download from the list is waiting", "列表里 1 个在排队")) >= 0, "the bar should say how many wait: " + els.listQueueText.textContent);
  assert(els.listQueueText.textContent.indexOf("1/1") >= 0 || els.listQueueText.textContent.indexOf("1 of 1") >= 0, "and that the slots are full: " + els.listQueueText.textContent);
  same(els.listLimit.value, "1", "the bar's limit should be the limit");
  els.listLimit.value = "4";
  dispatch(els.listLimit, "change");
  await tick();
  assert(limited && limited.indexOf("n=4") >= 0, "changing the bar's limit should send it: " + limited);
  same(els.limit.value, "4", "the top bar's limit should follow the one changed in the list bar");
  await poll();
  same(els.listQueue.hidden, true, "with slots free the bar still says the list waits for one");
  tasks = tasks.map((x) => (x.id === "s" ? Object.assign({}, x, { state: "done" }) : x));
  await poll();
  assert(els.list.innerHTML.indexOf('data-task="s"') < 0, "a download started from the list should leave it once done");
  assert(html.indexOf(words("Waiting", "待下载")) >= 0, "a download waiting on the list should say so");
  assert(html.indexOf(words("Trying", "试下中")) >= 0, "a try under way should say so");
  assert(html.indexOf(words("Tried 8.0 MB of 1.0 GB", "已试下 8.0 MB，共 1.0 GB")) >= 0, "a waiting download should say what its try fetched");
  assert(html.indexOf(words("5.0 MB/s in the try", "试下速度 5.0 MB/s")) >= 0, "and how fast");
  assert(html.indexOf(words("Waiting to be tried", "等待试下")) >= 0, "one not yet tried should say so, not where it is in the queue");
  assert(html.indexOf('data-act="resume" data-id="w">' + words("Start", "开始") + "<") >= 0, "a waiting download is started with Start");
  same(els.listStart.disabled, false, "Start all is off with downloads on the list");

  dispatch(els.listStart, "click");
  await tick();
  same(started, 1, "Start all did not ask the daemon to start the list");

  // Starting by the clock: nothing is sent while the time is being typed, then
  // the last of it is.
  els.listTimed.checked = true;
  els.listAt.value = "03:15";
  els.listDaily.value = "1";
  dispatch(els.listAt, "change");
  dispatch(els.listDaily, "change");
  same(saved, null, "a time being typed was sent at once");
  // The list is polled while the send waits, and the daemon still says the
  // clock is off: that must not undo what was just chosen.
  await poll();
  same(els.listTimed.checked, true, "a poll undid the box just ticked");
  runTimers();
  await tick();
  same(saved && saved.list_at, "03:15", "the time set");
  same(saved.list_daily, true, "every day");
  assert(els.listNext.textContent.indexOf("03:15") >= 0 || els.listNext.textContent.indexOf(":15") >= 0, "the bar should say when the list starts next: " + els.listNext.textContent);
  els.listTimed.checked = false;
  dispatch(els.listTimed, "change");
  runTimers();
  await tick();
  same(saved.list_at, "", "turning the clock off sends no time");

  // What a try left behind goes with the download it was for; anything else
  // is kept unless asked.
  dispatch(child(els.list, "button", { "data-act": "remove", "data-id": "w" }), "click");
  same(els.rmDelete.checked, true, "removing a download on the list should take its tried part too");
  dispatch(child(els.list, "button", { "data-act": "remove", "data-id": "p" }), "click");
  same(els.rmDelete.checked, false, "removing another download should keep its file unless asked");

  // Add links has Add to list beside Add.
  els.addText.value = "https://files.test/a.zip\nhttps://files.test/b.zip";
  dispatch(els.addText, "input");
  same(els.addLater.disabled, false, "Add to list is off with links in the box");
  dispatch(els.addLater, "click");
  await tick();
  assert(batch && batch.items.length === 2 && batch.items.every((i) => i.later === true), "Add to list should send the links for the list: " + JSON.stringify(batch));
  same(page("S.tab"), "list", "after adding to the list the page should show the list");
  els.addText.value = "https://files.test/c.zip";
  dispatch(els.addText, "input");
  dispatch(els.addGo, "click");
  await tick();
  same(batch.items[0].later, false, "Add should send the link to start now");

  // What was started from the list is shown with it, but Start all has
  // nothing to start in it.
  tasks = [task("r", { state: "running" }), task("s", { state: "queued", from_list: true })];
  await poll();
  dispatch(child(els.tabs, "button", { "data-tab": "list" }), "click");
  assert(els.list.innerHTML.indexOf('data-task="s"') >= 0, "the list should still show what was started from it");
  same(els.listStart.disabled, true, "Start all is on with nothing on the list left to start");
};

const run = scenarios[which];
if (!run) { console.log("no scenario " + which); process.exit(2); }
run().then(() => console.log("ok " + which), (e) => { console.log("FAIL " + which + ": " + (e && e.message)); process.exit(1); });
`

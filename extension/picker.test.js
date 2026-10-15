"use strict";

// Run with: node --test extension/picker.test.js
//
// picker.js is a page script, so it is run here as the browser would run it,
// against a stand-in for the few parts of the document and of chrome.* that it
// touches. What is under test is what the person is shown to pick from, and
// what a press of Download sends.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { install } = require("./fakechrome.test.js");

const IDS = ["title", "page", "chips", "rows", "empty", "toggleShown", "count", "go", "search", "selAll", "selNone", "selFiles", "cancel", "status", "conns"];

class FakeElement {
  constructor() {
    this.innerHTML = "";
    this.textContent = "";
    this.className = "";
    this.hidden = false;
    this.checked = false;
    this.indeterminate = false;
    this.disabled = false;
    this.value = "";
    this.listeners = {};
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  fire(type, event) {
    for (const fn of this.listeners[type] || []) fn(event || { target: this });
  }
}

const settle = () => new Promise((resolve) => setImmediate(resolve));

const PAGE = "https://video.test/watch/1";
const HLS = "https://cdn.test/hls/master.m3u8";
const CLIP = "https://files.test/clip.mp4";
const ZIP = "https://video.test/files/extras.zip";
const ABOUT = "https://video.test/about";

const SNIFFED = [
  { url: HLS, kind: "hls", type: "application/vnd.apple.mpegurl", size: 0, title: "The Film - video.test" },
  { url: CLIP, kind: "file", type: "video/mp4", size: 80 * 1024 * 1024, title: "The Film - video.test" }
];
const LINKS = [
  { url: ZIP, text: "Extras", kind: "link" },
  { url: ABOUT, text: "About us", kind: "link" }
];
const DATA = { page: PAGE, title: "The Film - video.test", links: LINKS, videos: SNIFFED };

// open runs the picker with this list of links and videos, as the worker would
// have left it.
async function open(data) {
  const ui = {};
  for (const id of IDS) ui[id] = new FakeElement();
  const doc = new FakeElement();
  doc.getElementById = (id) => ui[id];
  doc.querySelectorAll = () => [];
  doc.documentElement = {};
  doc.title = "";

  const sent = [];
  const closed = [];
  const chrome = Object.assign({}, install("en"), {
    storage: {
      sync: { get: async (defaults) => defaults },
      session: { get: async (key) => ({ [key]: data }), remove: () => {} }
    },
    runtime: {
      sendMessage: async (message) => {
        sent.push(message);
        return { ok: true, ids: message.items.map((_, i) => "t" + i) };
      }
    }
  });
  const context = vm.createContext({
    document: doc,
    chrome,
    location: { search: "?k=pick-1" },
    URLSearchParams,
    URL,
    window: { close: () => closed.push("window.close") },
    setTimeout: (fn) => fn
  });
  for (const file of ["i18n.js", "picker.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), "utf8"), context, { filename: file });
  }
  await settle();
  install("en");
  return { ui, doc, sent, closed };
}

const unesc = (s) => s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");

// rows reads what the table shows.
function rows(p) {
  const out = [];
  const re = /<tr class="([^"]*)" data-url="([^"]*)">(.*?)<\/tr>/g;
  for (const m of p.ui.rows.innerHTML.matchAll(re)) {
    const cell = (cls) => {
      const c = new RegExp('<div class="' + cls + '">(.*?)</div>').exec(m[3]);
      return c ? unesc(c[1]) : "";
    };
    out.push({
      url: unesc(m[2]),
      checked: !m[1].includes("off"),
      name: cell("name"),
      text: cell("text"),
      type: unesc(/<td class="ext">(.*?)<\/td>/.exec(m[3])[1])
    });
  }
  return out;
}

// chips reads the filter buttons, in order.
function chips(p) {
  return [...p.ui.chips.innerHTML.matchAll(/<button class="chip( on)?" data-type="([^"]*)"><b>(.*?)<\/b><span>(\d+)<\/span>/g)].map((m) => ({
    on: !!m[1],
    key: unesc(m[2]),
    label: unesc(m[3]),
    count: Number(m[4])
  }));
}

const clickRow = (p, url) => p.ui.rows.fire("click", { target: { closest: () => ({ getAttribute: () => url }) } });
const clickChip = (p, key) => p.ui.chips.fire("click", { target: { closest: () => ({ getAttribute: () => key }) } });
const urls = (p) => rows(p).map((r) => r.url);

test("the videos a page played are listed first, each with its name, type and size", async () => {
  const p = await open(DATA);
  assert.deepEqual(urls(p), [HLS, CLIP, ZIP, ABOUT]);
  const [stream, file] = rows(p);
  assert.equal(stream.name, "The Film - video.test", "a playlist is told by the page's name, not index.m3u8");
  assert.equal(stream.type, "HLS");
  assert.equal(stream.text, "HLS stream · best quality");
  assert.equal(file.name, "clip.mp4");
  assert.equal(file.type, "FILE");
  assert.equal(file.text, "video/mp4 · 80.0 MiB");
  assert.equal(rows(p)[2].type, "zip", "the page's links are as they were");
});

test("they have a group of their own beside the file types", async () => {
  const p = await open(DATA);
  const [first, ...rest] = chips(p);
  assert.equal(first.label, "Videos this page played");
  assert.equal(first.count, 2);
  assert.deepEqual(rest.map((c) => [c.label, c.count]), [["zip", 1], ["(none)", 1]]);

  clickChip(p, first.key);
  assert.deepEqual(urls(p), [HLS, CLIP], "the group filters to the videos");
  assert.equal(chips(p)[0].on, true);
  clickChip(p, first.key);
  assert.equal(rows(p).length, 4, "and a second press lets the rest back in");
});

test("a video can be found by the filter box like any link", async () => {
  const p = await open(DATA);
  p.ui.search.value = "film";
  p.ui.search.fire("input", { target: p.ui.search });
  assert.deepEqual(urls(p), [HLS], "by the page's name");
  p.ui.search.fire("input", { target: { value: " CLIP " } });
  assert.deepEqual(urls(p), [CLIP], "by its file name");
  p.ui.search.fire("input", { target: { value: "mpegurl" } });
  assert.deepEqual(urls(p), [], "the type is not searched, only the name, the text and the address");
});

test("a file of a listed type starts picked, a stream does not", async () => {
  const p = await open(DATA);
  assert.deepEqual(rows(p).filter((r) => r.checked).map((r) => r.url), [CLIP, ZIP]);
  assert.equal(p.ui.go.textContent, "Download 2 files");
});

test("a stream that was picked is sent as one, named for the page, with the rest in order", async () => {
  const p = await open(DATA);
  clickRow(p, HLS);
  assert.equal(rows(p)[0].checked, true);
  assert.equal(p.ui.go.textContent, "Download 3 files");
  p.ui.go.fire("click");
  await settle();

  assert.equal(p.sent.length, 1);
  assert.equal(p.sent[0].type, "godm-batch");
  assert.equal(p.sent[0].referrer, PAGE, "the page that played them is the referrer");
  assert.deepEqual(p.sent[0].items, [
    { url: HLS, kind: "hls", filename: "The Film - video.test" },
    { url: CLIP },
    { url: ZIP }
  ]);
  assert.match(p.ui.status.textContent, /Added 3 downloads/);
});

test("a stream with no name of its own is named for the page", async () => {
  const bare = { page: PAGE, title: "The Film", links: [], videos: [{ url: HLS, kind: "hls", type: "", size: 0, title: "" }] };
  const p = await open(bare);
  assert.equal(rows(p)[0].name, "master.m3u8", "the row has nothing better to say");
  clickRow(p, HLS);
  p.ui.go.fire("click");
  await settle();
  assert.deepEqual(p.sent[0].items, [{ url: HLS, kind: "hls", filename: "The Film" }]);
});

test("a page's own links are sent as they always were, and a stream is the only thing with a kind", async () => {
  const p = await open({ page: PAGE, title: "", links: LINKS });
  assert.deepEqual(chips(p).map((c) => c.label), ["zip", "(none)"], "no group for videos that were not played");
  clickRow(p, ABOUT);
  p.ui.go.fire("click");
  await settle();
  assert.deepEqual(p.sent[0].items, [{ url: ZIP }, { url: ABOUT }]);

  const old = await open({ page: PAGE, title: "", links: LINKS, videos: undefined });
  assert.equal(rows(old).length, 2, "a list with no videos in it is read as having none");
});

test("a file the page links to and also played is listed once, as the video it is", async () => {
  const both = { page: PAGE, title: "", links: [{ url: CLIP, text: "Watch", kind: "media" }].concat(LINKS), videos: SNIFFED };
  const p = await open(both);
  assert.deepEqual(urls(p), [HLS, CLIP, ZIP, ABOUT]);
  assert.equal(rows(p)[1].type, "FILE");
  assert.equal(chips(p)[0].count, 2);
});

test("a list that has expired says so", async () => {
  const p = await open(undefined);
  assert.equal(p.ui.title.textContent, "This link list has expired");
  assert.equal(p.ui.empty.hidden, false);
});

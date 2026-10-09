"use strict";

// Run with: node --test extension/i18n.test.js
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const { expand, install, messagesFor } = require("./fakechrome.test.js");

const en = messagesFor("en");
const zh = messagesFor("zh_CN");

// Every file the extension is made of, except the tests and the messages.
const sources = fs
  .readdirSync(__dirname)
  .filter((f) => /\.(js|html|json)$/.test(f) && !/\.test\.js$/.test(f))
  .map((f) => ({ name: f, text: fs.readFileSync(path.join(__dirname, f), "utf8") }));

function holders(message) {
  return [...message.matchAll(/\$([A-Za-z0-9_@]+)\$/g)].map((m) => m[1].toLowerCase());
}

test("English and Chinese have the same messages", () => {
  assert.deepEqual(Object.keys(zh).sort(), Object.keys(en).sort());
});

test("a message name is something Chrome accepts", () => {
  for (const key of Object.keys(en)) assert.match(key, /^[A-Za-z0-9_@]+$/, key);
});

test("every message has text, and none is left empty in Chinese", () => {
  for (const [locale, messages] of [["en", en], ["zh_CN", zh]]) {
    for (const [key, entry] of Object.entries(messages)) {
      assert.equal(typeof entry.message, "string", `${locale} ${key}`);
      assert.ok(entry.message.trim(), `${locale} ${key} is empty`);
    }
  }
  for (const [key, entry] of Object.entries(zh)) {
    assert.match(entry.message, /[一-鿿]/, `zh_CN ${key} has no Chinese in it: ${entry.message}`);
  }
});

test("placeholders are declared, used, and the same in both languages", () => {
  for (const key of Object.keys(en)) {
    const declared = (m) => Object.keys(m[key].placeholders || {}).sort();
    const used = (m) => [...new Set(holders(m[key].message))].sort();
    assert.deepEqual(used(en), declared(en), `en ${key}: used and declared placeholders differ`);
    assert.deepEqual(used(zh), declared(zh), `zh_CN ${key}: used and declared placeholders differ`);
    assert.deepEqual(declared(zh), declared(en), `${key}: the languages declare different placeholders`);
    for (const name of declared(en)) {
      assert.deepEqual(zh[key].placeholders[name], en[key].placeholders[name], `${key} ${name}`);
      assert.match(en[key].placeholders[name].content, /^\$[1-9]$/, `${key} ${name}`);
    }
  }
});

test("no message has a stray dollar sign", () => {
  for (const [locale, messages] of [["en", en], ["zh_CN", zh]]) {
    for (const [key, entry] of Object.entries(messages)) {
      const bare = entry.message.replace(/\$[A-Za-z0-9_@]+\$/g, "");
      assert.ok(!bare.includes("$"), `${locale} ${key}: ${entry.message}`);
    }
  }
});

test("the same markup survives translation", () => {
  const tags = (s) => (s.match(/<\/?[a-z]+[^>]*>/g) || []).join("");
  for (const key of Object.keys(en)) assert.equal(tags(zh[key].message), tags(en[key].message), key);
});

test("the manifest names English as the default and only messages that exist", () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, "manifest.json"), "utf8"));
  assert.equal(manifest.default_locale, "en");
  assert.ok(fs.existsSync(path.join(__dirname, "_locales", manifest.default_locale, "messages.json")));
  const named = [...JSON.stringify(manifest).matchAll(/__MSG_([A-Za-z0-9_@]+)__/g)].map((m) => m[1]);
  assert.ok(named.includes("ext_name") && named.includes("ext_description"));
  for (const key of named) assert.ok(en[key], `the manifest names ${key}`);
});

// A key is found wherever the extension names one: in a call, in a table of
// them, in an attribute, in the manifest. The names begin with the page or the
// kind of thing they belong to, and any string that is exactly one is taken for
// a use, so a state looked up through a table is counted too.
test("every message the extension uses exists, and every message is used", () => {
  const areas = [...new Set(Object.keys(en).map((k) => k.split("_")[0]))];
  const shape = new RegExp(`["']((?:${areas.join("|")})_[A-Za-z0-9_@]*)["']`, "g");
  const calls = /\b(?:tr|getMessage)\(\s*["']([A-Za-z0-9_@]+)["']/g;
  const attrs = /data-i18n(?:-title|-ph)?="([A-Za-z0-9_@]+)"/g;
  const named = /__MSG_([A-Za-z0-9_@]+)__/g;

  const used = new Set();
  for (const { name, text } of sources) {
    // The manifest's own keys (options_page, say) look like names too, so only
    // the places it names a message count there.
    const patterns = name === "manifest.json" ? [named] : [shape, calls, attrs, named];
    for (const re of patterns) {
      for (const m of text.matchAll(re)) {
        used.add(m[1]);
        assert.ok(en[m[1]], `${name} uses ${m[1]}, which _locales/en does not have`);
      }
    }
  }
  for (const key of Object.keys(en)) assert.ok(used.has(key), `${key} is in the messages but nothing uses it`);
});

test("every page loads i18n.js before any script of its own", () => {
  const pages = sources.filter((s) => s.name.endsWith(".html"));
  assert.ok(pages.length >= 4);
  for (const { name, text } of pages) {
    const first = text.search(/<script src="(?!i18n\.js)/);
    const i18n = text.indexOf('<script src="i18n.js">');
    assert.ok(i18n >= 0, `${name} does not load i18n.js`);
    assert.ok(first < 0 || i18n < first, `${name} loads i18n.js after a script that needs it`);
  }
});

test("the service worker loads i18n.js before handoff.js, which speaks through it", () => {
  const worker = fs.readFileSync(path.join(__dirname, "background.js"), "utf8");
  const m = /importScripts\(([^)]*)\)/.exec(worker);
  const files = [...m[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]);
  assert.ok(files.includes("i18n.js"));
  assert.ok(files.indexOf("i18n.js") < files.indexOf("handoff.js"));
});

test("a message fills its placeholders in the order the English gives them", () => {
  install("en");
  const { tr } = require("./i18n.js");
  assert.equal(tr("picker_count", [3, 10]), "3 of 10 links selected");
  assert.equal(tr("picker_count_shown", [3, 10, 4]), "3 of 10 links selected · 4 shown");
  assert.equal(tr("popup_conns", 6), "6 conn");
  assert.equal(tr("no_such_message"), "no_such_message");
  install("zh_CN");
  assert.match(tr("picker_count", [3, 10]), /3.*10/);
  assert.match(tr("picker_count_shown", [3, 10, 4]), /3.*10.*4/);
  install("en");
});

test("a Chinese message can put the values in a different order", () => {
  const entry = { message: "$B$ then $A$", placeholders: { a: { content: "$1" }, b: { content: "$2" } } };
  assert.equal(expand(entry, ["x", "y"]), "y then x");
});

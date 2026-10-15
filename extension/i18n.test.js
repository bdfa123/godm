"use strict";

// Run with: node --test extension/i18n.test.js
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { expand, install, messagesFor, ownFiles } = require("./fakechrome.test.js");

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

// ---------- the language option ----------

const settle = () => new Promise((resolve) => setImmediate(resolve));

// speak loads i18n.js the way a page or the worker does, with the browser set to
// browserLanguage and the options page having saved language (nothing, if
// undefined). It returns the context once the language has been read.
async function speak(browserLanguage, language, extra) {
  const base = install(browserLanguage);
  const stored = language === undefined ? {} : { language };
  const chrome = Object.assign({}, base, {
    storage: { sync: { get: async (defaults) => Object.assign({}, defaults, stored) } },
    runtime: { getURL: (p) => "chrome-extension://godm-extension-id/" + p }
  });
  const context = vm.createContext(
    Object.assign({ chrome, fetch: ownFiles("godm-extension-id"), console: { warn() {} } }, extra)
  );
  vm.runInContext(fs.readFileSync(path.join(__dirname, "i18n.js"), "utf8"), context, { filename: "i18n.js" });
  await vm.runInContext("i18nReady", context);
  install("en");
  return context;
}

test("with no language chosen, or automatic, the words are the browser's", async () => {
  for (const language of [undefined, "auto"]) {
    assert.equal((await speak("en", language)).tr("picker_cancel"), "Cancel");
    assert.equal((await speak("zh_CN", language)).tr("picker_cancel"), "取消");
  }
});

test("a language that is chosen is spoken whatever the browser is set to", async () => {
  const inChinese = await speak("en", "zh_CN");
  assert.equal(inChinese.tr("picker_cancel"), "取消");
  assert.equal(inChinese.tr("popup_empty"), "还没有下载任务。");
  const inEnglish = await speak("zh_CN", "en");
  assert.equal(inEnglish.tr("picker_cancel"), "Cancel");
  assert.equal(inEnglish.tr("popup_empty"), "No downloads yet.");
});

test("a chosen language fills in values as the browser would have", async () => {
  // The browser's own reading of the same messages is the stand-in for Chrome,
  // so this is the choice agreeing with Chrome on every message there is.
  for (const [locale, other] of [["en", "zh_CN"], ["zh_CN", "en"]]) {
    const chosen = await speak(other, locale);
    const native = await speak(locale);
    for (const key of Object.keys(messagesFor(locale))) {
      const subs = ["first", "second", "third"];
      assert.equal(chosen.tr(key, subs), native.tr(key, subs), `${locale} ${key}`);
      assert.equal(chosen.tr(key), native.tr(key), `${locale} ${key} with no values`);
    }
  }
  const inChinese = await speak("en", "zh_CN");
  assert.equal(inChinese.tr("picker_count", [3, 10]), "已选择 3 / 10 个链接");
  assert.equal(inChinese.tr("popup_conns", 6), "6 个连接");
});

test("anything that cannot be read leaves the browser's language, not a broken page", async () => {
  assert.equal((await speak("en", "fr")).tr("picker_cancel"), "Cancel", "a language there is none of");
  assert.equal((await speak("zh_CN", 7)).tr("picker_cancel"), "取消");
  const noFile = await speak("en", "zh_CN", { fetch: async () => ({ ok: false, status: 404 }) });
  assert.equal(noFile.tr("picker_cancel"), "Cancel", "a file that is not there");
  const broken = await speak("en", "zh_CN", { fetch: async () => { throw new TypeError("offline"); } });
  assert.equal(broken.tr("picker_cancel"), "Cancel", "a file that cannot be fetched");

  // Storage that will not answer is the default, and so is no storage at all.
  const base = install("zh_CN");
  const deaf = vm.createContext({ chrome: Object.assign({}, base, { storage: { sync: { get: async () => { throw new Error("no"); } } } }) });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "i18n.js"), "utf8"), deaf);
  await vm.runInContext("i18nReady", deaf);
  assert.equal(deaf.tr("picker_cancel"), "取消");
  install("en");
});

test("a message the chosen language lacks is the browser's, and one nobody has is its own name", async () => {
  const partial = await speak("en", "zh_CN", {
    fetch: async () => ({ ok: true, json: async () => ({ picker_cancel: { message: "放弃" } }) })
  });
  assert.equal(partial.tr("picker_cancel"), "放弃");
  assert.equal(partial.tr("picker_download"), "Download");
  assert.equal(partial.tr("no_such_message"), "no_such_message");
});

test("of two choices made close together the later one is the language spoken", async () => {
  const waiting = {};
  const context = await speak("en", undefined, {
    fetch: (url) => new Promise((resolve) => {
      waiting[/_locales\/([^/]+)\//.exec(url)[1]] = () =>
        ownFiles("godm-extension-id")(url).then(resolve);
    })
  });
  const first = vm.runInContext('useLanguage("zh_CN")', context);
  const second = vm.runInContext('useLanguage("en")', context);
  waiting.en();
  await second;
  waiting.zh_CN(); // the first one's file comes in after the second one has been used
  await first;
  assert.equal(context.tr("picker_cancel"), "Cancel");
});

test("a page is written in the browser's language at once and again in the chosen one", async () => {
  const label = { dataset: { i18n: "picker_cancel" }, textContent: "" };
  const tip = { dataset: { i18nTitle: "picker_toggle_tip" }, title: "" };
  const hint = { dataset: { i18nPh: "picker_filter_ph" }, placeholder: "" };
  const document = {
    documentElement: {},
    querySelectorAll: (sel) => (sel === "[data-i18n]" ? [label] : sel === "[data-i18n-title]" ? [tip] : [hint])
  };
  const base = install("en");
  const chrome = Object.assign({}, base, {
    storage: { sync: { get: async (d) => Object.assign({}, d, { language: "zh_CN" }) } },
    runtime: { getURL: (p) => "chrome-extension://godm-extension-id/" + p }
  });
  const context = vm.createContext({ chrome, document, fetch: ownFiles("godm-extension-id"), console });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "i18n.js"), "utf8"), context);
  assert.equal(label.textContent, "Cancel", "before the choice is read the page has words in it");
  assert.equal(document.documentElement.lang, "en");
  await vm.runInContext("i18nReady", context);
  assert.equal(label.textContent, "取消");
  assert.equal(tip.title, "选择所有显示的链接");
  assert.equal(hint.placeholder, "按名称、文本或地址筛选");
  assert.equal(document.documentElement.lang, "zh-CN");
  install("en");
});

test("i18n.js lists the languages by the folders the extension has", () => {
  const folders = fs.readdirSync(path.join(__dirname, "_locales")).sort();
  const { LANGUAGES } = require("./i18n.js");
  assert.deepEqual([...LANGUAGES].sort(), folders);
});

test("a choice made while the saved one is still being read is not undone by it", async () => {
  // A worker that has only just woken reads the setting while the options page
  // is saving a new one; the read answers late, with what it saw first.
  let answer;
  const late = new Promise((resolve) => (answer = resolve));
  const chrome = Object.assign({}, install("en"), {
    storage: { sync: { get: () => late } },
    runtime: { getURL: (p) => "chrome-extension://godm-extension-id/" + p }
  });
  const context = vm.createContext({ chrome, fetch: ownFiles("godm-extension-id"), console });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "i18n.js"), "utf8"), context);
  await vm.runInContext('useLanguage("zh_CN")', context);
  answer({ language: "auto" });
  await vm.runInContext("i18nReady", context);
  assert.equal(context.tr("picker_cancel"), "取消");
  install("en");
});

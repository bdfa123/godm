"use strict";

// The words the extension says come from _locales. By default they are in the
// language the browser itself is set to, which is how Chrome picks an
// extension's language; the options page can name one instead (English or
// 简体中文) for a person whose browser speaks another language than they want
// godm to. English is the default_locale and so the fallback either way.
//
// Chrome only ever hands out the messages of its own language, so a language
// that was named is read from the extension's own _locales file, here, and tr
// looks in it first. That read is asynchronous, so a script that says anything
// as it starts has to wait for i18nReady. The name and description in
// chrome://extensions come from the manifest, which only the browser reads, and
// stay in the browser's language.
//
// To add a string: a key in _locales/en/messages.json and in zh_CN, then tr(key)
// in script, or a data-i18n attribute on an element. The manifest names a key
// between a pair of double underscores after MSG_. i18n.test.js checks that
// both files have the same keys and that every key the extension uses is in them.

// What the setting holds when nobody has chosen: speak the browser's language.
const LANGUAGE_DEFAULT = "auto";
// The languages there are words for, named as their folders under _locales.
const LANGUAGES = ["en", "zh_CN"];

let spoken = null; // the messages of the language that was named, or null to leave it to the browser
let spokenName = ""; // which language that is
let languageTurn = 0;

// fillMessage does to one message what chrome.i18n.getMessage does: $name$
// becomes what the placeholder stands for, and $1 to $9 in that become the
// substitutions.
function fillMessage(entry, subs) {
  const holders = entry.placeholders || {};
  return String(entry.message)
    .replace(/\$([A-Za-z0-9_@]+)\$/g, (m, name) => (holders[name.toLowerCase()] ? holders[name.toLowerCase()].content : m))
    .replace(/\$([1-9])/g, (m, d) => (subs[d - 1] === undefined ? "" : subs[d - 1]));
}

// tr is the text for a key; subs fill $1, $2 ... through the placeholders the
// message declares. A key with no message comes back as itself, which is ugly
// enough to be noticed.
function tr(key, subs) {
  const given = subs === undefined ? undefined : [].concat(subs).map(String);
  const own = spoken && spoken[key];
  const text = own && typeof own.message === "string" ? fillMessage(own, given || []) : chrome.i18n.getMessage(key, given);
  return text || key;
}

// A page marks what to fill in: data-i18n sets the text, data-i18n-title the
// tooltip and data-i18n-ph the placeholder. HTML cannot name a message the way
// the manifest does; Chrome only expands that in the manifest and in CSS.
function localizePage() {
  document.documentElement.lang = spokenName ? spokenName.replace("_", "-") : chrome.i18n.getUILanguage();
  for (const el of document.querySelectorAll("[data-i18n]")) el.textContent = tr(el.dataset.i18n);
  for (const el of document.querySelectorAll("[data-i18n-title]")) el.title = tr(el.dataset.i18nTitle);
  for (const el of document.querySelectorAll("[data-i18n-ph]")) el.placeholder = tr(el.dataset.i18nPh);
}

async function messagesOf(language) {
  const res = await fetch(chrome.runtime.getURL("_locales/" + language + "/messages.json"));
  if (!res.ok) throw new Error("HTTP " + res.status);
  return res.json();
}

// useLanguage makes the extension speak a language: LANGUAGE_DEFAULT, or one of
// LANGUAGES. Anything else is taken for the default. A file that cannot be read
// leaves the browser's own language, which is a wrong language and not a broken
// page. A page that is up is written again; of two calls that overlap the
// later one counts.
async function useLanguage(choice) {
  const turn = ++languageTurn;
  const language = LANGUAGES.includes(choice) ? choice : "";
  let messages = null;
  if (language) {
    try {
      messages = await messagesOf(language);
    } catch (e) {
      console.warn("godm: could not read the " + language + " messages", e);
    }
  }
  if (turn !== languageTurn) return;
  spoken = messages;
  spokenName = messages ? language : "";
  if (typeof document !== "undefined") localizePage();
}

// chosenLanguage is what the options page saved. A browser that will not say
// leaves the default, since nothing here is worth failing for.
async function chosenLanguage() {
  try {
    return (await chrome.storage.sync.get({ language: LANGUAGE_DEFAULT })).language;
  } catch (e) {
    return LANGUAGE_DEFAULT;
  }
}

// The page's own script comes after this one, so the elements it looks for are
// already there. The service worker has no document and skips this.
if (typeof document !== "undefined") localizePage();

// i18nReady settles once the language is known and a page written in it. Until
// then tr answers in the browser's language, so a page that is already on screen
// has words in it, and a script that says anything of its own as it starts waits
// for this first. It never fails.
let i18nReady = (async () => {
  const before = languageTurn;
  const choice = await chosenLanguage();
  // A choice made while the saved one was being read is newer than it.
  if (languageTurn === before) await useLanguage(choice);
})().catch(() => {});

// Node runs handoff.js and the tests against a stand-in for chrome.i18n; a page
// or the service worker has no module object and skips this.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { tr, useLanguage, LANGUAGES, LANGUAGE_DEFAULT, ready: () => i18nReady };
}

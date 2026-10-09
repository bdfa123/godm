"use strict";

// Not a test: a stand-in for chrome.i18n that the tests load, so that the rules
// which speak to the person can run under node. It reads the same messages.json
// Chrome would and fills in placeholders the way Chrome does. It is named like a
// test so that the release script leaves it out of the zip.

const fs = require("node:fs");
const path = require("node:path");

function messagesFor(locale) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, "_locales", locale, "messages.json"), "utf8"));
}

// expand does to one message what getMessage does: $name$ becomes what the
// placeholder stands for, and $1 to $9 in that become the substitutions.
function expand(entry, subs) {
  const holders = entry.placeholders || {};
  return entry.message
    .replace(/\$([A-Za-z0-9_@]+)\$/g, (m, name) => (holders[name.toLowerCase()] ? holders[name.toLowerCase()].content : m))
    .replace(/\$([1-9])/g, (m, d) => (subs[d - 1] === undefined ? "" : subs[d - 1]));
}

// install puts a chrome.i18n on the global object for a locale, English unless
// asked otherwise, and returns it.
function install(locale) {
  const messages = messagesFor(locale || "en");
  globalThis.chrome = {
    i18n: {
      getMessage(key, subs) {
        const entry = messages[key];
        if (!entry) return "";
        return expand(entry, subs === undefined ? [] : [].concat(subs));
      },
      getUILanguage() {
        return locale || "en";
      }
    }
  };
  return globalThis.chrome;
}

module.exports = { expand, install, messagesFor };

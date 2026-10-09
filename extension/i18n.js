"use strict";

// The words the extension says come from _locales, in the language the browser
// itself is set to. Unlike the manager there is no setting here: that is how
// Chrome picks an extension's language, and a second switch would only get out
// of step with it. English is the default_locale and so the fallback.
//
// To add a string: a key in _locales/en/messages.json and in zh_CN, then tr(key)
// in script, or a data-i18n attribute on an element. The manifest names a key
// between a pair of double underscores after MSG_. i18n.test.js checks that
// both files have the same keys and that every key the extension uses is in them.

// tr is the text for a key; subs fill $1, $2 ... through the placeholders the
// message declares. A key with no message comes back as itself, which is ugly
// enough to be noticed.
function tr(key, subs) {
  const text = chrome.i18n.getMessage(key, subs === undefined ? undefined : [].concat(subs).map(String));
  return text || key;
}

// A page marks what to fill in: data-i18n sets the text, data-i18n-title the
// tooltip and data-i18n-ph the placeholder. HTML cannot name a message the way
// the manifest does; Chrome only expands that in the manifest and in CSS.
function localizePage() {
  document.documentElement.lang = chrome.i18n.getUILanguage();
  for (const el of document.querySelectorAll("[data-i18n]")) el.textContent = tr(el.dataset.i18n);
  for (const el of document.querySelectorAll("[data-i18n-title]")) el.title = tr(el.dataset.i18nTitle);
  for (const el of document.querySelectorAll("[data-i18n-ph]")) el.placeholder = tr(el.dataset.i18nPh);
}

// The page's own script comes after this one, so the elements it looks for are
// already there. The service worker has no document and skips this.
if (typeof document !== "undefined") localizePage();

// Node runs handoff.js and the tests against a stand-in for chrome.i18n; a page
// or the service worker has no module object and skips this.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { tr };
}

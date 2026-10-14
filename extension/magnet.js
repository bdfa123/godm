"use strict";

// A content script that sends a clicked magnet link to godm. A magnet is not a
// download as far as the browser is concerned: a click on one goes straight to
// whatever program claims the scheme, or nowhere, so the downloads API never
// hears of it. The click itself is the only place to catch it.
//
// The rules for which clicks count are plain functions, so node can run them
// against a table of cases, and the service worker loads this file too, to
// check what a page sends it against the same rules. Neither has a document,
// and only a page wires the listener up.

// The two settings this needs, with the defaults the worker and the options
// page use. The name differs from the worker's DEFAULTS because the worker
// loads this file into the same scope.
const MAGNET_DEFAULTS = { enabled: true, takeTorrents: true };

// A v1 info hash as the daemon reads one: forty hex digits, or thirty-two
// characters of base32 in capitals, which is the only case its decoder takes.
const BTIH = /^urn:btih:([0-9a-fA-F]{40}|[A-Z2-7]{32})$/;

// magnetLink returns the link if it is a magnet godm can add, else "". The
// daemon refuses a magnet without exactly one v1 info hash (a v2-only link,
// say), and one it would refuse is better left to a client that may take it.
// The parameters are read decoded, as the daemon reads them, so a link that
// spells the colons as %3A counts too.
function magnetLink(href) {
  if (typeof href !== "string") return "";
  const link = href.trim();
  if (!/^magnet:/i.test(link)) return "";
  let u;
  try {
    u = new URL(link);
  } catch (e) {
    return "";
  }
  const hashes = u.searchParams.getAll("xt").filter((xt) => xt.startsWith("urn:btih:"));
  return hashes.length === 1 && BTIH.test(hashes[0]) ? link : "";
}

// magnetName is what a person would know a magnet link by: the name it carries,
// or failing that its info hash. It is what the notice of a clicked magnet
// names, so that one added by a click meant for something else is noticed.
function magnetName(link) {
  try {
    const u = new URL(link);
    const dn = (u.searchParams.get("dn") || "").trim();
    if (dn) return dn;
    const xt = u.searchParams.getAll("xt").find((v) => v.startsWith("urn:btih:"));
    return xt ? xt.slice("urn:btih:".length) : "";
  } catch (e) {
    return "";
  }
}

// linkIn finds what a click would follow: the nearest a or area with an href
// on the event's path. composedPath() runs through open shadow roots, where a
// plain walk up from the target would stop at the first one. The href property
// is the address as the browser parsed it; an SVG link has no string there, so
// it falls back to the attribute.
//
// A link in something being edited (a mail being written, a post in a forum's
// editor) is not followed: the browser only puts the caret there. Such a click
// finds no link either.
function linkIn(path) {
  for (const node of path || []) {
    if (node && node.isContentEditable === true) return "";
    const name = node && typeof node.localName === "string" ? node.localName.toLowerCase() : "";
    if ((name === "a" || name === "area") && typeof node.hasAttribute === "function" && node.hasAttribute("href")) {
      return typeof node.href === "string" ? node.href : node.getAttribute("href");
    }
  }
  return "";
}

// magnetToTake decides one click: the magnet link to hand to godm, or "" to
// leave the click to the browser. cfg is null until the settings have been
// read, and nothing is taken before then.
//
// Only a click the person made counts: a page calling a.click() on its own
// produces an untrusted event, and must never start a torrent. Only the main
// button, with no modifier: a key held down asks the browser for something
// other than following the link (a new tab, a new window, saving it), and
// holding Ctrl or Shift is how a person leaves a magnet for an installed client
// without switching anything off. A click something else has already cancelled
// was never going to open the link.
function magnetToTake(ev, cfg) {
  if (!cfg || !cfg.enabled || !cfg.takeTorrents) return "";
  if (!ev || ev.isTrusted !== true || ev.defaultPrevented) return "";
  if (ev.button !== 0 || ev.ctrlKey || ev.shiftKey || ev.altKey || ev.metaKey) return "";
  const path = typeof ev.composedPath === "function" ? ev.composedPath() : [ev.target];
  return magnetLink(linkIn(path));
}

// After the extension is reloaded or updated, the copy of this script already
// in an open page is cut off from it: chrome.runtime.id goes away and sending a
// message throws. Such a page has to behave as if godm were not there.
function extensionAlive() {
  try {
    return !!(chrome.runtime && chrome.runtime.id);
  } catch (e) {
    return false;
  }
}

function watchMagnetClicks() {
  let cfg = null;
  // A change that lands before the first read answers is kept, so the answer,
  // which may predate it, cannot undo it.
  const changed = {};
  try {
    chrome.storage.sync.get(MAGNET_DEFAULTS, (got) => {
      if (chrome.runtime.lastError || !got) return;
      cfg = Object.assign({}, MAGNET_DEFAULTS, got, changed);
    });
    chrome.storage.onChanged.addListener((changes, areaName) => {
      if (areaName !== "sync") return;
      for (const key of Object.keys(MAGNET_DEFAULTS)) {
        if (!changes[key]) continue;
        // A setting removed from storage is back at its default.
        const value = "newValue" in changes[key] ? changes[key].newValue : MAGNET_DEFAULTS[key];
        changed[key] = value;
        if (cfg) cfg[key] = value;
      }
    });
  } catch (e) {
    return; // already cut off from the extension
  }

  const onClick = (ev) => {
    if (!extensionAlive()) return;
    const url = magnetToTake(ev, cfg);
    if (!url) return;
    let sent;
    try {
      sent = chrome.runtime.sendMessage({ type: "godm-magnet", url: url });
    } catch (e) {
      return; // cut off after all: the browser keeps the click
    }
    ev.preventDefault();
    ev.stopImmediatePropagation();
    // The worker answers every magnet, handing it back to the browser itself
    // when godm cannot take it. A failure here means it never got the link,
    // so the page opens it as the click would have.
    if (sent && typeof sent.catch === "function") {
      sent.catch(() => {
        try {
          window.location.assign(url);
        } catch (e) {
          // A sandboxed frame may not navigate; nothing more can be done.
        }
      });
    }
  };
  // Capturing on the window runs before anything the page has registered, so
  // a page handler cannot open the link first, and stopping the event here
  // keeps the page from acting on a click that has been dealt with.
  const listen = () => window.addEventListener("click", onClick, true);
  listen();

  // A page that calls document.open(), or document.write() once it has loaded,
  // wipes every listener on the window, this one included, and empties the
  // document as it does. Watching the document's own children sees that
  // happen. Adding the same listener again while it is still there does
  // nothing, so it is simply added again whenever they change.
  if (typeof MutationObserver === "function") {
    try {
      new MutationObserver(listen).observe(document, { childList: true });
    } catch (e) {
      // No document to watch; the listener above is all there is.
    }
  }
}

if (typeof document !== "undefined" && typeof window !== "undefined") watchMagnetClicks();

// Node runs this file directly to test the rules; a page or the service worker
// has no module object and skips it.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { MAGNET_DEFAULTS, extensionAlive, linkIn, magnetLink, magnetName, magnetToTake };
}

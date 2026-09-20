"use strict";

// The rules for spotting video live in their own file so they can be tested
// without a browser.
importScripts("sniff.js");

const HOST_NAME = "com.godm.host";

// How long a failed host check keeps takeover switched off, and how long a URL
// we handed back to the browser stays immune from re-interception.
const HOST_RECHECK_MS = 60000;
const HANDBACK_TTL_MS = 120000;
const NOTIFY_COOLDOWN_MS = 60000;

const DEFAULTS = {
  enabled: true,
  takeAll: false,
  minSize: 1048576, // 1 MiB, only applied when the browser already knows the size
  connections: 8,
  extensions:
    "7z,apk,appimage,avi,bin,bz2,deb,dmg,exe,flac,flv,gz,img,iso,jar,mkv,mov,mp3,mp4,msi,ova,pdf,pkg,rar,rpm,tar,vhd,wav,webm,whl,xz,zip,zst",
  blocklist: ""
};

async function getConfig() {
  const stored = await chrome.storage.sync.get(DEFAULTS);
  return Object.assign({}, DEFAULTS, stored);
}

// ---------- native host ----------

function callHost(message) {
  return new Promise((resolve) => {
    let settled = false;
    const done = (v) => {
      if (settled) return;
      settled = true;
      resolve(v);
    };
    try {
      chrome.runtime.sendNativeMessage(HOST_NAME, message, (resp) => {
        if (chrome.runtime.lastError) {
          done({ ok: false, error: chrome.runtime.lastError.message });
        } else if (!resp) {
          done({ ok: false, error: "native host returned nothing" });
        } else {
          done(resp);
        }
      });
    } catch (e) {
      done({ ok: false, error: String(e) });
    }
    // The host is a thin client to the daemon and answers immediately; if it
    // does not, something is wrong and we want to fail over, not hang.
    setTimeout(() => done({ ok: false, error: "native host timed out" }), 15000);
  });
}

// ---------- host health ----------
//
// Session storage rather than a module variable: the MV3 service worker is torn
// down after ~30s idle, and losing this state between a cancel and the retry is
// exactly what turned one failure into a notification storm.

async function getHostState() {
  const { hostState } = await chrome.storage.session.get({
    hostState: { ok: true, at: 0, error: "" }
  });
  return hostState;
}

async function setHostState(ok, error) {
  const st = { ok: !!ok, at: Date.now(), error: error || "" };
  await chrome.storage.session.set({ hostState: st });
  await chrome.storage.local.set({
    lastPing: { ok: st.ok, error: st.error, at: new Date().toISOString() }
  });
  // Painting the badge here directly used to wipe the count of videos found on
  // the page, because the popup asks for the task list every second and every
  // answer came through this function.
  paintBadge();
  chrome.action.setTitle({
    title: ok ? "godm" : "godm - native host unreachable, downloads left to the browser"
  });
  return st;
}

// takeoverDisabled reports whether a recent failure means we should keep our
// hands off. Cancelling a download we cannot actually take over is strictly
// worse than doing nothing.
async function takeoverDisabled() {
  const st = await getHostState();
  if (st.ok) return false;
  return Date.now() - st.at < HOST_RECHECK_MS;
}

// ---------- hand-back bookkeeping ----------

async function markHandedBack(url) {
  const { handedBack } = await chrome.storage.session.get({ handedBack: {} });
  const now = Date.now();
  for (const [k, t] of Object.entries(handedBack)) {
    if (now - t > HANDBACK_TTL_MS) delete handedBack[k];
  }
  handedBack[url] = now;
  await chrome.storage.session.set({ handedBack });
}

async function wasHandedBack(url) {
  const { handedBack } = await chrome.storage.session.get({ handedBack: {} });
  const t = handedBack[url];
  return !!t && Date.now() - t < HANDBACK_TTL_MS;
}

// ---------- takeover decision ----------

function extensionOf(name) {
  if (!name) return "";
  const clean = name.split(/[?#]/)[0];
  const dot = clean.lastIndexOf(".");
  if (dot < 0 || dot === clean.length - 1) return "";
  return clean.slice(dot + 1).toLowerCase();
}

function shouldTakeOver(item, cfg) {
  if (item.byExtensionId && item.byExtensionId === chrome.runtime.id) return "own fallback";

  const url = item.finalUrl || item.url || "";
  if (!/^https?:\/\//i.test(url)) return "not http(s)";
  if (item.state === "complete") return "already complete";

  let host = "";
  try {
    host = new URL(url).hostname.toLowerCase();
  } catch (e) {
    return "unparseable url";
  }

  const blocked = cfg.blocklist
    .split(/[\s,]+/)
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);
  if (blocked.some((b) => host === b || host.endsWith("." + b))) return "host blocked";

  // totalBytes is often 0 at creation time; only filter when it is real.
  if (item.totalBytes > 0 && item.totalBytes < cfg.minSize) return "below size floor";

  if (cfg.takeAll) return null;

  const allowed = new Set(
    cfg.extensions.split(/[\s,]+/).map((s) => s.trim().toLowerCase().replace(/^\./, "")).filter(Boolean)
  );
  const ext =
    extensionOf(item.filename) ||
    extensionOf(pathOf(item.url)) ||
    extensionOf(pathOf(url));
  if (!allowed.has(ext)) return "extension not in list (" + (ext || "none") + ")";

  return null; // take it
}

async function cookieHeader(url) {
  try {
    const cookies = await chrome.cookies.getAll({ url });
    // httpOnly cookies come through here too; page JS could never read them,
    // and without them any authenticated download comes back as a login page.
    return cookies.map((c) => c.name + "=" + c.value).join("; ");
  } catch (e) {
    return "";
  }
}

// ---------- notifications ----------

async function notifyThrottled(key, title, message) {
  const stamps = (await chrome.storage.session.get({ notifyAt: {} })).notifyAt;
  const now = Date.now();
  if (stamps[key] && now - stamps[key] < NOTIFY_COOLDOWN_MS) return;
  stamps[key] = now;
  await chrome.storage.session.set({ notifyAt: stamps });
  chrome.notifications.create({
    type: "basic",
    iconUrl: "icons/128.png",
    title: title,
    message: message
  });
}

// ---------- main interception ----------

let activeCount = 0;

function bumpBadge(delta) {
  activeCount = Math.max(0, activeCount + delta);
  paintBadge();
  if (activeCount) setTimeout(() => bumpBadge(-1), 4000);
}

// The one place the badge is painted. Three things want to say something on
// it, so they are ranked here rather than each writing over the others: a dead
// native host is a problem, a download just handed over is news, and otherwise
// it says how many videos were spotted on the tab in front.
async function paintBadge() {
  const host = await getHostState();
  if (!host.ok) {
    chrome.action.setBadgeBackgroundColor({ color: "#d64545" });
    chrome.action.setBadgeText({ text: "!" });
    return;
  }
  if (activeCount > 0) {
    chrome.action.setBadgeBackgroundColor({ color: "#2f6df6" });
    chrome.action.setBadgeText({ text: String(activeCount) });
    return;
  }
  let found = 0;
  try {
    const [tab] = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
    if (tab) found = (await mediaFor(tab.id)).length;
  } catch (e) {
    // No window in focus, or the tab went away between the two calls.
  }
  chrome.action.setBadgeBackgroundColor({ color: "#1f9d55" });
  chrome.action.setBadgeText({ text: found ? String(found) : "" });
}

// The decision is made in onDeterminingFilename rather than onCreated. When
// onCreated fires the response headers have not arrived, so item.filename is
// empty, and for anything behind a redirect to a signed CDN URL (GitHub
// releases, S3, most file hosts) the URL path carries no extension either.
// The real name only exists in Content-Disposition, which Chrome has parsed by
// the time it asks listeners to confirm the filename.
chrome.downloads.onDeterminingFilename.addListener((item, suggest) => {
  // Answer straight away without overriding anything. Chrome holds the
  // download until every listener has suggested, and we must not stall it
  // while we talk to the native host.
  suggest();
  handleDownload(item);
});

async function handleDownload(item) {
  const cfg = await getConfig();
  if (!cfg.enabled) return;

  const url = item.finalUrl || item.url;

  // Two gates before we touch anything. Both exist because a cancelled
  // download we cannot take over is a download the user has lost.
  if (await takeoverDisabled()) {
    console.debug("godm: host is down, leaving this to the browser -", url);
    return;
  }
  if (url && (await wasHandedBack(url))) {
    console.debug("godm: already handed this back, not touching it again -", url);
    return;
  }

  const skip = shouldTakeOver(item, cfg);
  if (skip) {
    console.debug("godm: leaving to browser -", skip, url);
    return;
  }

  // Confirm the host is actually reachable *before* cancelling. One extra
  // round trip is cheap next to losing the user's download.
  const health = await callHost({ type: "ping" });
  if (!health.ok) {
    await setHostState(false, health.error);
    await notifyThrottled(
      "host-down",
      "godm is not connected",
      (health.error || "native host unreachable") +
        "\nDownloads are being left to Chrome until this is fixed."
    );
    console.error("godm: host unreachable, not intercepting -", health.error);
    return; // the browser download continues untouched
  }
  await setHostState(true, "");

  try {
    await chrome.downloads.cancel(item.id);
    await chrome.downloads.erase({ id: item.id });
  } catch (e) {
    console.warn("godm: could not cancel browser download", e);
  }

  // Hand over the URL the user actually clicked, not the redirect target.
  // Signed CDN links expire within minutes; godm follows redirects itself, so
  // starting from the original gets a fresh signature every time it resumes.
  const jobUrl = /^https?:\/\//i.test(item.url || "") ? item.url : url;
  const resp = await callHost({
    type: "download",
    url: jobUrl,
    filename: item.filename ? item.filename.split(/[\\/]/).pop() : "",
    referrer: item.referrer || "",
    cookie: await cookieHeader(jobUrl),
    userAgent: navigator.userAgent,
    connections: cfg.connections
  });

  if (resp.ok) {
    bumpBadge(1);
    return;
  }

  // The host answered the ping but failed the job. Hand the download back and
  // remember the URL so the replacement is not intercepted in turn.
  await setHostState(false, resp.error);
  await markHandedBack(url);
  console.error("godm: handoff failed -", resp.error);
  await notifyThrottled(
    "handoff-failed",
    "godm could not take over",
    (resp.error || "unknown error") + "\nHanded the download back to Chrome."
  );
  try {
    await chrome.downloads.download({ url: url });
  } catch (e) {
    await notifyThrottled(
      "lost",
      "Download lost",
      "godm failed and Chrome refused the retry.\n" + url
    );
  }
}

// ---------- right-click entry point ----------

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "godm-link",
      title: "Download with godm",
      contexts: ["link", "video", "audio", "image"]
    });
    chrome.contextMenus.create({
      id: "godm-page-links",
      title: "Download links on this page with godm…",
      contexts: ["page"]
    });
    chrome.contextMenus.create({
      id: "godm-selection-links",
      title: "Download selected links with godm…",
      contexts: ["selection"]
    });
  });
  selfTest();
});

chrome.runtime.onStartup.addListener(selfTest);

// selfTest pings the host as soon as the worker wakes, so a broken
// registration shows up on the options page rather than the first time a real
// download needs it.
async function selfTest() {
  const resp = await callHost({ type: "ping" });
  await setHostState(!!resp.ok, resp.error);
  if (resp.ok) console.log("godm: native host reachable");
  else console.error("godm: native host unreachable -", resp.error);
}

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === "godm-page-links" || info.menuItemId === "godm-selection-links") {
    await pickLinksFromTab(tab, info.menuItemId === "godm-selection-links");
    return;
  }
  if (info.menuItemId !== "godm-link") return;
  const url = info.linkUrl || info.srcUrl;
  if (!url) return;
  const cfg = await getConfig();
  const resp = await callHost({
    type: "download",
    url: url,
    referrer: info.pageUrl || (tab && tab.url) || "",
    cookie: await cookieHeader(url),
    userAgent: navigator.userAgent,
    connections: cfg.connections
  });
  if (resp.ok) {
    await setHostState(true, "");
    bumpBadge(1);
  } else {
    await setHostState(false, resp.error);
    // An explicit right-click deserves an immediate answer, not a throttled one.
    chrome.notifications.create({
      type: "basic",
      iconUrl: "icons/128.png",
      title: "godm error",
      message: resp.error || "unknown error"
    });
  }
});

// ---------- download list: pick links from a page ----------

// collectLinksInPage runs inside the page, so it must be self-contained. It
// gathers anchors and media sources, resolved and de-duplicated, optionally
// only those inside the user's selection.
function collectLinksInPage(selectionOnly) {
  const sel = window.getSelection();
  const useSelection = selectionOnly && sel && sel.rangeCount > 0 && !sel.isCollapsed;
  const inSelection = (el) => {
    if (!useSelection) return true;
    for (let i = 0; i < sel.rangeCount; i++) {
      if (sel.getRangeAt(i).intersectsNode(el)) return true;
    }
    return false;
  };
  const seen = new Set();
  const links = [];
  const add = (href, text, kind) => {
    let u;
    try {
      u = new URL(href, location.href);
    } catch (e) {
      return;
    }
    if (u.protocol !== "http:" && u.protocol !== "https:") return;
    u.hash = "";
    if (seen.has(u.href)) return;
    seen.add(u.href);
    links.push({ url: u.href, text: (text || "").replace(/\s+/g, " ").trim().slice(0, 160), kind: kind });
  };
  document.querySelectorAll("a[href]").forEach((a) => {
    if (inSelection(a)) add(a.href, a.textContent || a.title || a.getAttribute("download") || "", "link");
  });
  document.querySelectorAll("video[src], audio[src], video source[src], audio source[src]").forEach((m) => {
    if (inSelection(m)) add(m.src, m.title || "", "media");
  });
  return { page: location.href, title: document.title, links: links };
}

async function pickLinksFromTab(tab, selectionOnly) {
  if (!tab || tab.id === undefined) return { ok: false, error: "no active tab" };
  let result;
  try {
    const [res] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: collectLinksInPage,
      args: [!!selectionOnly]
    });
    result = res && res.result;
  } catch (e) {
    // chrome:// pages, the Web Store and PDFs refuse script injection.
    const msg = "This page does not allow reading its links.";
    chrome.notifications.create({ type: "basic", iconUrl: "icons/128.png", title: "godm", message: msg });
    return { ok: false, error: msg };
  }
  if (!result || !result.links.length) {
    const msg = selectionOnly ? "No links in the selection." : "No links found on this page.";
    chrome.notifications.create({ type: "basic", iconUrl: "icons/128.png", title: "godm", message: msg });
    return { ok: false, error: msg };
  }
  // The picker is a separate extension page; hand it the list through session
  // storage rather than a URL, which would leak every link into history.
  const key = "pick-" + Date.now() + "-" + Math.random().toString(36).slice(2, 8);
  await chrome.storage.session.set({ [key]: result });
  await chrome.windows.create({
    url: chrome.runtime.getURL("picker.html?k=" + encodeURIComponent(key)),
    type: "popup",
    width: 920,
    height: 700
  });
  return { ok: true, count: result.links.length };
}

// submitPicked sends the chosen links as one batch. Cookies are looked up per
// link because a single page often links to several hosts.
async function submitPicked(msg) {
  const cfg = await getConfig();
  const items = [];
  for (const it of msg.items || []) {
    if (!/^https?:\/\//i.test(it.url || "")) continue;
    items.push({ url: it.url, filename: it.filename || "", cookie: await cookieHeader(it.url) });
  }
  if (!items.length) return { ok: false, error: "nothing selected" };
  const resp = await callHost({
    type: "batch",
    items: items,
    referrer: msg.referrer || "",
    userAgent: navigator.userAgent,
    connections: msg.connections || cfg.connections
  });
  await setHostState(!!resp.ok, resp.error);
  if (resp.ok) bumpBadge(1);
  return resp;
}

// ---------- popup / options / picker bridge ----------

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg && msg.type === "godm-grab-active") {
    chrome.tabs.query({ active: true, lastFocusedWindow: true })
      .then((tabs) => pickLinksFromTab(tabs[0], false))
      .then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-batch") {
    submitPicked(msg).then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-media") {
    listMedia().then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-media-inspect") {
    inspectStream(msg).then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-media-download") {
    submitStream(msg).then(sendResponse);
    return true;
  }
  callHost(msg).then(async (resp) => {
    if (msg && (msg.type === "ping" || msg.type === "tasks")) {
      await setHostState(!!resp.ok, resp.error);
    }
    sendResponse(resp);
  });
  return true; // keep the channel open for the async reply
});

// ---------- media sniffing ----------
//
// A page that plays video always fetches either a playlist (HLS .m3u8, DASH
// .mpd) or one long media response. Watching responses go by is enough to find
// it: nothing is blocked, nothing is injected into the page, and a page that
// plays nothing costs a regex per response.

const MEDIA_TTL_MS = 30 * 60 * 1000;
const MEDIA_MAX_PER_TAB = 40;
async function mediaFor(tabId) {
  const key = "media:" + tabId;
  const got = await chrome.storage.session.get(key);
  const now = Date.now();
  return (got[key] || []).filter((m) => now - m.at < MEDIA_TTL_MS);
}

// Responses arrive from several connections at once, so read-modify-write on
// the stored list has to be serialised or entries go missing.
let mediaQueue = Promise.resolve();

function withMedia(tabId, fn) {
  mediaQueue = mediaQueue
    .then(async () => {
      const list = await mediaFor(tabId);
      const next = await fn(list);
      if (next) {
        await chrome.storage.session.set({
          ["media:" + tabId]: next.slice(-MEDIA_MAX_PER_TAB)
        });
      }
    })
    .catch((err) => console.warn("godm: media store", err));
  return mediaQueue;
}

async function noteMedia(d) {
  if (d.tabId < 0 || !/^https?:/i.test(d.url || "")) return;
  const h = headerMap(d.responseHeaders);
  const contentType = (h["content-type"] || "").split(";")[0].trim().toLowerCase();
  const length = parseInt(h["content-length"] || "0", 10) || 0;
  const kind = classifyMedia(d.url, contentType, length);
  if (!kind) return;

  let added = false;
  await withMedia(d.tabId, async (list) => {
    let title = "";
    try {
      title = (await chrome.tabs.get(d.tabId)).title || "";
    } catch (e) {
      // The tab closed while we were looking at its traffic.
    }
    const item = mediaEntry(d.url, kind, contentType, length, title, Date.now());
    if (!shouldRecord(list, item)) return null;
    list.push(item);
    added = true;
    return list;
  });
  if (added) paintBadge();
}

chrome.webRequest.onHeadersReceived.addListener(
  (d) => { noteMedia(d); },
  { urls: ["http://*/*", "https://*/*"] },
  ["responseHeaders"]
);

// What was found belongs to the page that was open. A new page, including the
// in-page navigations a video site does without reloading, starts empty.
chrome.tabs.onUpdated.addListener((tabId, info) => {
  if (info.status === "loading" && info.url) {
    chrome.storage.session.remove("media:" + tabId);
    paintBadge();
  }
});
chrome.tabs.onRemoved.addListener((tabId) => {
  chrome.storage.session.remove("media:" + tabId);
});
chrome.tabs.onActivated.addListener(() => paintBadge());

async function listMedia() {
  const tabs = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
  if (!tabs[0]) return { ok: true, items: [] };
  return {
    ok: true,
    items: await mediaFor(tabs[0].id),
    title: tabs[0].title || "",
    page: tabs[0].url || ""
  };
}

async function inspectStream(msg) {
  const resp = await callHost({
    type: "inspect",
    url: msg.url,
    cookie: await cookieHeader(msg.url),
    referrer: msg.page || "",
    userAgent: navigator.userAgent
  });
  await setHostState(!!resp.ok, resp.error);
  return resp;
}

async function submitStream(msg) {
  const cfg = await getConfig();
  const resp = await callHost({
    type: "download",
    url: msg.url,
    kind: msg.kind === "hls" ? "hls" : "",
    variant: typeof msg.variant === "number" ? msg.variant : -1,
    filename: msg.filename || "",
    cookie: await cookieHeader(msg.url),
    referrer: msg.page || "",
    userAgent: navigator.userAgent,
    connections: msg.connections || cfg.connections
  });
  await setHostState(!!resp.ok, resp.error);
  if (resp.ok) bumpBadge(1);
  return resp;
}

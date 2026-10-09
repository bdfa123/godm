"use strict";

// The rules for spotting video, and for checking what the download dialog
// sends back, live in their own files so they can be tested without a browser.
// i18n.js comes first because handoff.js speaks through it.
importScripts("sniff.js", "i18n.js", "handoff.js");

const HOST_NAME = "com.godm.host";

// How long a failed host check keeps takeover switched off, and how long a URL
// we handed back to the browser stays immune from re-interception.
const HOST_RECHECK_MS = 60000;
const HANDBACK_TTL_MS = 120000;
const NOTIFY_COOLDOWN_MS = 60000;

// A confirmation dialog that has been sitting this long was forgotten about.
const CONFIRM_TTL_MS = 24 * 60 * 60 * 1000;
const CONFIRM_WIDTH = 540;
const CONFIRM_HEIGHT = 370;
// The folder chooser waits for a person; the daemon itself gives up after three
// minutes, so this only has to outlast that.
const BROWSE_TIMEOUT_MS = 4 * 60 * 1000 + 10000;

const DEFAULTS = {
  enabled: true,
  // Ask for a name, folder and connection count before each takeover.
  confirmDownload: true,
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

function callHost(message, timeoutMs) {
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
          done({ ok: false, error: tr("err_host_nothing") });
        } else {
          done(resp);
        }
      });
    } catch (e) {
      done({ ok: false, error: String(e) });
    }
    // The host is a thin client to the daemon and answers immediately; if it
    // does not, something is wrong and we want to fail over, not hang.
    setTimeout(() => done({ ok: false, error: tr("err_host_timeout") }), timeoutMs || 15000);
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
    title: ok ? "godm" : tr("action_title_down")
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
      tr("notify_host_down_title"),
      tr("notify_host_down_body", health.error || tr("err_host_unreachable"))
    );
    console.error("godm: host unreachable, not intercepting -", health.error);
    return; // the browser download continues untouched
  }
  await setHostState(true, "");

  try {
    await chrome.downloads.cancel(item.id);
  } catch (e) {
    console.warn("godm: could not cancel browser download", e);
    return; // Chrome still owns it; submitting too would download it twice.
  }
  try {
    await chrome.downloads.erase({ id: item.id });
  } catch (e) {
    // Erasing only removes the history row. The transfer is already stopped,
    // so a history cleanup failure must not lose the handoff.
    console.warn("godm: could not erase cancelled browser download", e);
  }

  // Hand over the URL the user actually clicked, not the redirect target.
  // Signed CDN links expire within minutes; godm follows redirects itself, so
  // starting from the original gets a fresh signature every time it resumes.
  const jobUrl = /^https?:\/\//i.test(item.url || "") ? item.url : url;
  const held = {
    url: jobUrl,
    // What Chrome gets back if godm cannot take the job after all.
    backUrl: url,
    referrer: item.referrer || "",
    filename: suggestName(item),
    size: pickSize(item),
    connections: cfg.connections
  };

  if (cfg.confirmDownload) {
    // The dialog owns the download from here: Start hands it to godm, Cancel or
    // closing the window drops it, which is what the person asked for by
    // choosing to be asked.
    let asked = false;
    try {
      asked = await openConfirm(held);
    } catch (e) {
      console.warn("godm: confirmation dialog failed", e);
    }
    if (asked) return;
    // No window to ask in. The browser's copy is already cancelled, so doing
    // nothing would lose the file; carry on as if the dialog were switched off.
    console.warn("godm: could not open the confirmation dialog, handing over directly");
  }
  await handOff(held, {
    type: "download",
    url: held.url,
    filename: item.filename ? item.filename.split(/[\\/]/).pop() : "",
    referrer: held.referrer,
    connections: held.connections,
    outDir: ""
  });
}

// handOff sends one job to godm and, if that fails, gives the download back to
// the browser. Both the direct path and the dialog's Start button end here, so
// a refusal is handled the same way whichever way the download arrived.
// Resolves {ok:true}, or {ok:false, handedBack:true, error}.
async function handOff(held, job) {
  const resp = await callHost(
    Object.assign({}, job, {
      cookie: await cookieHeader(job.url),
      userAgent: navigator.userAgent
    })
  );

  if (resp.ok) {
    bumpBadge(1);
    return { ok: true };
  }

  // The host answered the ping but failed the job. Hand the download back and
  // remember the URL so the replacement is not intercepted in turn.
  const url = held.backUrl;
  await setHostState(false, resp.error);
  await markHandedBack(url);
  console.error("godm: handoff failed -", resp.error);
  await notifyThrottled(
    "handoff-failed",
    tr("notify_handoff_title"),
    tr("notify_handoff_body", resp.error || tr("err_unknown"))
  );
  try {
    await chrome.downloads.download({ url: url });
  } catch (e) {
    await notifyThrottled("lost", tr("notify_lost_title"), tr("notify_lost_body", url));
  }
  return { ok: false, handedBack: true, error: resp.error || tr("err_unknown") };
}

// ---------- download confirmation dialog ----------
//
// Each dialog is its own extension window, told apart by a key in the URL. What
// the worker needs to finish the job lives in session storage under that key,
// not in a variable: the worker is torn down when idle, a person can sit in a
// dialog far longer than that, and several downloads can be waiting at once.
//
//   confirm:<id>        the held download, written before the window opens
//   confirmwin:<winId>  which key a window belongs to, so closing the window
//                       (Cancel, the X, Escape) can throw the held one away

// Start can arrive twice (a double click, a retry after a lost reply) while the
// first is still talking to the host. Only the first may send the job.
const startingNow = new Set();

// Downloads that arrive together each ask to open a dialog at once. Taken in
// turn, every one sees the dialogs already waiting and fans out from them;
// taken together, each would count none and they would stack exactly.
let confirmQueue = Promise.resolve();

function openConfirm(held) {
  const run = confirmQueue.then(() => openConfirmNow(held));
  confirmQueue = run.catch(() => {}); // one failure must not block the next
  return run;
}

async function openConfirmNow(held) {
  const now = Date.now();
  let waiting = 0;
  const stale = [];
  for (const [k, v] of Object.entries(await chrome.storage.session.get(null))) {
    if (!k.startsWith("confirm:")) continue;
    if (now - (v.at || 0) > CONFIRM_TTL_MS) stale.push(k);
    else waiting++;
  }
  if (stale.length) await chrome.storage.session.remove(stale);

  const key = "confirm:" + now + "-" + Math.random().toString(36).slice(2, 8);
  await chrome.storage.session.set({ [key]: Object.assign({ at: now }, held) });

  let parent = null;
  try {
    const wins = await chrome.windows.getAll();
    const normal = wins.filter((w) => w.type === "normal");
    parent = normal.find((w) => w.focused) || normal[0] || null;
  } catch (e) {
    // No parent to centre over; the browser will place the dialog.
  }
  const bounds = dialogBounds(parent, waiting, CONFIRM_WIDTH, CONFIRM_HEIGHT);
  const page = chrome.runtime.getURL("confirm.html?k=" + encodeURIComponent(key));

  let win = null;
  try {
    win = await chrome.windows.create(Object.assign({ url: page, type: "popup", focused: true }, bounds));
  } catch (e) {
    // A position the browser considers off-screen is refused outright. The
    // size alone is always acceptable.
    try {
      win = await chrome.windows.create({
        url: page,
        type: "popup",
        focused: true,
        width: CONFIRM_WIDTH,
        height: CONFIRM_HEIGHT
      });
    } catch (e2) {
      console.warn("godm: cannot open a window", e2);
    }
  }
  if (!win) {
    await chrome.storage.session.remove(key);
    return false;
  }
  // The window is up, so the dialog now owns the download whatever happens
  // here. Failing to note which window it is only means closing it cannot tidy
  // up after itself; the stale-entry sweep above does that eventually.
  try {
    await chrome.storage.session.set({ ["confirmwin:" + win.id]: key });
  } catch (e) {
    console.warn("godm: could not record the dialog window", e);
  }
  return true;
}

// A dialog window going away without Start means "do not download this".
chrome.windows.onRemoved.addListener(async (windowId) => {
  const wk = "confirmwin:" + windowId;
  const got = await chrome.storage.session.get(wk);
  if (!got[wk]) return;
  await chrome.storage.session.remove([wk, got[wk]]);
});

// confirmStart runs when the dialog's Start button is pressed. The page only
// reports what is in its form; the checks are repeated here, and the held
// download is only given up once godm has answered, so a worker that dies
// halfway leaves the dialog able to try again.
async function confirmStart(msg) {
  const key = msg && msg.key;
  if (typeof key !== "string" || !key.startsWith("confirm:")) {
    return { ok: false, error: tr("err_unknown_request") };
  }
  if (startingNow.has(key)) return { ok: false, error: tr("err_already_starting") };
  startingNow.add(key);
  try {
    const held = (await chrome.storage.session.get(key))[key];
    if (!held) {
      return { ok: false, expired: true, error: tr("err_request_expired") };
    }
    const built = buildJob(held, msg);
    if (built.error) return { ok: false, error: built.error };

    if (msg.dontAsk) {
      // A settings hiccup is no reason to lose the download.
      try {
        await chrome.storage.sync.set({ confirmDownload: false });
      } catch (e) {
        console.warn("godm: could not save the do-not-ask setting", e);
      }
    }

    const result = await handOff(held, built.job);
    await chrome.storage.session.remove(key);
    return result;
  } finally {
    startingNow.delete(key);
  }
}

async function browseFolder(current) {
  return callHost(
    { type: "browse", current: typeof current === "string" ? current : "" },
    BROWSE_TIMEOUT_MS
  );
}

// ---------- right-click entry point ----------

// The titles are stored with the menu, in the browser's language at the time.
// Making them again at every start keeps them right after the browser's
// language is changed, which no install or update would announce.
function buildMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "godm-link",
      title: tr("menu_link"),
      contexts: ["link", "video", "audio", "image"]
    });
    chrome.contextMenus.create({
      id: "godm-page-links",
      title: tr("menu_page_links"),
      contexts: ["page"]
    });
    chrome.contextMenus.create({
      id: "godm-selection-links",
      title: tr("menu_selection_links"),
      contexts: ["selection"]
    });
  });
}

chrome.runtime.onInstalled.addListener(() => {
  buildMenus();
  selfTest();
});

chrome.runtime.onStartup.addListener(() => {
  buildMenus();
  selfTest();
});

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
  // The item shows on every link, magnet links included: a menu pattern
  // cannot name the magnet: scheme. A magnet is not fetched over HTTP, so it
  // carries no cookies.
  const magnet = /^magnet:/i.test(url);
  const resp = await callHost({
    type: "download",
    url: url,
    kind: magnet ? "bt" : "",
    referrer: info.pageUrl || (tab && tab.url) || "",
    cookie: magnet ? "" : await cookieHeader(url),
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
      title: tr("notify_error_title"),
      message: resp.error || tr("err_unknown")
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
  if (!tab || tab.id === undefined) return { ok: false, error: tr("err_no_active_tab") };
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
    const msg = tr("notify_page_blocked");
    chrome.notifications.create({ type: "basic", iconUrl: "icons/128.png", title: "godm", message: msg });
    return { ok: false, error: msg };
  }
  if (!result || !result.links.length) {
    const msg = tr(selectionOnly ? "notify_no_selection" : "notify_no_links");
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
  if (!items.length) return { ok: false, error: tr("err_nothing_selected") };
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
  if (msg && msg.type === "godm-confirm-start") {
    confirmStart(msg)
      .catch((e) => ({ ok: false, error: String(e) }))
      .then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-browse") {
    browseFolder(msg.current).then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-media") {
    listMedia().then(sendResponse);
    return true;
  }
  if (msg && msg.type === "godm-config") {
    daemonConfig().then(sendResponse);
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

async function daemonConfig() {
  const resp = await callHost({ type: "config" });
  await setHostState(!!resp.ok, resp.error);
  return resp;
}

async function inspectStream(msg) {
  const resp = await callHost({
    type: "inspect",
    url: msg.url,
    kind: msg.kind || "",
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
    kind: msg.kind === "hls" || msg.kind === "yt-dlp" ? msg.kind : "",
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

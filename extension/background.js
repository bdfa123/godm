"use strict";

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
  chrome.action.setBadgeBackgroundColor({ color: ok ? "#2f6df6" : "#d64545" });
  chrome.action.setBadgeText({ text: ok ? "" : "!" });
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
    extensionOf(decodeURIComponent(new URL(url).pathname));
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
  chrome.action.setBadgeBackgroundColor({ color: "#2f6df6" });
  chrome.action.setBadgeText({ text: activeCount ? String(activeCount) : "" });
  if (activeCount) setTimeout(() => bumpBadge(-1), 4000);
}

chrome.downloads.onCreated.addListener(async (item) => {
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

  const resp = await callHost({
    type: "download",
    url: url,
    filename: item.filename ? item.filename.split(/[\\/]/).pop() : "",
    referrer: item.referrer || "",
    cookie: await cookieHeader(url),
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
});

// ---------- right-click entry point ----------

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "godm-link",
      title: "Download with godm",
      contexts: ["link", "video", "audio", "image"]
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

// ---------- popup / options bridge ----------

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  callHost(msg).then(async (resp) => {
    if (msg && (msg.type === "ping" || msg.type === "tasks")) {
      await setHostState(!!resp.ok, resp.error);
    }
    sendResponse(resp);
  });
  return true; // keep the channel open for the async reply
});

"use strict";

const HOST_NAME = "com.godm.host";

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
    try {
      chrome.runtime.sendNativeMessage(HOST_NAME, message, (resp) => {
        if (settled) return;
        settled = true;
        if (chrome.runtime.lastError) {
          resolve({ ok: false, error: chrome.runtime.lastError.message });
        } else if (!resp) {
          resolve({ ok: false, error: "native host returned nothing" });
        } else {
          resolve(resp);
        }
      });
    } catch (e) {
      resolve({ ok: false, error: String(e) });
    }
    // The host is a thin client to the daemon and answers immediately; if it
    // does not, something is wrong and we want to fail over, not hang.
    setTimeout(() => {
      if (!settled) {
        settled = true;
        resolve({ ok: false, error: "native host timed out" });
      }
    }, 15000);
  });
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
  // Never grab a download we ourselves handed back to the browser.
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

// ---------- main interception ----------

let activeCount = 0;

function bumpBadge(delta) {
  activeCount = Math.max(0, activeCount + delta);
  chrome.action.setBadgeBackgroundColor({ color: "#2f6df6" });
  chrome.action.setBadgeText({ text: activeCount ? String(activeCount) : "" });
  if (activeCount) setTimeout(() => bumpBadge(-1), 4000);
}

function notify(title, message) {
  chrome.notifications.create({
    type: "basic",
    iconUrl: "icons/128.png",
    title: title,
    message: message
  });
}

chrome.downloads.onCreated.addListener(async (item) => {
  const cfg = await getConfig();
  if (!cfg.enabled) return;

  const skip = shouldTakeOver(item, cfg);
  if (skip) {
    console.debug("godm: leaving to browser -", skip, item.finalUrl || item.url);
    return;
  }

  const url = item.finalUrl || item.url;

  // Cancel first so the browser stops pulling bytes we are about to refetch.
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

  // Handing it back is the only honest failure mode: we already cancelled the
  // browser download, so silently dropping it would lose the user's file.
  console.error("godm: handoff failed -", resp.error);
  notify("godm could not take over", (resp.error || "unknown error") + " - falling back to the browser.");
  try {
    await chrome.downloads.download({ url: url });
  } catch (e) {
    notify("Download lost", "godm failed and the browser refused the retry. URL: " + url);
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

// selfTest pings the native host as soon as the worker wakes, so a broken
// registration shows up immediately instead of the first time a download is
// cancelled and lost. The result is kept in storage for the options page.
async function selfTest() {
  const resp = await callHost({ type: "ping" });
  const record = {
    ok: !!resp.ok,
    error: resp.error || "",
    at: new Date().toISOString()
  };
  await chrome.storage.local.set({ lastPing: record });
  if (resp.ok) {
    console.log("godm: native host reachable");
  } else {
    console.error("godm: native host unreachable -", resp.error);
  }
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
  if (resp.ok) bumpBadge(1);
  else notify("godm error", resp.error || "unknown error");
});

// ---------- popup / options bridge ----------

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  callHost(msg).then(sendResponse);
  return true; // keep the channel open for the async reply
});

"use strict";

const DEFAULTS = {
  enabled: true,
  confirmDownload: true,
  takeAll: false,
  minSize: 1048576,
  connections: 8,
  extensions:
    "7z,apk,appimage,avi,bin,bz2,deb,dmg,exe,flac,flv,gz,img,iso,jar,mkv,mov,mp3,mp4,msi,ova,pdf,pkg,rar,rpm,tar,vhd,wav,webm,whl,xz,zip,zst",
  blocklist: ""
};

const fields = ["enabled", "takeAll", "confirmDownload", "minSize", "connections", "extensions", "blocklist"];

function load() {
  chrome.storage.sync.get(DEFAULTS, (cfg) => {
    for (const f of fields) {
      const el = document.getElementById(f);
      if (el.type === "checkbox") el.checked = cfg[f];
      else el.value = cfg[f];
    }
  });
}

document.getElementById("save").addEventListener("click", () => {
  const out = {};
  for (const f of fields) {
    const el = document.getElementById(f);
    if (el.type === "checkbox") out[f] = el.checked;
    else if (el.type === "number") out[f] = parseInt(el.value, 10) || DEFAULTS[f];
    else out[f] = el.value.trim();
  }
  chrome.storage.sync.set(out, () => {
    const s = document.getElementById("status");
    s.textContent = tr("options_saved");
    setTimeout(() => (s.textContent = ""), 1600);
  });
});

document.getElementById("extid").textContent = chrome.runtime.id;
document.getElementById("cmd").textContent = "godm install --ext-id " + chrome.runtime.id;

chrome.runtime.sendMessage({ type: "ping" }, (resp) => {
  const el = document.getElementById("probe");
  if (resp && resp.ok) {
    el.textContent = tr("options_probe_ok");
    el.className = "good";
  } else {
    el.textContent = (resp && resp.error) || tr("options_probe_none");
    el.className = "bad";
  }
});

// Show the startup self-test too: it runs without the user triggering a
// download, so it tells them whether the next download will actually work.
chrome.storage.local.get({ lastPing: null }, (s) => {
  const el = document.getElementById("lastping");
  if (!el) return;
  if (!s.lastPing) {
    el.textContent = tr("options_ping_never");
    return;
  }
  const when = new Date(s.lastPing.at).toLocaleString();
  el.textContent = s.lastPing.ok
    ? tr("options_ping_ok", when)
    : tr("options_ping_bad", [s.lastPing.error, when]);
  el.className = s.lastPing.ok ? "good" : "bad";
});

load();

"use strict";

const DEFAULTS = {
  enabled: true,
  confirmDownload: true,
  takeAll: false,
  minSize: 1048576,
  connections: 8,
  extensions:
    "7z,apk,appimage,avi,bin,bz2,deb,dmg,exe,flac,flv,gz,img,iso,jar,mkv,mov,mp3,mp4,msi,ova,pdf,pkg,rar,rpm,tar,vhd,wav,webm,whl,xz,zip,zst",
  blocklist: "",
  takeMagnets: true,
  takeTorrentFiles: true,
  // Which language the extension speaks; see i18n.js.
  language: LANGUAGE_DEFAULT
};

const fields = ["enabled", "takeAll", "confirmDownload", "takeMagnets", "takeTorrentFiles", "minSize", "connections", "extensions", "blocklist", "language"];

// The two torrent switches replaced one, takeTorrents, and one never saved
// shows what that one said, as magnet.js and the worker read it.
const TORRENT_SWITCHES = ["takeMagnets", "takeTorrentFiles"];

function withTorrentSwitches(got) {
  const cfg = Object.assign({}, got);
  for (const f of TORRENT_SWITCHES) {
    if (typeof cfg[f] !== "boolean") cfg[f] = cfg.takeTorrents !== false;
  }
  return cfg;
}

// languageOf is a language the extension has, or else the default: a value
// that synced in from a newer version, say, must not leave the box empty.
function languageOf(value) {
  return LANGUAGES.includes(value) ? value : LANGUAGE_DEFAULT;
}

function load() {
  const unset = { takeMagnets: null, takeTorrentFiles: null, takeTorrents: null };
  chrome.storage.sync.get(Object.assign({}, DEFAULTS, unset), (got) => {
    const cfg = withTorrentSwitches(got);
    for (const f of fields) {
      const el = document.getElementById(f);
      if (el.type === "checkbox") el.checked = cfg[f];
      else el.value = f === "language" ? languageOf(cfg[f]) : cfg[f];
    }
  });
}

// The smallest and largest value each number field takes. Zero is a real answer
// for the minimum size ("take over small files too"), so a value is judged by
// being a number and not by being truthy.
const LIMITS = { minSize: [0, Infinity], connections: [1, 32] };

// numberOf reads a number field. Something that is not a number at all gets the
// default; a number outside the field's limits gets the nearest one, except
// that a negative size means nothing and gets the default too.
function numberOf(f, text) {
  const n = parseInt(text, 10);
  if (!Number.isFinite(n)) return DEFAULTS[f];
  const [lo, hi] = LIMITS[f];
  if (f === "minSize" && n < lo) return DEFAULTS[f];
  return Math.min(hi, Math.max(lo, n));
}

document.getElementById("save").addEventListener("click", () => {
  const out = {};
  for (const f of fields) {
    const el = document.getElementById(f);
    if (el.type === "checkbox") out[f] = el.checked;
    else if (el.type === "number") {
      out[f] = numberOf(f, el.value);
      // Show what was kept, so the field never says one thing while storage
      // holds another.
      el.value = out[f];
    } else if (f === "language") out[f] = languageOf(el.value);
    else out[f] = el.value.trim();
  }
  chrome.storage.sync.set(out, () => {
    const s = document.getElementById("status");
    // The page changes language here and now, and says "Saved" in it. What it
    // worked out for itself in the old one is worked out again.
    useLanguage(out.language).then(() => {
      showProbe();
      showSelfTest();
      s.textContent = tr("options_saved");
      setTimeout(() => (s.textContent = ""), 1600);
    });
  });
});

document.getElementById("extid").textContent = chrome.runtime.id;
document.getElementById("cmd").textContent = "godm install --ext-id " + chrome.runtime.id;

let probe = null; // what the native host answered to the ping, once it has
let selfTest = null; // what the startup self-test found, once it is read

function showProbe() {
  if (!probe) return;
  const el = document.getElementById("probe");
  if (probe.ok) {
    el.textContent = tr("options_probe_ok");
    el.className = "good";
  } else {
    el.textContent = probe.error || tr("options_probe_none");
    el.className = "bad";
  }
}

// Show the startup self-test too: it runs without the user triggering a
// download, so it tells them whether the next download will actually work.
function showSelfTest() {
  const el = document.getElementById("lastping");
  if (!el || !selfTest) return;
  if (!selfTest.lastPing) {
    el.textContent = tr("options_ping_never");
    return;
  }
  const last = selfTest.lastPing;
  const when = new Date(last.at).toLocaleString();
  el.textContent = last.ok ? tr("options_ping_ok", when) : tr("options_ping_bad", [last.error, when]);
  el.className = last.ok ? "good" : "bad";
}

// These say things in words, so they wait for the language; see i18n.js.
i18nReady.then(() => {
  chrome.runtime.sendMessage({ type: "ping" }, (resp) => {
    probe = resp || {};
    showProbe();
  });
  chrome.storage.local.get({ lastPing: null }, (s) => {
    selfTest = s;
    showSelfTest();
  });
});

load();

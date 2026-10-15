"use strict";

// Kept in step with DEFAULTS in background.js and options.js.
const DEFAULTS = {
  connections: 8,
  extensions:
    "7z,apk,appimage,avi,bin,bz2,deb,dmg,exe,flac,flv,gz,img,iso,jar,mkv,mov,mp3,mp4,msi,ova,pdf,pkg,rar,rpm,tar,vhd,wav,webm,whl,xz,zip,zst"
};

const state = { page: "", title: "", links: [], selected: new Set(), types: new Set(), query: "", fileTypes: new Set() };

// What a link with no file type is filed under. It is only an identity: the chip
// that shows it is labelled in the person's language.
const NO_TYPE = "(none)";
// The group of the videos the page played, which are not in its HTML but were
// seen going by. Like NO_TYPE it is an identity, and cannot be mistaken for a
// file type, which is letters and digits.
const VIDEOS = "(video)";

function isVideo(link) {
  return link.kind === "video";
}

// groupOf is the chip a link is filed under.
function groupOf(link) {
  return isVideo(link) ? VIDEOS : link.ext || NO_TYPE;
}

function human(n) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
}

function baseOf(url) {
  try {
    return decodeURIComponent(new URL(url).pathname.split("/").pop() || "");
  } catch (e) {
    return "";
  }
}

// The type comes from the path only. A bare site link falls back to showing
// its host, and "example.com" must not be mistaken for a .com file.
function extOf(link) {
  const m = /\.([a-z0-9]{1,5})$/i.exec(baseOf(link.url));
  return m ? m[1].toLowerCase() : "";
}

function nameFor(link) {
  const base = baseOf(link.url);
  if (base) return base;
  try {
    return new URL(link.url).host;
  } catch (e) {
    return link.url;
  }
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function shown() {
  const q = state.query;
  return state.links.filter((l) => {
    if (state.types.size && !state.types.has(groupOf(l))) return false;
    if (!q) return true;
    return l.name.toLowerCase().includes(q) || l.text.toLowerCase().includes(q) || l.url.toLowerCase().includes(q);
  });
}

function renderChips() {
  const counts = new Map();
  for (const l of state.links) {
    const k = groupOf(l);
    counts.set(k, (counts.get(k) || 0) + 1);
  }
  // The videos the page played first, then file types, then the rest by
  // frequency, so "zip 12" beats "html 80".
  const rank = (k) => (k === VIDEOS ? 0 : state.fileTypes.has(k) ? 1 : 2);
  const keys = [...counts.keys()].sort((a, b) => {
    return rank(a) - rank(b) || counts.get(b) - counts.get(a) || a.localeCompare(b);
  });
  const label = (k) => (k === VIDEOS ? tr("picker_videos") : k === NO_TYPE ? tr("picker_none") : k);
  document.getElementById("chips").innerHTML = keys.map((k) =>
    '<button class="chip' + (state.types.has(k) ? " on" : "") + '" data-type="' + esc(k) + '">' +
    "<b>" + esc(label(k)) + "</b><span>" + counts.get(k) + "</span></button>").join("");
}

function render() {
  const list = shown();
  document.getElementById("rows").innerHTML = list.map((l) => {
    const on = state.selected.has(l.url);
    return '<tr class="' + (on ? "" : "off") + '" data-url="' + esc(l.url) + '">' +
      '<td><input type="checkbox" ' + (on ? "checked" : "") + "></td>" +
      '<td><div class="name">' + esc(l.name) + "</div>" +
      (l.text && l.text !== l.name ? '<div class="text">' + esc(l.text) + "</div>" : "") +
      '<div class="url">' + esc(l.url) + "</div></td>" +
      '<td class="ext">' + esc(isVideo(l) ? l.format.toUpperCase() : l.ext || (l.kind === "media" ? tr("picker_media") : "")) + "</td></tr>";
  }).join("");
  document.getElementById("empty").hidden = list.length > 0;

  const shownSelected = list.filter((l) => state.selected.has(l.url)).length;
  const toggle = document.getElementById("toggleShown");
  toggle.checked = list.length > 0 && shownSelected === list.length;
  toggle.indeterminate = shownSelected > 0 && shownSelected < list.length;

  const n = state.selected.size;
  document.getElementById("count").textContent = list.length !== state.links.length
    ? tr("picker_count_shown", [n, state.links.length, list.length])
    : tr("picker_count", [n, state.links.length]);
  const go = document.getElementById("go");
  go.disabled = n === 0;
  go.textContent = n > 1 ? tr("picker_download_many", n) : tr("picker_download");
}

function setStatus(msg, kind) {
  const el = document.getElementById("status");
  el.textContent = msg;
  el.className = "status" + (kind ? " " + kind : "");
}

document.getElementById("rows").addEventListener("click", (e) => {
  const row = e.target.closest("tr[data-url]");
  if (!row) return;
  const url = row.getAttribute("data-url");
  if (state.selected.has(url)) state.selected.delete(url);
  else state.selected.add(url);
  render();
});

document.getElementById("chips").addEventListener("click", (e) => {
  const b = e.target.closest("[data-type]");
  if (!b) return;
  const t = b.getAttribute("data-type");
  if (state.types.has(t)) state.types.delete(t);
  else state.types.add(t);
  renderChips();
  render();
});

document.getElementById("search").addEventListener("input", (e) => {
  state.query = e.target.value.trim().toLowerCase();
  render();
});

document.getElementById("toggleShown").addEventListener("change", (e) => {
  for (const l of shown()) {
    if (e.target.checked) state.selected.add(l.url);
    else state.selected.delete(l.url);
  }
  render();
});
document.getElementById("selAll").addEventListener("click", () => {
  shown().forEach((l) => state.selected.add(l.url));
  render();
});
document.getElementById("selNone").addEventListener("click", () => {
  state.selected.clear();
  render();
});
document.getElementById("selFiles").addEventListener("click", () => {
  state.selected = new Set(state.links.filter((l) => state.fileTypes.has(l.ext)).map((l) => l.url));
  render();
});
document.getElementById("cancel").addEventListener("click", () => window.close());

// itemFor is what the worker is sent for a link. A stream the page played says
// it is one, so that godm runs it as a stream whatever its address looks like,
// and is named for the page, since a playlist is nearly always called
// index.m3u8. It names no quality: the best is taken, and the picker has no row
// to choose one in. A video that is a file is a file like any other link.
function itemFor(link) {
  if (!isVideo(link) || link.format !== "hls") return { url: link.url };
  return { url: link.url, kind: "hls", filename: link.title || state.title };
}

document.getElementById("go").addEventListener("click", async () => {
  // Keep page order: that is the order the queue will download in.
  const items = state.links.filter((l) => state.selected.has(l.url)).map(itemFor);
  if (!items.length) return;
  const go = document.getElementById("go");
  go.disabled = true;
  setStatus(tr("picker_sending", items.length));
  const connections = parseInt(document.getElementById("conns").value, 10) || DEFAULTS.connections;
  const resp = await chrome.runtime.sendMessage({ type: "godm-batch", items, referrer: state.page, connections });
  if (resp && resp.ok) {
    const n = (resp.ids || []).length;
    setStatus(tr(n === 1 ? "picker_added_one" : "picker_added_other", n), "ok");
    setTimeout(() => window.close(), 1200);
  } else {
    setStatus((resp && resp.error) || tr("err_no_response"), "err");
    go.disabled = false;
  }
});

// linksOf is the list the picker shows: the videos the page played, then the
// links in its HTML. A video the page also links to, a <video src> the sniffer
// saw being fetched, is listed once, as the video it is, because a row is told
// from the others by its address.
function linksOf(data) {
  const links = [];
  const seen = new Set();
  for (const v of data.videos || []) {
    if (seen.has(v.url)) continue;
    seen.add(v.url);
    const stream = v.kind === "hls";
    const link = { url: v.url, kind: "video", format: stream ? "hls" : "file", title: v.title || "" };
    // A playlist is called index.m3u8 or master.m3u8 by nearly every site, so
    // what the page is called says more about it, as it does in the popup.
    link.name = (stream && link.title) || nameFor(link);
    link.text = stream
      ? tr("picker_video_hls")
      : [v.type, v.size > 0 ? human(v.size) : ""].filter(Boolean).join(" · ");
    // A file is filed by its type as any link is, and a stream has none.
    link.ext = stream ? "" : extOf(link);
    links.push(link);
  }
  for (const l of data.links || []) {
    if (seen.has(l.url)) continue;
    seen.add(l.url);
    const link = { url: l.url, text: l.text || "", kind: l.kind };
    link.name = nameFor(link);
    link.ext = extOf(link);
    links.push(link);
  }
  return links;
}

(async function init() {
  const key = new URLSearchParams(location.search).get("k");
  const cfg = await chrome.storage.sync.get(DEFAULTS);
  state.fileTypes = new Set(String(cfg.extensions).split(/[\s,]+/).map((s) => s.trim().toLowerCase().replace(/^\./, "")).filter(Boolean));
  document.getElementById("conns").value = cfg.connections || DEFAULTS.connections;

  const data = key ? (await chrome.storage.session.get(key))[key] : null;
  if (key) chrome.storage.session.remove(key);
  if (!data) {
    document.getElementById("title").textContent = tr("picker_expired");
    document.getElementById("page").textContent = tr("picker_expired_sub");
    document.getElementById("empty").hidden = false;
    return;
  }

  state.page = data.page;
  state.title = data.title || "";
  state.links = linksOf(data);
  // Start with what godm would have taken over anyway: real files, not pages.
  // A stream is left for the person to pick: a page can play several, and one
  // may be many gigabytes.
  state.selected = new Set(state.links.filter((l) => state.fileTypes.has(l.ext)).map((l) => l.url));

  document.getElementById("title").textContent = data.title || tr("picker_heading");
  document.getElementById("page").textContent = data.page;
  document.title = data.title ? "godm — " + data.title : tr("picker_title");
  renderChips();
  render();
})();

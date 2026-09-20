"use strict";

let uiURL = "";

function human(n) {
  if (n === null || n === undefined || n < 0) return "?";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + " " + u[i];
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"]/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function render(tasks) {
  const body = document.getElementById("body");
  if (!tasks.length) {
    body.innerHTML = '<div class="msg">No downloads yet.</div>';
    return;
  }
  body.innerHTML = tasks.slice(0, 8).map((t) => {
    const pct = t.size > 0 ? Math.min(100, (t.received / t.size) * 100)
      : (t.state === "done" ? 100 : 0);
    const cls = t.state === "done" ? "done" : (t.state === "error" ? "error" : "");
    const labels = { queued: "queued", paused: "paused", done: "done", error: "failed",
      needs_refresh: "link expired", awaiting_refresh: "waiting for link" };
    const right = t.state === "running"
      ? human(t.speed) + "/s" + (t.segments && t.segments.length > 1 ? " · " + t.active + " conn" : "")
      : (labels[t.state] || t.state);
    return '<div class="task">'
      + '<div class="row"><span class="name">' + esc(t.filename || t.url) + '</span>'
      + '<span class="meta">' + esc(right) + '</span></div>'
      + '<div class="track"><div class="fill ' + cls + '" style="width:' + pct.toFixed(1) + '%"></div></div>'
      + '</div>';
  }).join("");
}

function refresh() {
  chrome.runtime.sendMessage({ type: "tasks" }, (resp) => {
    if (!resp || !resp.ok) {
      const err = (resp && resp.error) || "no response";
      document.getElementById("body").innerHTML =
        '<div class="msg bad">Cannot reach godm.<br><small>' + esc(err) + "</small><br><br>"
        + "<small>Run <b>godm install --ext-id " + esc(chrome.runtime.id)
        + "</b> and restart the browser.</small></div>";
      return;
    }
    uiURL = resp.ui || "";
    render(resp.tasks || []);
  });
}

document.getElementById("open").addEventListener("click", () => {
  if (uiURL) chrome.tabs.create({ url: uiURL });
});
document.getElementById("opts").addEventListener("click", () => {
  chrome.runtime.openOptionsPage();
});
document.getElementById("grab").addEventListener("click", () => {
  chrome.runtime.sendMessage({ type: "godm-grab-active" }, (resp) => {
    if (resp && resp.ok) window.close(); // the picker window takes over
  });
});

const box = document.getElementById("enabled");
chrome.storage.sync.get({ enabled: true }, (c) => { box.checked = c.enabled; });
box.addEventListener("change", () => {
  chrome.storage.sync.set({ enabled: box.checked });
});

refresh();
setInterval(refresh, 1000);

// ---------- video found on this page ----------

const mediaState = { items: [], page: "", title: "" };
const infoByUrl = new Map();   // url -> StreamInfo, or { error }
const chosenByUrl = new Map(); // url -> variant index the user picked

// Only the first few streams are looked at. Reading a playlist is a request
// per stream, and a page with a dozen of them is usually a wall of previews.
const INSPECT_LIMIT = 3;

function baseName(u) {
  try {
    const p = new URL(u);
    return decodeURIComponent(p.pathname.split("/").filter(Boolean).pop() || "") || p.hostname;
  } catch (e) {
    return u;
  }
}

function fmtDuration(sec) {
  if (!sec || sec <= 0) return "";
  const s = Math.round(sec);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = s % 60;
  const mm = h ? String(m).padStart(2, "0") : String(m);
  return (h ? h + ":" : "") + mm + ":" + String(r).padStart(2, "0");
}

function mediaTitle(m) {
  // A playlist is nearly always called index.m3u8 or master.m3u8, so the page
  // title is the only thing that tells two of them apart.
  if (m.kind !== "file" && m.title) return m.title;
  return baseName(m.url);
}

function mediaDetail(m) {
  if (m.kind === "dash") return "DASH — not supported yet";
  if (m.kind === "file") return [human(m.size), m.type].filter(Boolean).join(" · ");
  const info = infoByUrl.get(m.url);
  if (!info) return "reading the playlist…";
  if (info.error) return info.error;
  return [fmtDuration(info.duration), info.segments ? info.segments + " segments" : ""]
    .filter(Boolean).join(" · ") || "stream";
}

function renderMedia() {
  const el = document.getElementById("media");
  if (!mediaState.items.length) {
    el.innerHTML = "";
    return;
  }
  el.innerHTML =
    '<div class="sec">Video on this page (' + mediaState.items.length + ")</div>" +
    mediaState.items.map((m, i) => {
      const info = infoByUrl.get(m.url);
      const blocked = m.kind === "dash" || (info && info.error);
      let picker = "";
      if (info && info.variants && info.variants.length > 1) {
        const chosen = chosenByUrl.has(m.url) ? chosenByUrl.get(m.url) : info.best;
        picker = '<select data-pick="' + i + '">' + info.variants.map((v) =>
          '<option value="' + v.index + '"' + (v.index === chosen ? " selected" : "") + ">"
          + esc(v.label) + "</option>").join("") + "</select>";
      }
      return '<div class="media">'
        + '<div class="row"><span class="chip ' + (m.kind === "hls" ? "hls" : "")
        + '">' + esc(m.kind.toUpperCase()) + "</span>"
        + '<span class="name">' + esc(mediaTitle(m)) + "</span></div>"
        + '<div class="meta">' + esc(mediaDetail(m)) + "</div>"
        + '<div class="foot">' + picker
        + '<button class="go" data-get="' + i + '"' + (blocked ? " disabled" : "") + ">"
        + (blocked ? "unavailable" : "Download") + "</button></div>"
        + "</div>";
    }).join("");
}

function loadMedia() {
  chrome.runtime.sendMessage({ type: "godm-media" }, (resp) => {
    if (!resp || !resp.ok) return;
    mediaState.items = resp.items || [];
    mediaState.page = resp.page || "";
    mediaState.title = resp.title || "";
    renderMedia();
    mediaState.items.filter((m) => m.kind === "hls").slice(0, INSPECT_LIMIT)
      .forEach((m) => inspect(m));
  });
}

function inspect(m) {
  if (infoByUrl.has(m.url)) return;
  chrome.runtime.sendMessage(
    { type: "godm-media-inspect", url: m.url, page: mediaState.page },
    (resp) => {
      infoByUrl.set(m.url, resp && resp.ok && resp.info
        ? resp.info
        : { error: (resp && resp.error) || "could not read the playlist" });
      renderMedia();
    });
}

document.getElementById("media").addEventListener("change", (e) => {
  const i = e.target.dataset.pick;
  if (i === undefined) return;
  chosenByUrl.set(mediaState.items[i].url, parseInt(e.target.value, 10));
});

document.getElementById("media").addEventListener("click", (e) => {
  const i = e.target.dataset.get;
  if (i === undefined) return;
  const m = mediaState.items[i];
  const info = infoByUrl.get(m.url);
  const variant = chosenByUrl.has(m.url)
    ? chosenByUrl.get(m.url)
    : (info && typeof info.best === "number" ? info.best : -1);

  e.target.disabled = true;
  e.target.textContent = "sending…";
  chrome.runtime.sendMessage({
    type: "godm-media-download",
    url: m.url,
    kind: m.kind,
    variant: variant,
    // A stream has no filename of its own worth using; the page title is what
    // the user would have called it.
    filename: m.kind === "hls" ? (m.title || mediaState.title || "") : "",
    page: mediaState.page
  }, (resp) => {
    e.target.textContent = resp && resp.ok ? "queued" : "failed";
    if (resp && !resp.ok) e.target.title = resp.error || "";
    refresh();
  });
});

loadMedia();

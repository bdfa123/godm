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
    body.innerHTML = '<div class="msg">' + esc(tr("popup_empty")) + "</div>";
    return;
  }
  body.innerHTML = tasks.slice(0, 8).map((t) => {
    const pct = t.size > 0 ? Math.min(100, (t.received / t.size) * 100)
      : (t.state === "done" ? 100 : 0);
    const cls = t.state === "done" ? "done" : (t.state === "error" ? "error" : "");
    const labels = { queued: "popup_state_queued", paused: "popup_state_paused", done: "popup_state_done",
      error: "popup_state_error", needs_refresh: "popup_state_needs_refresh",
      awaiting_refresh: "popup_state_awaiting_refresh" };
    const right = t.state === "running"
      ? human(t.speed) + "/s" + (t.segments && t.segments.length > 1 ? " · " + tr("popup_conns", t.active) : "")
      : (labels[t.state] ? tr(labels[t.state]) : t.state);
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
      const err = (resp && resp.error) || tr("popup_no_response");
      document.getElementById("body").innerHTML =
        '<div class="msg bad">' + esc(tr("popup_unreachable")) + "<br><small>" + esc(err) + "</small><br><br>"
        + "<small>" + tr("popup_run_install", esc(chrome.runtime.id)) + "</small></div>";
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
let daemonCfg = null; // what the daemon can do; null until it has answered
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
  if (m.kind === "dash") return tr("popup_dash");
  if (m.kind === "file") return [human(m.size), m.type].filter(Boolean).join(" · ");
  const info = infoByUrl.get(m.url);
  if (!info) return tr("popup_reading_playlist");
  if (info.error) return info.error;
  return [fmtDuration(info.duration), info.segments ? tr("popup_segments", info.segments) : ""]
    .filter(Boolean).join(" · ") || tr("popup_stream");
}

// The page itself is worth offering when a site hands its video out through a
// protocol of its own: nothing shows up in the sniffer, but yt-dlp knows the
// site. It is a pseudo-item so the same row, picker and button serve both.
function pageItem() {
  return { url: mediaState.page, kind: "yt-dlp", title: mediaState.title };
}

function itemAt(i) {
  return i === "page" ? pageItem() : mediaState.items[i];
}

function canUsePage() {
  return !!daemonCfg && /^https?:/i.test(mediaState.page || "");
}

function pageRow() {
  if (!canUsePage()) return "";
  const item = pageItem();
  const info = infoByUrl.get(item.url);
  const missing = daemonCfg.ytdlp === false;
  let detail, picker = "", label = tr("popup_read_qualities");
  if (missing) {
    detail = tr("popup_ytdlp_missing");
    label = tr("popup_unavailable");
  } else if (!info) {
    detail = tr("popup_ytdlp_ask");
  } else if (info.error) {
    detail = info.error;
    label = tr("popup_unavailable");
  } else {
    detail = [info.title || "", fmtDuration(info.duration)].filter(Boolean).join(" · ") || tr("popup_ready");
    label = tr("popup_download");
    if (info.variants && info.variants.length > 1) {
      const chosen = chosenByUrl.has(item.url) ? chosenByUrl.get(item.url) : info.best;
      picker = '<select data-pick="page">' + info.variants.map((v) =>
        '<option value="' + v.index + '"' + (v.index === chosen ? " selected" : "") + ">"
        + esc(v.label) + "</option>").join("") + "</select>";
    }
  }
  const blocked = missing || (info && info.error);
  return '<div class="sec">' + esc(tr("popup_this_page")) + '</div><div class="media">'
    + '<div class="row"><span class="chip">YT-DLP</span>'
    + '<span class="name">' + esc(mediaState.title || mediaState.page) + "</span></div>"
    + '<div class="meta">' + esc(detail) + "</div>"
    + '<div class="foot">' + picker
    + '<button class="go" data-get="page"' + (blocked ? " disabled" : "") + ">"
    + esc(label) + "</button></div></div>";
}

function renderMedia() {
  const el = document.getElementById("media");
  const found = mediaState.items.length
    ? '<div class="sec">' + esc(tr("popup_video_on_page", mediaState.items.length)) + "</div>" +
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
        + esc(blocked ? tr("popup_unavailable") : tr("popup_download")) + "</button></div>"
        + "</div>";
    }).join("")
    : "";
  el.innerHTML = found + pageRow();
}

function loadConfig() {
  chrome.runtime.sendMessage({ type: "godm-config" }, (resp) => {
    daemonCfg = (resp && resp.ok && resp.config) || {};
    renderMedia();
  });
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

function inspect(m, done) {
  if (infoByUrl.has(m.url)) {
    if (done) done();
    return;
  }
  chrome.runtime.sendMessage(
    { type: "godm-media-inspect", url: m.url, kind: m.kind, page: mediaState.page },
    (resp) => {
      infoByUrl.set(m.url, resp && resp.ok && resp.info
        ? resp.info
        : { error: (resp && resp.error) || tr("popup_playlist_error") });
      renderMedia();
      if (done) done();
    });
}

document.getElementById("media").addEventListener("change", (e) => {
  const i = e.target.dataset.pick;
  if (i === undefined) return;
  chosenByUrl.set(itemAt(i).url, parseInt(e.target.value, 10));
});

document.getElementById("media").addEventListener("click", (e) => {
  const i = e.target.dataset.get;
  if (i === undefined) return;
  const m = itemAt(i);

  // Reading a page with yt-dlp takes a few seconds and a request to the site,
  // so it happens on the first click rather than every time the popup opens.
  if (m.kind === "yt-dlp" && !infoByUrl.has(m.url)) {
    e.target.disabled = true;
    e.target.textContent = tr("popup_btn_reading");
    inspect(m);
    return;
  }

  const info = infoByUrl.get(m.url);
  const variant = chosenByUrl.has(m.url)
    ? chosenByUrl.get(m.url)
    : (info && typeof info.best === "number" ? info.best : -1);

  e.target.disabled = true;
  e.target.textContent = tr("popup_btn_sending");
  chrome.runtime.sendMessage({
    type: "godm-media-download",
    url: m.url,
    kind: m.kind,
    variant: variant,
    // A stream has no filename of its own worth using; the page title is what
    // the user would have called it.
    // yt-dlp names the file from the site's own metadata; a stream has no
    // name worth using, so the page title stands in.
    filename: m.kind === "hls" ? (m.title || mediaState.title || "") : "",
    page: mediaState.page
  }, (resp) => {
    e.target.textContent = tr(resp && resp.ok ? "popup_btn_queued" : "popup_btn_failed");
    if (resp && !resp.ok) e.target.title = resp.error || "";
    refresh();
  });
});

loadConfig();
loadMedia();

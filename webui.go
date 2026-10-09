package main

// uiHTML is served at / with __TOKEN__ replaced by the daemon secret so the
// page can call the guarded API without the user pasting anything. It is a
// single self-contained page: the app window and a plain browser tab load the
// same thing.
const uiHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>godm</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f3f4f7; --panel: #ffffff; --panel2: #f7f8fa; --line: #e2e5ea; --text: #151920;
    --muted: #697280; --faint: #9aa2ad;
    --accent: #2f6df6; --accent-soft: #e7eefe;
    --ok: #1c9551; --ok-soft: #e2f4e9;
    --warn: #b86e00; --warn-soft: #fff3de;
    --err: #d23f3f; --err-soft: #fce9e9;
    --track: #e7eaef; --gap: #ffffff;
    --shadow: 0 1px 2px rgba(20, 25, 35, .06);
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #111419; --panel: #191d24; --panel2: #1f242c; --line: #2a3039; --text: #e5e8ed;
      --muted: #8b939e; --faint: #626a75;
      --accent: #5b8cff; --accent-soft: #1d2842;
      --ok: #3bb471; --ok-soft: #16301f;
      --warn: #e2a13a; --warn-soft: #36290f;
      --err: #ee6b6b; --err-soft: #3a1c1c;
      --track: #262c35; --gap: #191d24;
      --shadow: none;
    }
  }
  * { box-sizing: border-box; }
  /* Components below set display:grid/flex, which would otherwise override the
     hidden attribute and leave every dialog open. */
  [hidden] { display: none !important; }
  html, body { height: 100%; }
  body {
    margin: 0; background: var(--bg); color: var(--text);
    font: 13.5px/1.45 "Segoe UI", system-ui, -apple-system, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  button, input, select, textarea { font: inherit; color: inherit; }

  /* ---------- top bar ---------- */
  .top {
    position: sticky; top: 0; z-index: 5; background: var(--panel);
    border-bottom: 1px solid var(--line); padding: 10px 18px;
    display: flex; align-items: center; gap: 14px; flex-wrap: wrap;
  }
  .brand { display: flex; align-items: center; gap: 8px; font-weight: 650; letter-spacing: .3px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--ok); }
  .dot.off { background: var(--err); }
  .stats { color: var(--muted); font-variant-numeric: tabular-nums; flex: 1; min-width: 160px; }
  .stats b { color: var(--text); font-weight: 600; }
  .ctl { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .field { display: flex; align-items: center; gap: 6px; color: var(--muted); }
  select, input[type=number], input[type=text], input[type=url], textarea {
    background: var(--panel2); border: 1px solid var(--line); border-radius: 6px; padding: 5px 8px;
  }
  select:focus, input:focus, textarea:focus { outline: 2px solid var(--accent-soft); border-color: var(--accent); }

  .btn {
    border: 1px solid var(--line); background: var(--panel); border-radius: 6px;
    padding: 5px 11px; cursor: pointer; white-space: nowrap;
  }
  .btn:hover { background: var(--panel2); }
  .btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
  .btn.primary:hover { filter: brightness(1.07); }
  .btn.warn { background: var(--warn); border-color: var(--warn); color: #fff; }
  .btn.small { padding: 3px 9px; font-size: 12.5px; }
  .btn.ghost { border-color: transparent; background: transparent; color: var(--muted); }
  .btn.ghost:hover { background: var(--panel2); color: var(--text); }
  .btn.danger:hover { color: var(--err); }
  .btn:disabled { opacity: .5; cursor: default; }

  /* ---------- tabs ---------- */
  .tabs { display: flex; gap: 4px; padding: 10px 18px 0; flex-wrap: wrap; }
  .tab {
    border: 0; background: transparent; padding: 6px 12px; border-radius: 6px; cursor: pointer; color: var(--muted);
  }
  .tab:hover { background: var(--panel); color: var(--text); }
  .tab.on { background: var(--panel); color: var(--text); box-shadow: var(--shadow); font-weight: 600; }
  .tab .n { margin-left: 5px; font-size: 11.5px; color: var(--faint); font-variant-numeric: tabular-nums; }
  .tab.attn .n { color: var(--warn); font-weight: 700; }

  main { padding: 10px 18px 40px; max-width: 1100px; margin: 0 auto; }

  /* ---------- task card ---------- */
  .task {
    background: var(--panel); border: 1px solid var(--line); border-radius: 9px;
    padding: 12px 14px; margin-bottom: 8px; box-shadow: var(--shadow);
    display: grid; grid-template-columns: 38px 1fr; gap: 0 12px;
  }
  .ico {
    width: 38px; height: 38px; border-radius: 8px; display: grid; place-items: center;
    font-size: 10px; font-weight: 700; letter-spacing: .4px; color: #fff; background: var(--faint);
    text-transform: uppercase; overflow: hidden;
  }
  .ico.video { background: #7b5cf0; } .ico.audio { background: #d0558f; } .ico.archive { background: #c7821c; }
  .ico.doc { background: #3478d0; } .ico.app { background: #2f9467; } .ico.image { background: #19a3a3; }
  .body { min-width: 0; }
  .row { display: flex; align-items: baseline; gap: 10px; min-width: 0; }
  .name { font-weight: 600; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .pill {
    font-size: 11px; font-weight: 600; padding: 1px 8px; border-radius: 999px; white-space: nowrap;
    background: var(--panel2); color: var(--muted); border: 1px solid var(--line);
  }
  .pill.running { background: var(--accent-soft); color: var(--accent); border-color: transparent; }
  .pill.done { background: var(--ok-soft); color: var(--ok); border-color: transparent; }
  .pill.error { background: var(--err-soft); color: var(--err); border-color: transparent; }
  .pill.needs_refresh, .pill.awaiting_refresh { background: var(--warn-soft); color: var(--warn); border-color: transparent; }
  .pill.awaiting_refresh { animation: pulse 1.6s ease-in-out infinite; }
  @keyframes pulse { 50% { opacity: .55; } }

  .bar { height: 6px; background: var(--track); border-radius: 3px; overflow: hidden; margin: 8px 0 5px; }
  .fill { height: 100%; background: var(--accent); border-radius: 3px; transition: width .35s ease; }
  .fill.done { background: var(--ok); } .fill.error { background: var(--err); }
  .fill.needs_refresh, .fill.awaiting_refresh { background: var(--warn); }
  .fill.paused, .fill.queued { background: var(--faint); }

  .segmap { position: relative; height: 12px; background: var(--track); border-radius: 3px; overflow: hidden; margin: 2px 0 6px; }
  .seg { position: absolute; top: 0; bottom: 0; border-right: 2px solid var(--gap); overflow: hidden; }
  .seg:last-child { border-right: 0; }
  .seg i { display: block; height: 100%; background: var(--faint); transition: width .35s ease; }
  .seg.active i, .seg.connecting i { background: var(--accent); }
  .seg.retrying i { background: var(--warn); }
  .seg.done i { background: var(--ok); }
  .seg.failed i { background: var(--err); }
  .seg.connecting { background: repeating-linear-gradient(-45deg, transparent 0 4px, var(--accent-soft) 4px 8px); }
  .seg.retrying { background: repeating-linear-gradient(-45deg, transparent 0 4px, var(--warn-soft) 4px 8px); }

  .meta { color: var(--muted); font-size: 12.5px; font-variant-numeric: tabular-nums; display: flex; flex-wrap: wrap; gap: 2px 14px; }
  .meta b { color: var(--text); font-weight: 600; }
  .url { color: var(--faint); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-top: 1px; }

  .banner { margin-top: 8px; padding: 8px 10px; border-radius: 7px; font-size: 12.5px; display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
  .banner.warn { background: var(--warn-soft); color: var(--text); }
  .banner.err { background: var(--err-soft); color: var(--text); }
  .banner .msg { flex: 1; min-width: 220px; }
  .banner .msg b { color: var(--warn); }
  .banner.err .msg b { color: var(--err); }
  .banner .hint { display: block; color: var(--muted); margin-top: 2px; }

  .actions { display: flex; gap: 4px; margin-top: 8px; flex-wrap: wrap; }

  .conns { margin-top: 8px; border-top: 1px dashed var(--line); padding-top: 6px; overflow-x: auto; }
  table { border-collapse: collapse; width: 100%; font-size: 12px; font-variant-numeric: tabular-nums; }
  th { text-align: left; color: var(--faint); font-weight: 600; padding: 3px 8px 3px 0; white-space: nowrap; }
  td { padding: 3px 8px 3px 0; white-space: nowrap; }
  td.note { color: var(--muted); white-space: normal; min-width: 160px; }
  .mini { width: 90px; height: 5px; background: var(--track); border-radius: 3px; overflow: hidden; }
  .mini i { display: block; height: 100%; background: var(--accent); }
  .st { font-weight: 600; }
  .st.active { color: var(--accent); } .st.retrying { color: var(--warn); } .st.done { color: var(--ok); }
  .st.failed { color: var(--err); } .st.waiting, .st.connecting { color: var(--muted); }

  .empty { text-align: center; color: var(--muted); padding: 70px 20px; }
  .empty b { display: block; color: var(--text); font-size: 15px; margin-bottom: 6px; }

  /* ---------- dialogs ---------- */
  .scrim { position: fixed; inset: 0; background: rgba(10, 12, 16, .45); display: grid; place-items: center; z-index: 20; padding: 16px; }
  .dialog { background: var(--panel); border: 1px solid var(--line); border-radius: 12px; width: min(560px, 100%); padding: 18px; box-shadow: 0 12px 40px rgba(0,0,0,.25); }
  .dialog h2 { margin: 0 0 4px; font-size: 16px; }
  .dialog p { margin: 0 0 12px; color: var(--muted); }
  .dialog textarea { width: 100%; min-height: 170px; resize: vertical; font-family: ui-monospace, Consolas, monospace; font-size: 12px; }
  .dialog input[type=url] { width: 100%; }
  .dialog .foot { display: flex; gap: 8px; justify-content: flex-end; align-items: center; margin-top: 14px; flex-wrap: wrap; }
  .dialog .foot .grow { flex: 1; color: var(--muted); font-size: 12.5px; }
  .check { display: flex; gap: 8px; align-items: center; margin: 4px 0; }
  .dialog .set { margin: 0 0 16px; }
  .dialog .set p { margin: 3px 0 0; font-size: 12.5px; }
  .dialog .set .check + p { margin-left: 26px; }
  #setDlg .dialog { max-height: calc(100vh - 32px); overflow-y: auto; }
  .folders { display: grid; grid-template-columns: 1fr 1fr; gap: 6px 16px; margin: 10px 0 0 26px; }
  .folders.off { opacity: .55; }
  .folders label { display: flex; align-items: center; gap: 8px; color: var(--muted); }
  .folders label span { width: 68px; flex: none; }
  .folders input { flex: 1; min-width: 0; }
  @media (max-width: 520px) { .folders { grid-template-columns: 1fr; } }

  .toast {
    position: fixed; left: 50%; bottom: 20px; transform: translateX(-50%); z-index: 30;
    background: var(--text); color: var(--panel); padding: 8px 14px; border-radius: 8px; max-width: min(620px, calc(100% - 32px));
    box-shadow: 0 8px 24px rgba(0,0,0,.2); font-size: 13px;
  }
  .toast.err { background: var(--err); color: #fff; }
</style>
</head>
<body>

<div class="top">
  <div class="brand"><span class="dot" id="dot"></span>godm</div>
  <div class="stats" id="stats"></div>
  <div class="ctl">
    <label class="field" title="How many files download at the same time; the rest wait in line">
      Downloads at once
      <select id="limit"></select>
    </label>
    <button class="btn" id="pauseAll">Pause all</button>
    <button class="btn" id="resumeAll">Resume all</button>
    <button class="btn primary" id="addBtn">+ Add links</button>
    <button class="btn" id="setBtn">Settings</button>
  </div>
</div>

<div class="tabs" id="tabs"></div>
<main id="list"></main>

<div class="scrim" id="addDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="addTitle">
    <h2 id="addTitle">Add links</h2>
    <p>One URL per line. They join the queue in this order.</p>
    <textarea id="addText" placeholder="https://example.com/file1.zip&#10;https://example.com/file2.zip" spellcheck="false"></textarea>
    <div class="foot">
      <span class="grow" id="addCount">No links yet</span>
      <label class="field">Connections <input type="number" id="addConns" value="8" min="1" max="32" style="width:64px"></label>
      <button class="btn" data-close="addDlg">Cancel</button>
      <button class="btn primary" id="addGo" disabled>Add</button>
    </div>
  </div>
</div>

<div class="scrim" id="rmDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="rmTitle">
    <h2 id="rmTitle">Remove download</h2>
    <p id="rmName"></p>
    <label class="check"><input type="checkbox" id="rmDelete"> Also delete the file from disk</label>
    <div class="foot">
      <button class="btn" data-close="rmDlg">Cancel</button>
      <button class="btn primary" id="rmGo">Remove</button>
    </div>
  </div>
</div>

<div class="scrim" id="addrDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="addrTitle">
    <h2 id="addrTitle">Change address</h2>
    <p>Paste a fresh link to the same file. godm checks the size matches before continuing, so a wrong link cannot corrupt what is already downloaded.</p>
    <input type="url" id="addrUrl" placeholder="https://" spellcheck="false">
    <div class="foot">
      <button class="btn" data-close="addrDlg">Cancel</button>
      <button class="btn primary" id="addrGo">Continue with this link</button>
    </div>
  </div>
</div>

<div class="scrim" id="setDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="setTitle">
    <h2 id="setTitle">Settings</h2>
    <div class="set" id="awakeRow">
      <label class="check"><input type="checkbox" id="setAwake"> Keep this PC awake while downloading</label>
      <p>Stops Windows going to sleep on its own while a download is running. The screen can still turn off.</p>
    </div>
    <div class="set" id="afterRow">
      <label class="field">When all downloads finish <select id="setAfter"></select></label>
      <p>Happens once, then goes back to Do nothing, and is forgotten if godm restarts. Shut down waits 60 seconds first: press Win+R, type shutdown /a and press Enter to cancel.</p>
    </div>
    <div class="set">
      <label class="check"><input type="checkbox" id="setSort"> Sort new downloads into folders by type</label>
      <p>Inside <b id="setBase"></b>. A download that comes with a folder of its own, like one you chose in the browser, goes where you said. Types godm does not recognise stay in the main folder, and downloads already in the list stay where they are. Leave a name empty to keep that type in the main folder.</p>
      <div class="folders" id="setFolders"></div>
    </div>
    <div class="foot">
      <button class="btn" data-close="setDlg">Cancel</button>
      <button class="btn primary" id="setGo">Save</button>
    </div>
  </div>
</div>

<div class="toast" id="toast" hidden></div>

<script>
var TOKEN = "__TOKEN__";
var S = { tasks: [], limit: 3, tab: "all", open: {}, prev: {}, target: null, online: true };

function api(path, opts) {
  opts = opts || {};
  opts.headers = { "Authorization": "Bearer " + TOKEN, "Content-Type": "application/json" };
  return fetch(path, opts).then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(t || ("HTTP " + r.status)); });
    return r.json();
  });
}
function post(path, body) {
  return api(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) });
}
function q(id) { return encodeURIComponent(id); }

function human(n) {
  if (n === null || n === undefined || n < 0) return "?";
  var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : 1)) + " " + u[i];
}
function dur(s) {
  if (!isFinite(s) || s < 0) return "";
  s = Math.round(s);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m " + (s % 60) + "s";
  return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
}
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
  });
}
function extOf(name) {
  var m = /\.([a-z0-9]{1,5})$/i.exec(name || "");
  return m ? m[1].toLowerCase() : "";
}
function kindOf(ext) {
  if (/^(mp4|mkv|avi|mov|webm|flv|wmv|m4v|ts)$/.test(ext)) return "video";
  if (/^(mp3|flac|wav|aac|m4a|ogg|opus)$/.test(ext)) return "audio";
  if (/^(zip|rar|7z|gz|xz|bz2|tar|zst|tgz|iso|img|dmg)$/.test(ext)) return "archive";
  if (/^(pdf|doc|docx|xls|xlsx|ppt|pptx|epub|txt|csv)$/.test(ext)) return "doc";
  if (/^(exe|msi|apk|deb|rpm|pkg|appimage|jar|whl)$/.test(ext)) return "app";
  if (/^(jpg|jpeg|png|gif|webp|svg|bmp|heic)$/.test(ext)) return "image";
  return "other";
}
function nameOf(t) {
  if (t.filename) return t.filename;
  try { var p = new URL(t.url).pathname.split("/").pop(); return decodeURIComponent(p) || t.url; } catch (e) { return t.url; }
}
function hostOf(u) { try { return new URL(u).host; } catch (e) { return ""; } }

var LABEL = {
  queued: "Queued", running: "Downloading", paused: "Paused", done: "Finished",
  error: "Failed", needs_refresh: "Link expired", awaiting_refresh: "Waiting for new link"
};
function attention(t) { return t.state === "needs_refresh" || t.state === "awaiting_refresh" || t.state === "error"; }
function inTab(t, tab) {
  // Paused belongs with unfinished work: pausing a download must not make it
  // vanish from the view the user just clicked Pause in.
  if (tab === "active") return t.state === "running" || t.state === "queued" || t.state === "paused";
  if (tab === "done") return t.state === "done";
  if (tab === "attn") return attention(t);
  return true;
}

// Per-connection speed is derived here from successive snapshots, smoothed so
// the numbers are readable rather than jittering every poll.
function segSpeeds(t) {
  var segs = t.segments || [], now = performance.now(), p = S.prev[t.id], out = [];
  if (p && p.n === segs.length) {
    var dt = (now - p.at) / 1000;
    for (var i = 0; i < segs.length; i++) {
      var inst = dt > 0.15 ? Math.max(0, (segs[i].done - p.done[i]) / dt) : p.speed[i];
      out[i] = segs[i].state === "active" ? (p.speed[i] ? 0.5 * p.speed[i] + 0.5 * inst : inst) : 0;
    }
    if (dt <= 0.15) return p.speed;
  }
  S.prev[t.id] = { at: now, n: segs.length, done: segs.map(function (s) { return s.done; }), speed: out };
  return out;
}

function segMap(t) {
  var segs = t.segments || [];
  if (t.size <= 0 || segs.length < 2) return "";
  var html = '<div class="segmap" title="Each block is one connection and the part of the file it downloads">';
  segs.forEach(function (s) {
    var len = s.end - s.start + 1;
    var left = s.start / t.size * 100, width = len / t.size * 100;
    var pct = Math.min(100, s.done / len * 100);
    html += '<div class="seg ' + s.state + '" style="left:' + left.toFixed(3) + '%;width:' + width.toFixed(3) + '%">' +
      '<i style="width:' + pct.toFixed(1) + '%"></i></div>';
  });
  return html + "</div>";
}

function connTable(t) {
  var segs = t.segments || [];
  if (!segs.length) return '<div class="conns"><span class="url">No connection details yet.</span></div>';
  var sp = segSpeeds(t), rows = "", finished = 0, finishedBytes = 0, row = 0;
  segs.forEach(function (s, i) {
    // Ranges get split as connections free up, so finished ones are folded into
    // a single summary line instead of filling the table.
    if (s.state === "done") { finished++; finishedBytes += s.done; return; }
    var len = s.end >= 0 ? s.end - s.start + 1 : 0;
    var pct = len > 0 ? Math.min(100, s.done / len * 100) : 0;
    var range = s.end >= 0 ? human(s.start) + " – " + human(s.end + 1) : "whole stream";
    rows += "<tr><td>" + (++row) + "</td>" +
      "<td>" + range + "</td>" +
      '<td><div class="mini"><i style="width:' + pct.toFixed(1) + '%"></i></div></td>' +
      "<td>" + (len > 0 ? pct.toFixed(0) + "%" : human(s.done)) + "</td>" +
      "<td>" + (sp[i] ? human(sp[i]) + "/s" : "") + "</td>" +
      '<td class="st ' + s.state + '">' + s.state + "</td>" +
      '<td class="note">' + esc(s.note || "") + "</td></tr>";
  });
  var summary = finished
    ? '<div class="url" style="margin-bottom:4px">' + human(finishedBytes) + " already downloaded in finished ranges</div>"
    : "";
  return '<div class="conns">' + summary + '<table><thead><tr><th>#</th><th>Range</th><th></th><th>Done</th><th>Speed</th><th>State</th><th></th></tr></thead><tbody>' +
    rows + "</tbody></table></div>";
}

var CONN_CHOICES = [1, 2, 3, 4, 6, 8, 12, 16, 24, 32];
function connSelect(t) {
  var opts = CONN_CHOICES.slice();
  if (opts.indexOf(t.conns) < 0 && t.conns > 0) opts.push(t.conns);
  opts.sort(function (a, b) { return a - b; });
  return '<label class="field connsel" title="Connections for this download. Changing it takes effect immediately.">Connections ' +
    '<select data-conns="' + t.id + '">' + opts.map(function (n) {
      return '<option value="' + n + '"' + (n === t.conns ? " selected" : "") + ">" + n + "</option>";
    }).join("") + "</select></label>";
}

function queuePosition(t) {
  var queued = S.tasks.filter(function (x) { return x.state === "queued"; })
    .sort(function (a, b) { return new Date(a.added_at) - new Date(b.added_at); });
  for (var i = 0; i < queued.length; i++) if (queued[i].id === t.id) return i + 1;
  return 0;
}

// A segment stream has no real total size until the last segment lands: what
// is shown as the size is a projection that moves as the download runs. The
// share of segments written is the number that only goes one way.
function progressOf(t) {
  if (t.kind === "hls" && t.segments_total > 0) {
    return Math.min(100, t.segments_done / t.segments_total * 100);
  }
  return t.size > 0 ? Math.min(100, t.received / t.size * 100) : 0;
}

function metaLine(t) {
  var bits = [];
  var hasSize = t.size > 0;
  var pct = progressOf(t);
  switch (t.state) {
    case "running":
      if (t.kind === "hls") {
        bits.push("<b>" + t.segments_done + "</b> / " + t.segments_total + " segments · " + pct.toFixed(1) + "%");
        bits.push("<b>" + human(t.received) + "</b>" + (hasSize ? " of about " + human(t.size) : ""));
      } else {
        bits.push("<b>" + human(t.received) + "</b>" + (hasSize ? " / " + human(t.size) + " · " + pct.toFixed(1) + "%" : ""));
      }
      bits.push("<b>" + human(t.speed) + "/s</b>");
      if (hasSize && t.speed > 0) bits.push(dur((t.size - t.received) / t.speed) + " left");
      if (t.resumable) bits.push("<b>" + t.active + "</b> of " + t.conns + " connections active");
      if (t.quality) bits.push(esc(t.quality));
      // A merged video arrives as two streams, so say which one is running or
      // the bar looks like it started over for no reason.
      if (t.stage === "video") bits.push("video track");
      else if (t.stage === "audio") bits.push("audio track");
      else if (t.size !== -1) bits.push("single connection · server cannot resume");
      break;
    case "queued":
      var pos = queuePosition(t);
      bits.push(pos ? "#" + pos + " in line" : "starting");
      if (t.received > 0) bits.push(human(t.received) + " already downloaded");
      break;
    case "done":
      bits.push("<b>" + human(t.size) + "</b>");
      if (t.started_at && t.ended_at) {
        var secs = (new Date(t.ended_at) - new Date(t.started_at)) / 1000;
        if (secs > 0) bits.push("finished in " + dur(secs) + " · avg " + human(t.size / secs) + "/s");
      }
      break;
    default:
      if (t.received > 0) bits.push("<b>" + human(t.received) + "</b>" + (hasSize ? " / " + human(t.size) + " · " + pct.toFixed(1) + "% saved" : " saved"));
  }
  return bits.map(function (b) { return "<span>" + b + "</span>"; }).join("");
}

function banner(t) {
  var pct = t.size > 0 ? Math.round(t.received / t.size * 100) + "%" : "your progress";
  if (t.state === "needs_refresh") {
    var from = t.referrer ? hostOf(t.referrer) : "";
    return '<div class="banner warn"><div class="msg"><b>The download link has expired.</b> ' + esc(pct) + " is saved and will be kept." +
      '<span class="hint">' + (from
        ? "Refresh opens " + esc(from) + ". Click the same download link there and godm continues from where it stopped."
        : "Paste a new link to the same file with Change address.") +
      (t.error ? " (" + esc(t.error.replace(/^download link expired:\s*/i, "")) + ")" : "") + "</span></div>" +
      (t.referrer ? '<button class="btn warn small" data-act="refresh" data-id="' + t.id + '">Refresh link</button>' : "") +
      '<button class="btn small" data-act="address" data-id="' + t.id + '">Change address</button></div>';
  }
  if (t.state === "awaiting_refresh") {
    var left = t.refresh_until ? (new Date(t.refresh_until) - Date.now()) / 1000 : 0;
    return '<div class="banner warn"><div class="msg"><b>Waiting for the new link…</b> Click the download link again in your browser.' +
      '<span class="hint">godm will match it by file size and continue this download instead of starting a new one.' +
      (left > 0 ? " Stops waiting in " + dur(left) + "." : "") + "</span></div>" +
      '<button class="btn small" data-act="pause" data-id="' + t.id + '">Stop waiting</button></div>';
  }
  if (t.state === "error") {
    return '<div class="banner err"><div class="msg"><b>Download failed.</b> <span class="hint">' + esc(t.error || "Unknown error") + "</span></div>" +
      '<button class="btn small" data-act="resume" data-id="' + t.id + '">Retry</button></div>';
  }
  return "";
}

// playable is true for a video that can be watched now: a finished one, or an
// ordinary file whose size is known, which the player can read while the rest
// arrives. Streams and yt-dlp downloads only become one file at the end.
function playable(t, ext) {
  if (kindOf(ext) !== "video" && kindOf(ext) !== "audio") return false;
  if (t.state === "done") return !!t.path;
  if (t.kind || !(t.size > 0) || t.resumable === false) return false;
  return t.state === "running" || t.state === "queued" || t.state === "paused" || t.state === "error";
}

function card(t) {
  var name = nameOf(t), ext = extOf(name), open = !!S.open[t.id];
  var hasSize = t.size > 0;
  var pct = t.state === "done" ? 100 : progressOf(t);
  var acts = [];
  if (playable(t, ext)) acts.push(["play", t.state === "done" ? "Play" : "Play now"]);
  if (t.state === "running" || t.state === "queued") acts.push(["pause", "Pause"]);
  if (t.state === "paused") acts.push(["resume", "Resume"]);
  if (t.path) acts.push(["open", t.state === "done" ? "Show in folder" : "Open folder"]);
  if (t.state !== "done" && (t.segments || []).length) acts.push(["conns", open ? "Hide connections" : "Connections"]);
  if (t.state === "paused" || t.state === "error") acts.push(["address", "Change address"]);
  acts.push(["remove", "Remove"]);

  return '<div class="task" data-task="' + t.id + '">' +
    '<div class="ico ' + kindOf(ext) + '">' + esc(ext || "file") + "</div>" +
    '<div class="body">' +
      '<div class="row"><span class="name" title="' + esc(name) + '">' + esc(name) + "</span>" +
      '<span class="pill ' + t.state + '">' + (LABEL[t.state] || t.state) + "</span></div>" +
      (t.state === "done" ? "" : '<div class="bar"><div class="fill ' + t.state + '" style="width:' + pct.toFixed(1) + '%"></div></div>') +
      (t.state === "done" ? "" : segMap(t)) +
      '<div class="meta">' + metaLine(t) + "</div>" +
      '<div class="url" title="' + esc(t.url) + '">' + esc(t.url) + "</div>" +
      banner(t) +
      '<div class="actions">' + acts.map(function (a) {
        return '<button class="btn ghost small' + (a[0] === "remove" ? " danger" : "") + '" data-act="' + a[0] + '" data-id="' + t.id + '">' + a[1] + "</button>";
      }).join("") +
      (t.state !== "done" && t.resumable !== false ? '<span style="flex:1"></span>' + connSelect(t) : "") +
      "</div>" +
      (open ? connTable(t) : "") +
    "</div></div>";
}

function render() {
  var tasks = S.tasks, running = 0, speed = 0, queued = 0;
  tasks.forEach(function (t) {
    if (t.state === "running") { running++; speed += t.speed || 0; }
    if (t.state === "queued") queued++;
  });
  var line = running
    ? "<b>" + running + "</b> downloading · <b>" + human(speed) + "/s</b>" + (queued ? " · " + queued + " waiting" : "")
    : tasks.length ? (queued ? queued + " waiting" : "Idle") : "";
  // A one-shot action armed and forgotten is the one that would surprise.
  var armed = { sleep: "will sleep when finished", shutdown: "will shut down when finished" }[S.afterAll];
  document.getElementById("stats").innerHTML = line + (armed ? (line ? " · " : "") + "<b>" + armed + "</b>" : "");

  var c = { all: tasks.length, active: 0, done: 0, attn: 0 };
  tasks.forEach(function (t) {
    if (inTab(t, "active")) c.active++;
    if (inTab(t, "done")) c.done++;
    if (inTab(t, "attn")) c.attn++;
  });
  var tabs = [["all", "All"], ["active", "Unfinished"], ["done", "Finished"], ["attn", "Needs attention"]];
  document.getElementById("tabs").innerHTML = tabs.map(function (x) {
    return '<button class="tab' + (S.tab === x[0] ? " on" : "") + (x[0] === "attn" && c.attn ? " attn" : "") +
      '" data-tab="' + x[0] + '">' + x[1] + '<span class="n">' + c[x[0]] + "</span></button>";
  }).join("");

  var shown = tasks.filter(function (t) { return inTab(t, S.tab); });
  var list = document.getElementById("list");
  // Re-rendering would close a dropdown the user is in the middle of choosing from.
  var a = document.activeElement;
  if (a && a.tagName === "SELECT" && list.contains(a)) return;
  if (!shown.length) {
    list.innerHTML = tasks.length
      ? '<div class="empty"><b>Nothing here</b>No downloads in this view.</div>'
      : '<div class="empty"><b>No downloads yet</b>Downloads you start in Chrome appear here automatically.<br>' +
        "You can also press Ctrl+V anywhere in this window to paste links, or use Add links.</div>";
  } else {
    list.innerHTML = shown.map(card).join("");
  }
}

function poll() {
  return api("/api/tasks").then(function (d) {
    S.online = true;
    document.getElementById("dot").classList.remove("off");
    S.tasks = d.tasks || [];
    S.afterAll = d.after_all;
    if (d.limit && d.limit !== S.limit) { S.limit = d.limit; document.getElementById("limit").value = d.limit; }
    render();
  }).catch(function () {
    S.online = false;
    document.getElementById("dot").classList.add("off");
    document.getElementById("stats").textContent = "Not connected to the godm service";
  });
}

var toastTimer;
function toast(msg, kind) {
  var el = document.getElementById("toast");
  el.textContent = msg;
  el.className = "toast" + (kind === "err" ? " err" : "");
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(function () { el.hidden = true; }, kind === "err" ? 6000 : 4000);
}
function fail(e) { toast(e && e.message ? e.message : String(e), "err"); }

function show(id) { document.getElementById(id).hidden = false; }
function hide(id) { document.getElementById(id).hidden = true; }
document.querySelectorAll("[data-close]").forEach(function (b) {
  b.addEventListener("click", function () { hide(b.getAttribute("data-close")); });
});
document.querySelectorAll(".scrim").forEach(function (s) {
  s.addEventListener("click", function (e) { if (e.target === s) s.hidden = true; });
});
document.addEventListener("keydown", function (e) {
  if (e.key === "Escape") document.querySelectorAll(".scrim").forEach(function (s) { s.hidden = true; });
});

// ---------- task actions ----------
document.getElementById("list").addEventListener("click", function (e) {
  var b = e.target.closest("[data-act]");
  if (!b) return;
  var id = b.getAttribute("data-id"), act = b.getAttribute("data-act");
  var t = S.tasks.filter(function (x) { return x.id === id; })[0];
  if (act === "play") {
    post("/api/play?id=" + q(id)).then(function (r) {
      if (!r.ok) return toast(r.error, "err");
      if (t && t.state !== "done") toast("Opening the player — the download follows where you watch.");
      poll();
    }).catch(fail);
  }
  else if (act === "pause") post("/api/pause?id=" + q(id)).then(poll).catch(fail);
  else if (act === "resume") post("/api/resume?id=" + q(id)).then(poll).catch(fail);
  else if (act === "open") post("/api/open?id=" + q(id)).then(function (r) { if (!r.ok) toast(r.error, "err"); }).catch(fail);
  else if (act === "conns") { S.open[id] = !S.open[id]; render(); }
  else if (act === "refresh") {
    post("/api/refresh?id=" + q(id)).then(function (r) {
      if (!r.ok) return toast(r.error, "err");
      toast(r.referrer ? "Opened " + hostOf(r.referrer) + " — click the same download link there." : "Waiting — click the download link again in your browser.");
      poll();
    }).catch(fail);
  } else if (act === "address") {
    S.target = id;
    document.getElementById("addrUrl").value = "";
    show("addrDlg");
    document.getElementById("addrUrl").focus();
  } else if (act === "remove") {
    S.target = id;
    document.getElementById("rmName").textContent = t ? nameOf(t) : "";
    var del = document.getElementById("rmDelete");
    del.checked = false;
    del.parentElement.hidden = !(t && t.path);
    show("rmDlg");
  }
});

document.getElementById("list").addEventListener("change", function (e) {
  var sel = e.target.closest("select[data-conns]");
  if (!sel) return;
  var id = sel.getAttribute("data-conns"), n = parseInt(sel.value, 10);
  sel.blur();
  post("/api/connections?id=" + q(id) + "&n=" + n).then(function (r) {
    if (!r.ok) return toast(r.error, "err");
    toast("Using up to " + r.connections + " connection" + (r.connections === 1 ? "" : "s") + " for this download.");
    poll();
  }).catch(fail);
});

document.getElementById("tabs").addEventListener("click", function (e) {
  var b = e.target.closest("[data-tab]");
  if (b) { S.tab = b.getAttribute("data-tab"); render(); }
});

document.getElementById("rmGo").addEventListener("click", function () {
  var del = document.getElementById("rmDelete").checked ? "&delete=1" : "";
  hide("rmDlg");
  post("/api/remove?id=" + q(S.target) + del).then(poll).catch(fail);
});

document.getElementById("addrGo").addEventListener("click", function () {
  var url = document.getElementById("addrUrl").value.trim();
  if (!/^https?:\/\//i.test(url)) return toast("That does not look like an http(s) link.", "err");
  hide("addrDlg");
  post("/api/address?id=" + q(S.target), { url: url }).then(function (r) {
    if (!r.ok) return toast(r.error, "err");
    toast("Checking the new link…");
    poll();
  }).catch(fail);
});

// ---------- queue controls ----------
(function () {
  var sel = document.getElementById("limit");
  for (var i = 1; i <= 8; i++) sel.insertAdjacentHTML("beforeend", '<option value="' + i + '">' + i + "</option>");
  sel.value = S.limit;
  sel.addEventListener("change", function () {
    post("/api/limit?n=" + sel.value).then(function (r) { S.limit = r.limit; poll(); }).catch(fail);
  });
})();
document.getElementById("pauseAll").addEventListener("click", function () { post("/api/pause-all").then(poll).catch(fail); });
document.getElementById("resumeAll").addEventListener("click", function () { post("/api/resume-all").then(poll).catch(fail); });

// ---------- add links ----------
function parseLinks(text) {
  var seen = {}, out = [];
  (text || "").split(/[\s]+/).forEach(function (s) {
    s = s.trim();
    if (/^https?:\/\/\S+$/i.test(s) && !seen[s]) { seen[s] = 1; out.push(s); }
  });
  return out;
}
function updateAddCount() {
  var n = parseLinks(document.getElementById("addText").value).length;
  document.getElementById("addCount").textContent = n ? n + " link" + (n === 1 ? "" : "s") : "No links yet";
  var go = document.getElementById("addGo");
  go.disabled = !n;
  go.textContent = n > 1 ? "Add " + n + " downloads" : "Add";
}
function openAdd(prefill) {
  var ta = document.getElementById("addText");
  if (prefill) ta.value = (ta.value && !document.getElementById("addDlg").hidden ? ta.value + "\n" : "") + prefill;
  show("addDlg");
  updateAddCount();
  ta.focus();
}
document.getElementById("addBtn").addEventListener("click", function () {
  document.getElementById("addText").value = "";
  openAdd("");
});
document.getElementById("addText").addEventListener("input", updateAddCount);
document.getElementById("addGo").addEventListener("click", function () {
  var links = parseLinks(document.getElementById("addText").value);
  var conns = parseInt(document.getElementById("addConns").value, 10) || 8;
  if (!links.length) return;
  hide("addDlg");
  post("/api/batch", { items: links.map(function (u) { return { url: u, connections: conns }; }) }).then(function (r) {
    var n = (r.ids || []).length;
    toast("Added " + n + " download" + (n === 1 ? "" : "s") + ((r.errors || []).length ? " (" + r.errors.length + " rejected)" : ""));
    document.getElementById("addText").value = "";
    S.tab = "all";
    poll();
  }).catch(fail);
});

// ---------- settings ----------
// Only what the user changed is sent. A setting that changed on its own while
// the dialog was open (a one-shot action that has already fired) must not be
// put back by pressing Save.
var AFTER_LABEL = { nothing: "Do nothing", sleep: "Sleep", shutdown: "Shut down" };
var AFTER_DOES = { sleep: "put this PC to sleep", shutdown: "shut this PC down" };
// The kinds match what the daemon sorts by; the names are only what to call them.
var FOLDER_KINDS = [["video", "Video"], ["audio", "Music"], ["archive", "Archives"], ["doc", "Documents"], ["app", "Programs"], ["image", "Images"]];
function folderInputs() { return document.querySelectorAll("#setFolders input"); }
function dimFolders() { document.getElementById("setFolders").classList.toggle("off", !document.getElementById("setSort").checked); }
document.getElementById("setFolders").innerHTML = FOLDER_KINDS.map(function (k) {
  return '<label><span>' + k[1] + '</span><input type="text" data-folder="' + k[0] + '" maxlength="100" placeholder="main folder" spellcheck="false"></label>';
}).join("");
document.getElementById("setSort").addEventListener("change", dimFolders);

function openSettings() {
  api("/api/settings").then(function (d) {
    S.settings = d;
    document.getElementById("setAwake").checked = !!d.keep_awake;
    document.getElementById("awakeRow").hidden = !d.can_keep_awake;
    var opts = d.after_all_options || [], sel = document.getElementById("setAfter");
    sel.innerHTML = opts.map(function (o) { return '<option value="' + o + '">' + (AFTER_LABEL[o] || o) + "</option>"; }).join("");
    sel.value = d.after_all;
    document.getElementById("afterRow").hidden = opts.length < 2;
    document.getElementById("setSort").checked = !!d.sort_by_type;
    document.getElementById("setBase").textContent = d.out_dir || "";
    folderInputs().forEach(function (i) { i.value = (d.folders || {})[i.getAttribute("data-folder")] || ""; });
    dimFolders();
    show("setDlg");
  }).catch(fail);
}
document.getElementById("setBtn").addEventListener("click", openSettings);
document.getElementById("setGo").addEventListener("click", function () {
  var was = S.settings, upd = {};
  var awake = document.getElementById("setAwake").checked;
  if (awake !== !!was.keep_awake) upd.keep_awake = awake;
  var after = document.getElementById("setAfter").value;
  if (after && after !== was.after_all) upd.after_all = after;
  var sort = document.getElementById("setSort").checked;
  if (sort !== !!was.sort_by_type) upd.sort_by_type = sort;
  var names = {}, renamed = false;
  folderInputs().forEach(function (i) {
    var k = i.getAttribute("data-folder");
    if (i.value.trim() !== ((was.folders || {})[k] || "")) { names[k] = i.value; renamed = true; }
  });
  if (renamed) upd.folders = names;
  if (!Object.keys(upd).length) return hide("setDlg");
  // The dialog stays open if the daemon refuses, so a typo does not cost the rest.
  post("/api/settings", upd).then(function (r) {
    hide("setDlg");
    toast(upd.after_all && AFTER_DOES[r.after_all]
      ? "godm will " + AFTER_DOES[r.after_all] + " when all downloads finish."
      : "Settings saved.");
    poll();
  }).catch(fail);
});

// Pasting links anywhere outside a text field opens the add dialog with them.
document.addEventListener("paste", function (e) {
  var a = document.activeElement;
  if (a && (a.tagName === "INPUT" || a.tagName === "TEXTAREA")) return;
  var text = (e.clipboardData || window.clipboardData).getData("text");
  if (parseLinks(text).length) { e.preventDefault(); openAdd(text); }
});

poll();
setInterval(poll, 700);
</script>
</body>
</html>
`

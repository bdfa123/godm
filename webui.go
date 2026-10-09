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
    <label class="field" data-i18n-title="top.limit.tip">
      <span data-i18n="top.limit"></span>
      <select id="limit"></select>
    </label>
    <label class="field" data-i18n-title="top.speed.tip">
      <span data-i18n="top.speed"></span>
      <select id="speed"></select>
    </label>
    <button class="btn" id="pauseAll" data-i18n="top.pauseAll"></button>
    <button class="btn" id="resumeAll" data-i18n="top.resumeAll"></button>
    <button class="btn primary" id="addBtn" data-i18n="top.add"></button>
    <button class="btn" id="setBtn" data-i18n="top.settings"></button>
  </div>
</div>

<div class="tabs" id="tabs"></div>
<main id="list"></main>

<div class="scrim" id="addDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="addTitle">
    <h2 id="addTitle" data-i18n="add.title"></h2>
    <p data-i18n="add.help"></p>
    <textarea id="addText" placeholder="https://example.com/file1.zip&#10;magnet:?xt=urn:btih:..." spellcheck="false"></textarea>
    <div class="foot">
      <span class="grow" id="addCount"></span>
      <button class="btn" id="torrentPick" data-i18n="add.torrent" data-i18n-title="add.torrent.tip"></button>
      <input type="file" id="torrentFile" accept=".torrent,application/x-bittorrent" multiple hidden>
      <label class="field"><span data-i18n="add.conns"></span> <input type="number" id="addConns" value="8" min="1" max="32" style="width:64px"></label>
      <button class="btn" data-close="addDlg" data-i18n="common.cancel"></button>
      <button class="btn primary" id="addGo" disabled></button>
    </div>
  </div>
</div>

<div class="scrim" id="rmDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="rmTitle">
    <h2 id="rmTitle" data-i18n="rm.title"></h2>
    <p id="rmName"></p>
    <label class="check"><input type="checkbox" id="rmDelete"> <span data-i18n="rm.delete"></span></label>
    <div class="foot">
      <button class="btn" data-close="rmDlg" data-i18n="common.cancel"></button>
      <button class="btn primary" id="rmGo" data-i18n="rm.go"></button>
    </div>
  </div>
</div>

<div class="scrim" id="addrDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="addrTitle">
    <h2 id="addrTitle" data-i18n="addr.title"></h2>
    <p data-i18n="addr.help"></p>
    <input type="url" id="addrUrl" placeholder="https://" spellcheck="false">
    <div class="foot">
      <button class="btn" data-close="addrDlg" data-i18n="common.cancel"></button>
      <button class="btn primary" id="addrGo" data-i18n="addr.go"></button>
    </div>
  </div>
</div>

<div class="scrim" id="setDlg" hidden>
  <div class="dialog" role="dialog" aria-labelledby="setTitle">
    <h2 id="setTitle" data-i18n="set.title"></h2>
    <div class="set">
      <label class="field"><span data-i18n="set.lang"></span> <select id="setLang"></select></label>
      <p data-i18n="set.lang.help"></p>
    </div>
    <div class="set" id="awakeRow">
      <label class="check"><input type="checkbox" id="setAwake"> <span data-i18n="set.awake"></span></label>
      <p data-i18n="set.awake.help"></p>
    </div>
    <div class="set" id="afterRow">
      <label class="field"><span data-i18n="set.after"></span> <select id="setAfter"></select></label>
      <p data-i18n="set.after.help"></p>
    </div>
    <div class="set">
      <label class="check"><input type="checkbox" id="setSort"> <span data-i18n="set.sort"></span></label>
      <p id="setSortHelp"></p>
      <div class="folders" id="setFolders"></div>
    </div>
    <div class="foot">
      <button class="btn" data-close="setDlg" data-i18n="common.cancel"></button>
      <button class="btn primary" id="setGo" data-i18n="common.save"></button>
    </div>
  </div>
</div>

<div class="toast" id="toast" hidden></div>

<script>
var TOKEN = "__TOKEN__";
// The Language setting as godm has it: auto, en or zh. The page works out what
// auto means for this browser; the tray and the notifications do the same for
// Windows.
var LANG_SETTING = "__LANG__";
var LANG = "en";
var S = { tasks: [], limit: 3, speed: 0, tab: "all", open: {}, prev: {}, target: null, online: true, langAt: 0 };

// ---------- words ----------
// Everything the page says is looked up here by key. English is what the page
// is written in and the fallback for a key another language lacks. To add a
// string: one line in I18N.en, one in I18N.zh, then tr(key) in script or a
// data-i18n attribute on an element (data-i18n-title and data-i18n-ph do the
// title and the placeholder). {name} is filled in by the caller. A count that
// changes the wording has a .one and an .other key and is asked for with
// trn(key, n). A value is HTML where it is put into markup, so a name that goes
// into one is passed through esc() first; one used by data-i18n or in a toast
// is plain text. i18n_test.go checks that the two tables have the same keys and
// that every key the page uses is there.
var I18N = {};
I18N.en = {
  "top.limit": "Downloads at once",
  "top.limit.tip": "How many files download at the same time; the rest wait in line",
  "top.speed": "Speed limit",
  "top.speed.tip": "The most all downloads together may use. Each download can be limited on its own as well.",
  "top.pauseAll": "Pause all",
  "top.resumeAll": "Resume all",
  "top.add": "+ Add links",
  "top.settings": "Settings",
  "common.cancel": "Cancel",
  "common.save": "Save",

  "stats.running": "<b>{n}</b> downloading · <b>{speed}</b>",
  "stats.waiting": "{n} waiting",
  "stats.idle": "Idle",
  "stats.sleep": "will sleep when finished",
  "stats.shutdown": "will shut down when finished",
  "net.offline": "Not connected to the godm service",

  "tab.all": "All",
  "tab.active": "Unfinished",
  "tab.done": "Finished",
  "tab.attn": "Needs attention",
  "empty.view.title": "Nothing here",
  "empty.view.text": "No downloads in this view.",
  "empty.first.title": "No downloads yet",
  "empty.first.text": "Downloads you start in Chrome appear here automatically.<br>You can also press Ctrl+V anywhere in this window to paste links, or use Add links.",

  "state.queued": "Queued",
  "state.running": "Downloading",
  "state.paused": "Paused",
  "state.done": "Finished",
  "state.error": "Failed",
  "state.needs_refresh": "Link expired",
  "state.awaiting_refresh": "Waiting for new link",
  "state.metadata": "Fetching metadata",
  "state.seeding": "Seeding",
  "ico.file": "file",

  "meta.segments": "<b>{done}</b> / {total} segments · {pct}%",
  "meta.aboutSize": "<b>{received}</b> of about {size}",
  "meta.sizeOf": "<b>{received}</b> / {size} · {pct}%",
  "meta.left": "{time} left",
  "meta.active": "<b>{active}</b> of {total} connections active",
  "meta.single": "single connection · server cannot resume",
  "meta.videoTrack": "video track",
  "meta.audioTrack": "audio track",
  "meta.inLine": "#{n} in line",
  "meta.starting": "starting",
  "meta.kept": "{size} already downloaded",
  "meta.finishedIn": "finished in {time} · avg {speed}",
  "meta.saved": "<b>{received}</b> / {size} · {pct}% saved",
  "meta.savedNoSize": "<b>{received}</b> saved",
  "bt.peers.one": "<b>{n}</b> peer",
  "bt.peers.other": "<b>{n}</b> peers",
  "bt.seeds.one": "{n} seed",
  "bt.seeds.other": "{n} seeds",
  "bt.askingPeers": "Asking peers for the file list…",
  "bt.uploaded": "{size} uploaded",
  "bt.uploadedRatio": "{size} uploaded · ratio {ratio}",

  "banner.expired.pct": "<b>The download link has expired.</b> {pct} is saved and will be kept.",
  "banner.expired.noPct": "<b>The download link has expired.</b> Your progress is saved and will be kept.",
  "banner.expired.refresh": "Refresh opens {host}. Click the same download link there and godm continues from where it stopped.",
  "banner.expired.paste": "Paste a new link to the same file with Change address.",
  "banner.awaiting": "<b>Waiting for the new link…</b> Click the download link again in your browser.",
  "banner.awaiting.hint": "godm will match it by file size and continue this download instead of starting a new one.",
  "banner.awaiting.hintTime": "godm will match it by file size and continue this download instead of starting a new one. Stops waiting in {time}.",
  "banner.failed": "<b>Download failed.</b>",
  "banner.unknown": "Unknown error",

  "act.play": "Play",
  "act.playNow": "Play now",
  "act.pause": "Pause",
  "act.resume": "Resume",
  "act.showFolder": "Show in folder",
  "act.openFolder": "Open folder",
  "act.conns": "Connections",
  "act.connsHide": "Hide connections",
  "act.address": "Change address",
  "act.remove": "Remove",
  "act.refresh": "Refresh link",
  "act.stopWaiting": "Stop waiting",
  "act.retry": "Retry",

  "task.conns": "Connections",
  "task.conns.tip": "Connections for this download. Changing it takes effect immediately.",
  "task.speed": "Speed",
  "task.speed.tip": "Speed limit for this download, on top of the overall one. Changing it takes effect immediately.",
  "speed.unlimited": "Unlimited",

  "conn.none": "No connection details yet.",
  "conn.map.tip": "Each block is one connection and the part of the file it downloads",
  "conn.th.range": "Range",
  "conn.th.done": "Done",
  "conn.th.speed": "Speed",
  "conn.th.state": "State",
  "conn.whole": "whole stream",
  "conn.finished": "{size} already downloaded in finished ranges",
  "seg.waiting": "waiting",
  "seg.connecting": "connecting",
  "seg.active": "active",
  "seg.retrying": "retrying",
  "seg.done": "done",
  "seg.failed": "failed",

  "toast.player": "Opening the player — the download follows where you watch.",
  "toast.refresh.opened": "Opened {host} — click the same download link there.",
  "toast.refresh.waiting": "Waiting — click the download link again in your browser.",
  "toast.conns.one": "Using up to {n} connection for this download.",
  "toast.conns.other": "Using up to {n} connections for this download.",
  "toast.speed.task": "This download is limited to {speed}.",
  "toast.speed.taskNone": "This download has no limit of its own.",
  "toast.speed.all": "All downloads together are limited to {speed}.",
  "toast.speed.allNone": "Downloads are no longer speed limited.",
  "toast.address.bad": "That does not look like an http(s) link.",
  "toast.address.check": "Checking the new link…",
  "toast.added.one": "Added {n} download",
  "toast.added.other": "Added {n} downloads",
  "toast.torrent.one": "Added {n} torrent",
  "toast.torrent.other": "Added {n} torrents",
  "toast.torrent.some": "Added {n}.",
  "toast.rejected": "({n} rejected)",
  "toast.saved": "Settings saved.",
  "toast.after.sleep": "godm will put this PC to sleep when all downloads finish.",
  "toast.after.shutdown": "godm will shut this PC down when all downloads finish.",

  "add.title": "Add links",
  "add.help": "One URL or magnet link per line. They join the queue in this order.",
  "add.count.none": "No links yet",
  "add.count.one": "{n} link",
  "add.count.other": "{n} links",
  "add.torrent": "Open .torrent…",
  "add.torrent.tip": "Add torrents from .torrent files on this computer",
  "add.conns": "Connections",
  "add.go": "Add",
  "add.goMany": "Add {n} downloads",
  "rm.title": "Remove download",
  "rm.delete": "Also delete the file from disk",
  "rm.go": "Remove",
  "addr.title": "Change address",
  "addr.help": "Paste a fresh link to the same file. godm checks the size matches before continuing, so a wrong link cannot corrupt what is already downloaded.",
  "addr.go": "Continue with this link",

  "set.title": "Settings",
  "set.lang": "Language",
  "set.lang.auto": "Auto (follow the system)",
  "set.lang.help": "Applies to this window, the tray menu and the notifications.",
  "set.awake": "Keep this PC awake while downloading",
  "set.awake.help": "Stops Windows going to sleep on its own while a download is running. The screen can still turn off.",
  "set.after": "When all downloads finish",
  "set.after.help": "Happens once, then goes back to Do nothing, and is forgotten if godm restarts. Shut down waits 60 seconds first: press Win+R, type shutdown /a and press Enter to cancel.",
  "after.nothing": "Do nothing",
  "after.sleep": "Sleep",
  "after.shutdown": "Shut down",
  "set.sort": "Sort new downloads into folders by type",
  "set.sort.help": "Inside {dir}. A download that comes with a folder of its own, like one you chose in the browser, goes where you said. Types godm does not recognise stay in the main folder, and downloads already in the list stay where they are. Leave a name empty to keep that type in the main folder.",
  "set.folder.ph": "main folder",
  "kind.video": "Video",
  "kind.audio": "Music",
  "kind.archive": "Archives",
  "kind.doc": "Documents",
  "kind.app": "Programs",
  "kind.image": "Images",

  "time.s": "{s}s",
  "time.ms": "{m}m {s}s",
  "time.hm": "{h}h {m}m"
};
I18N.zh = {
  "top.limit": "同时下载数",
  "top.limit.tip": "同时下载的任务数量，其余任务排队等待",
  "top.speed": "限速",
  "top.speed.tip": "所有任务合计最多可使用的速度。每个任务也可以单独限速。",
  "top.pauseAll": "全部暂停",
  "top.resumeAll": "全部继续",
  "top.add": "+ 添加链接",
  "top.settings": "设置",
  "common.cancel": "取消",
  "common.save": "保存",

  "stats.running": "<b>{n}</b> 个任务下载中 · <b>{speed}</b>",
  "stats.waiting": "{n} 个任务等待中",
  "stats.idle": "空闲",
  "stats.sleep": "完成后将进入睡眠",
  "stats.shutdown": "完成后将关机",
  "net.offline": "未连接到 godm 服务",

  "tab.all": "全部",
  "tab.active": "未完成",
  "tab.done": "已完成",
  "tab.attn": "需要处理",
  "empty.view.title": "暂无内容",
  "empty.view.text": "此分类下没有任务。",
  "empty.first.title": "还没有下载任务",
  "empty.first.text": "在 Chrome 中开始的下载会自动显示在这里。<br>您也可以在此窗口任意位置按 Ctrl+V 粘贴链接，或使用“添加链接”。",

  "state.queued": "排队中",
  "state.running": "下载中",
  "state.paused": "已暂停",
  "state.done": "已完成",
  "state.error": "失败",
  "state.needs_refresh": "链接已过期",
  "state.awaiting_refresh": "等待新链接",
  "state.metadata": "正在获取元数据",
  "state.seeding": "做种中",
  "ico.file": "文件",

  "meta.segments": "<b>{done}</b> / {total} 分片 · {pct}%",
  "meta.aboutSize": "<b>{received}</b> / 约 {size}",
  "meta.sizeOf": "<b>{received}</b> / {size} · {pct}%",
  "meta.left": "剩余 {time}",
  "meta.active": "<b>{active}</b> / {total} 个连接活动中",
  "meta.single": "单连接 · 服务器不支持断点续传",
  "meta.videoTrack": "视频轨道",
  "meta.audioTrack": "音频轨道",
  "meta.inLine": "排队第 {n} 位",
  "meta.starting": "正在启动",
  "meta.kept": "已下载 {size}",
  "meta.finishedIn": "耗时 {time} · 平均 {speed}",
  "meta.saved": "<b>{received}</b> / {size} · 已保存 {pct}%",
  "meta.savedNoSize": "已保存 <b>{received}</b>",
  "bt.peers.one": "<b>{n}</b> 个节点",
  "bt.peers.other": "<b>{n}</b> 个节点",
  "bt.seeds.one": "{n} 个做种者",
  "bt.seeds.other": "{n} 个做种者",
  "bt.askingPeers": "正在向节点请求文件列表…",
  "bt.uploaded": "已上传 {size}",
  "bt.uploadedRatio": "已上传 {size} · 分享率 {ratio}",

  "banner.expired.pct": "<b>下载链接已过期。</b>已保存 {pct}，将予以保留。",
  "banner.expired.noPct": "<b>下载链接已过期。</b>下载进度已保存，将予以保留。",
  "banner.expired.refresh": "刷新链接会打开 {host}。请在那里再点一次同一个下载链接，godm 会从中断处继续。",
  "banner.expired.paste": "使用“更改地址”粘贴同一文件的新链接。",
  "banner.awaiting": "<b>正在等待新链接…</b>请在浏览器中再次点击下载链接。",
  "banner.awaiting.hint": "godm 将根据文件大小匹配并继续此任务，而不是开始新任务。",
  "banner.awaiting.hintTime": "godm 将根据文件大小匹配并继续此任务，而不是开始新任务。将在 {time} 后停止等待。",
  "banner.failed": "<b>下载失败。</b>",
  "banner.unknown": "未知错误",

  "act.play": "播放",
  "act.playNow": "立即播放",
  "act.pause": "暂停",
  "act.resume": "继续",
  "act.showFolder": "在文件夹中显示",
  "act.openFolder": "打开文件夹",
  "act.conns": "查看连接",
  "act.connsHide": "隐藏连接",
  "act.address": "更改地址",
  "act.remove": "移除",
  "act.refresh": "刷新链接",
  "act.stopWaiting": "停止等待",
  "act.retry": "重试",

  "task.conns": "连接数",
  "task.conns.tip": "此任务的连接数。修改后立即生效。",
  "task.speed": "限速",
  "task.speed.tip": "此任务的限速，与总限速同时生效。修改后立即生效。",
  "speed.unlimited": "不限速",

  "conn.none": "暂无连接详情。",
  "conn.map.tip": "每个分块对应一个连接及其下载的文件部分",
  "conn.th.range": "范围",
  "conn.th.done": "已完成",
  "conn.th.speed": "速度",
  "conn.th.state": "状态",
  "conn.whole": "整个数据流",
  "conn.finished": "已完成的范围共下载 {size}",
  "seg.waiting": "等待中",
  "seg.connecting": "连接中",
  "seg.active": "传输中",
  "seg.retrying": "重试中",
  "seg.done": "已完成",
  "seg.failed": "失败",

  "toast.player": "正在打开播放器——下载将跟随您的观看进度。",
  "toast.refresh.opened": "已打开 {host}——请在那里点击相同的下载链接。",
  "toast.refresh.waiting": "等待中——请在浏览器中再次点击下载链接。",
  "toast.conns.one": "此任务最多使用 {n} 个连接。",
  "toast.conns.other": "此任务最多使用 {n} 个连接。",
  "toast.speed.task": "此任务限速为 {speed}。",
  "toast.speed.taskNone": "此任务没有单独限速。",
  "toast.speed.all": "所有任务合计限速 {speed}。",
  "toast.speed.allNone": "已取消限速。",
  "toast.address.bad": "这不是有效的 http(s) 链接。",
  "toast.address.check": "正在检查新链接…",
  "toast.added.one": "已添加 {n} 个任务",
  "toast.added.other": "已添加 {n} 个任务",
  "toast.torrent.one": "已添加 {n} 个种子",
  "toast.torrent.other": "已添加 {n} 个种子",
  "toast.torrent.some": "已添加 {n} 个。",
  "toast.rejected": "（{n} 个被拒绝）",
  "toast.saved": "设置已保存。",
  "toast.after.sleep": "所有下载完成后，godm 会让这台电脑进入睡眠。",
  "toast.after.shutdown": "所有下载完成后，godm 会让这台电脑关机。",

  "add.title": "添加链接",
  "add.help": "每行一个链接或磁力链接，将按此顺序加入下载队列。",
  "add.count.none": "暂无链接",
  "add.count.one": "{n} 个链接",
  "add.count.other": "{n} 个链接",
  "add.torrent": "打开 .torrent 文件…",
  "add.torrent.tip": "从本机 .torrent 文件添加种子",
  "add.conns": "连接数",
  "add.go": "添加",
  "add.goMany": "添加 {n} 个任务",
  "rm.title": "移除任务",
  "rm.delete": "同时从磁盘删除文件",
  "rm.go": "移除",
  "addr.title": "更改地址",
  "addr.help": "粘贴同一文件的新链接。godm 会在继续前检查大小是否匹配，因此错误的链接不会损坏已下载的内容。",
  "addr.go": "使用此链接继续",

  "set.title": "设置",
  "set.lang": "语言 / Language",
  "set.lang.auto": "自动（跟随系统）",
  "set.lang.help": "适用于此窗口、托盘菜单和通知。",
  "set.awake": "下载时保持电脑唤醒",
  "set.awake.help": "下载进行时，防止 Windows 自行进入睡眠。屏幕仍可以关闭。",
  "set.after": "所有下载完成后",
  "set.after.help": "仅执行一次，之后会恢复为“不执行任何操作”，godm 重启后也会恢复。选择“关机”时会先等待 60 秒：按 Win+R，输入 shutdown /a 并按 Enter 可取消。",
  "after.nothing": "不执行任何操作",
  "after.sleep": "睡眠",
  "after.shutdown": "关机",
  "set.sort": "按类型将新下载分到不同文件夹",
  "set.sort.help": "文件夹建在 {dir} 中。自带保存位置的下载（例如在浏览器中选好了文件夹的）仍保存到您指定的位置。godm 无法识别的类型留在默认文件夹，列表中已有的任务不会移动。某个类型的名称留空，则该类型仍放在默认文件夹。",
  "set.folder.ph": "默认文件夹",
  "kind.video": "视频",
  "kind.audio": "音乐",
  "kind.archive": "压缩包",
  "kind.doc": "文档",
  "kind.app": "程序",
  "kind.image": "图片",

  "time.s": "{s}秒",
  "time.ms": "{m}分{s}秒",
  "time.hm": "{h}小时{m}分"
};

// resolveLang turns a setting into the language to show.
function resolveLang(setting) {
  if (setting === "en" || setting === "zh") return setting;
  var l = (navigator.languages && navigator.languages[0]) || navigator.language || "";
  return /^zh/i.test(l) ? "zh" : "en";
}
function tr(key, vars) {
  var s = (I18N[LANG] || {})[key];
  if (s === undefined) s = I18N.en[key];
  if (s === undefined) return key;
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, function (m, k) { return vars[k] === undefined ? m : vars[k]; });
}
// trn picks the wording for a count; the count is {n} in the text.
function trn(key, n, vars) {
  vars = vars || {};
  vars.n = n;
  return tr(key + (n === 1 ? ".one" : ".other"), vars);
}

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
  if (s < 60) return tr("time.s", { s: s });
  if (s < 3600) return tr("time.ms", { m: Math.floor(s / 60), s: s % 60 });
  return tr("time.hm", { h: Math.floor(s / 3600), m: Math.floor((s % 3600) / 60) });
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

// The keys of the words for each state of a download and of a connection.
var LABEL = {
  queued: "state.queued", running: "state.running", paused: "state.paused", done: "state.done",
  error: "state.error", needs_refresh: "state.needs_refresh", awaiting_refresh: "state.awaiting_refresh"
};
var SEG_LABEL = {
  waiting: "seg.waiting", connecting: "seg.connecting", active: "seg.active",
  retrying: "seg.retrying", done: "seg.done", failed: "seg.failed"
};
function word(map, state) { return map[state] ? tr(map[state]) : state; }
// A torrent says more than its state: it may still be learning what files it
// has, or be finished and uploading.
function stateLabel(t) {
  if (t.kind === "bt" && t.state === "running" && t.stage === "metadata") return tr("state.metadata");
  if (t.kind === "bt" && t.state === "done" && t.stage === "seeding") return tr("state.seeding");
  return word(LABEL, t.state);
}
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
  var html = '<div class="segmap" title="' + esc(tr("conn.map.tip")) + '">';
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
  if (!segs.length) return '<div class="conns"><span class="url">' + tr("conn.none") + "</span></div>";
  var sp = segSpeeds(t), rows = "", finished = 0, finishedBytes = 0, row = 0;
  segs.forEach(function (s, i) {
    // Ranges get split as connections free up, so finished ones are folded into
    // a single summary line instead of filling the table.
    if (s.state === "done") { finished++; finishedBytes += s.done; return; }
    var len = s.end >= 0 ? s.end - s.start + 1 : 0;
    var pct = len > 0 ? Math.min(100, s.done / len * 100) : 0;
    var range = s.end >= 0 ? human(s.start) + " – " + human(s.end + 1) : tr("conn.whole");
    rows += "<tr><td>" + (++row) + "</td>" +
      "<td>" + range + "</td>" +
      '<td><div class="mini"><i style="width:' + pct.toFixed(1) + '%"></i></div></td>' +
      "<td>" + (len > 0 ? pct.toFixed(0) + "%" : human(s.done)) + "</td>" +
      "<td>" + (sp[i] ? human(sp[i]) + "/s" : "") + "</td>" +
      '<td class="st ' + s.state + '">' + esc(word(SEG_LABEL, s.state)) + "</td>" +
      '<td class="note">' + esc(s.note || "") + "</td></tr>";
  });
  var summary = finished
    ? '<div class="url" style="margin-bottom:4px">' + tr("conn.finished", { size: human(finishedBytes) }) + "</div>"
    : "";
  return '<div class="conns">' + summary + "<table><thead><tr><th>#</th><th>" + tr("conn.th.range") + "</th><th></th><th>" +
    tr("conn.th.done") + "</th><th>" + tr("conn.th.speed") + "</th><th>" + tr("conn.th.state") + "</th><th></th></tr></thead><tbody>" +
    rows + "</tbody></table></div>";
}

var CONN_CHOICES = [1, 2, 3, 4, 6, 8, 12, 16, 24, 32];
function connSelect(t) {
  var opts = CONN_CHOICES.slice();
  if (opts.indexOf(t.conns) < 0 && t.conns > 0) opts.push(t.conns);
  opts.sort(function (a, b) { return a - b; });
  return '<label class="field connsel" title="' + esc(tr("task.conns.tip")) + '">' + esc(tr("task.conns")) + " " +
    '<select data-conns="' + t.id + '">' + opts.map(function (n) {
      return '<option value="' + n + '"' + (n === t.conns ? " selected" : "") + ">" + n + "</option>";
    }).join("") + "</select></label>";
}

// Speed limits are in bytes per second; 0 means none.
var SPEED_CHOICES = [0, 512 << 10, 1 << 20, 2 << 20, 5 << 20, 10 << 20, 20 << 20, 50 << 20];
function speedLabel(n) { return n > 0 ? human(n).replace(".0 ", " ") + "/s" : tr("speed.unlimited"); }
function speedOptions(cur) {
  var opts = SPEED_CHOICES.slice();
  if (opts.indexOf(cur) < 0 && cur > 0) opts.push(cur);
  opts.sort(function (a, b) { return a - b; });
  return opts.map(function (n) {
    return '<option value="' + n + '"' + (n === cur ? " selected" : "") + ">" + speedLabel(n) + "</option>";
  }).join("");
}
function speedSelect(t) {
  return '<label class="field speedsel" title="' + esc(tr("task.speed.tip")) + '">' + esc(tr("task.speed")) + " " +
    '<select data-speed="' + t.id + '">' + speedOptions(t.speed_limit || 0) + "</select></label>";
}

// taskControls are the selects at the end of a card. yt-dlp runs its own
// transfers, so neither applies to it.
// A torrent counts peers rather than connections, and the speed limit does
// not reach the torrent client yet, so neither control would do anything.
function taskControls(t) {
  if (t.state === "done" || t.kind === "yt-dlp" || t.kind === "bt") return "";
  return '<span style="flex:1"></span>' + speedSelect(t) + (t.resumable !== false ? connSelect(t) : "");
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

// torrentMeta is the line under a torrent: peers instead of connections, and
// what it has uploaded, since that is what the seeding policy goes by.
function torrentMeta(t) {
  var bits = [], hasSize = t.size > 0, pct = progressOf(t);
  var peers = trn("bt.peers", t.active || 0) + (t.conns ? " · " + trn("bt.seeds", t.conns) : "");
  if (t.state === "running" && t.stage === "metadata") {
    bits.push(tr("bt.askingPeers"), peers);
  } else if (t.state === "running") {
    bits.push(hasSize
      ? tr("meta.sizeOf", { received: human(t.received), size: human(t.size), pct: pct.toFixed(1) })
      : "<b>" + human(t.received) + "</b>");
    bits.push("<b>" + human(t.speed) + "/s</b>");
    if (hasSize && t.speed > 0) bits.push(tr("meta.left", { time: dur((t.size - t.received) / t.speed) }));
    bits.push(peers);
  } else if (t.state === "done") {
    bits.push("<b>" + human(t.size) + "</b>");
    if (t.stage === "seeding") bits.push(peers);
  } else if (t.received > 0) {
    bits.push(hasSize
      ? tr("meta.saved", { received: human(t.received), size: human(t.size), pct: pct.toFixed(1) })
      : tr("meta.savedNoSize", { received: human(t.received) }));
  }
  if (t.uploaded > 0) {
    bits.push(hasSize
      ? tr("bt.uploadedRatio", { size: human(t.uploaded), ratio: (t.uploaded / t.size).toFixed(2) })
      : tr("bt.uploaded", { size: human(t.uploaded) }));
  }
  return bits.map(function (b) { return "<span>" + b + "</span>"; }).join("");
}

function metaLine(t) {
  if (t.kind === "bt") return torrentMeta(t);
  var bits = [];
  var hasSize = t.size > 0;
  var pct = progressOf(t);
  switch (t.state) {
    case "running":
      if (t.kind === "hls") {
        bits.push(tr("meta.segments", { done: t.segments_done, total: t.segments_total, pct: pct.toFixed(1) }));
        bits.push(hasSize ? tr("meta.aboutSize", { received: human(t.received), size: human(t.size) }) : "<b>" + human(t.received) + "</b>");
      } else {
        bits.push(hasSize
          ? tr("meta.sizeOf", { received: human(t.received), size: human(t.size), pct: pct.toFixed(1) })
          : "<b>" + human(t.received) + "</b>");
      }
      bits.push("<b>" + human(t.speed) + "/s</b>");
      if (hasSize && t.speed > 0) bits.push(tr("meta.left", { time: dur((t.size - t.received) / t.speed) }));
      // The resume note is only about plain files: yt-dlp fetches on its own
      // terms, and a stream always resumes by segment.
      if (t.resumable) bits.push(tr("meta.active", { active: t.active, total: t.conns }));
      else if (!t.kind && t.size !== -1) bits.push(tr("meta.single"));
      if (t.quality) bits.push(esc(t.quality));
      // A merged video arrives as two streams, so say which one is running or
      // the bar looks like it started over for no reason.
      if (t.stage === "video") bits.push(tr("meta.videoTrack"));
      else if (t.stage === "audio") bits.push(tr("meta.audioTrack"));
      break;
    case "queued":
      var pos = queuePosition(t);
      bits.push(pos ? tr("meta.inLine", { n: pos }) : tr("meta.starting"));
      if (t.received > 0) bits.push(tr("meta.kept", { size: human(t.received) }));
      break;
    case "done":
      bits.push("<b>" + human(t.size) + "</b>");
      if (t.started_at && t.ended_at) {
        var secs = (new Date(t.ended_at) - new Date(t.started_at)) / 1000;
        if (secs > 0) bits.push(tr("meta.finishedIn", { time: dur(secs), speed: human(t.size / secs) + "/s" }));
      }
      break;
    default:
      if (t.received > 0) {
        bits.push(hasSize
          ? tr("meta.saved", { received: human(t.received), size: human(t.size), pct: pct.toFixed(1) })
          : tr("meta.savedNoSize", { received: human(t.received) }));
      }
  }
  return bits.map(function (b) { return "<span>" + b + "</span>"; }).join("");
}

function banner(t) {
  if (t.state === "needs_refresh") {
    var from = t.referrer ? hostOf(t.referrer) : "";
    var saved = t.size > 0 ? tr("banner.expired.pct", { pct: Math.round(t.received / t.size * 100) + "%" }) : tr("banner.expired.noPct");
    return '<div class="banner warn"><div class="msg">' + saved +
      '<span class="hint">' + (from ? tr("banner.expired.refresh", { host: esc(from) }) : tr("banner.expired.paste")) +
      (t.error ? " (" + esc(t.error.replace(/^download link expired:\s*/i, "")) + ")" : "") + "</span></div>" +
      (t.referrer ? '<button class="btn warn small" data-act="refresh" data-id="' + t.id + '">' + tr("act.refresh") + "</button>" : "") +
      '<button class="btn small" data-act="address" data-id="' + t.id + '">' + tr("act.address") + "</button></div>";
  }
  if (t.state === "awaiting_refresh") {
    var left = t.refresh_until ? (new Date(t.refresh_until) - Date.now()) / 1000 : 0;
    return '<div class="banner warn"><div class="msg">' + tr("banner.awaiting") +
      '<span class="hint">' + (left > 0 ? tr("banner.awaiting.hintTime", { time: dur(left) }) : tr("banner.awaiting.hint")) + "</span></div>" +
      '<button class="btn small" data-act="pause" data-id="' + t.id + '">' + tr("act.stopWaiting") + "</button></div>";
  }
  if (t.state === "error") {
    return '<div class="banner err"><div class="msg">' + tr("banner.failed") + ' <span class="hint">' + esc(t.error || tr("banner.unknown")) + "</span></div>" +
      '<button class="btn small" data-act="resume" data-id="' + t.id + '">' + tr("act.retry") + "</button></div>";
  }
  return "";
}

// playable is true for a video that can be watched now: a finished one, or an
// ordinary file whose size is known, which the player can read while the rest
// arrives. Streams and yt-dlp downloads only become one file at the end.
function playable(t, ext) {
  // A torrent plays its largest video, which the daemon names once the file
  // list is known; pieces near where the player reads are fetched first.
  if (t.kind === "bt") ext = extOf(t.media);
  if (kindOf(ext) !== "video" && kindOf(ext) !== "audio") return false;
  if (t.kind === "bt") return t.state === "done" || t.state === "running" || t.state === "queued" || t.state === "paused" || t.state === "error";
  if (t.state === "done") return !!t.path;
  if (t.kind || !(t.size > 0) || t.resumable === false) return false;
  return t.state === "running" || t.state === "queued" || t.state === "paused" || t.state === "error";
}

function card(t) {
  var name = nameOf(t), ext = extOf(name), open = !!S.open[t.id];
  var bt = t.kind === "bt", icon = bt ? extOf(t.media) || "bt" : ext || tr("ico.file");
  var hasSize = t.size > 0;
  var pct = t.state === "done" ? 100 : progressOf(t);
  var acts = [];
  if (playable(t, ext)) acts.push(["play", tr(t.state === "done" ? "act.play" : "act.playNow")]);
  if (t.state === "running" || t.state === "queued") acts.push(["pause", tr("act.pause")]);
  if (t.state === "paused") acts.push(["resume", tr("act.resume")]);
  if (t.path) acts.push(["open", tr(t.state === "done" ? "act.showFolder" : "act.openFolder")]);
  if (t.state !== "done" && (t.segments || []).length) acts.push(["conns", tr(open ? "act.connsHide" : "act.conns")]);
  if ((t.state === "paused" || t.state === "error") && !bt) acts.push(["address", tr("act.address")]);
  acts.push(["remove", tr("act.remove")]);

  return '<div class="task" data-task="' + t.id + '">' +
    '<div class="ico ' + kindOf(icon) + '">' + esc(icon) + "</div>" +
    '<div class="body">' +
      '<div class="row"><span class="name" title="' + esc(name) + '">' + esc(name) + "</span>" +
      '<span class="pill ' + t.state + '">' + esc(stateLabel(t)) + "</span></div>" +
      (t.state === "done" ? "" : '<div class="bar"><div class="fill ' + t.state + '" style="width:' + pct.toFixed(1) + '%"></div></div>') +
      (t.state === "done" ? "" : segMap(t)) +
      '<div class="meta">' + metaLine(t) + "</div>" +
      '<div class="url" title="' + esc(t.url) + '">' + esc(t.url) + "</div>" +
      banner(t) +
      '<div class="actions">' + acts.map(function (a) {
        return '<button class="btn ghost small' + (a[0] === "remove" ? " danger" : "") + '" data-act="' + a[0] + '" data-id="' + t.id + '">' + a[1] + "</button>";
      }).join("") +
      taskControls(t) +
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
    ? tr("stats.running", { n: running, speed: human(speed) + "/s" }) + (queued ? " · " + tr("stats.waiting", { n: queued }) : "")
    : tasks.length ? (queued ? tr("stats.waiting", { n: queued }) : tr("stats.idle")) : "";
  // A one-shot action armed and forgotten is the one that would surprise.
  var armed = { sleep: "stats.sleep", shutdown: "stats.shutdown" }[S.afterAll];
  document.getElementById("stats").innerHTML = line + (armed ? (line ? " · " : "") + "<b>" + tr(armed) + "</b>" : "");

  var c = { all: tasks.length, active: 0, done: 0, attn: 0 };
  tasks.forEach(function (t) {
    if (inTab(t, "active")) c.active++;
    if (inTab(t, "done")) c.done++;
    if (inTab(t, "attn")) c.attn++;
  });
  var tabs = [["all", "tab.all"], ["active", "tab.active"], ["done", "tab.done"], ["attn", "tab.attn"]];
  document.getElementById("tabs").innerHTML = tabs.map(function (x) {
    return '<button class="tab' + (S.tab === x[0] ? " on" : "") + (x[0] === "attn" && c.attn ? " attn" : "") +
      '" data-tab="' + x[0] + '">' + esc(tr(x[1])) + '<span class="n">' + c[x[0]] + "</span></button>";
  }).join("");

  var shown = tasks.filter(function (t) { return inTab(t, S.tab); });
  var list = document.getElementById("list");
  // Re-rendering would close a dropdown the user is in the middle of choosing from.
  var a = document.activeElement;
  if (a && a.tagName === "SELECT" && list.contains(a)) return;
  if (!shown.length) {
    list.innerHTML = tasks.length
      ? '<div class="empty"><b>' + tr("empty.view.title") + "</b>" + tr("empty.view.text") + "</div>"
      : '<div class="empty"><b>' + tr("empty.first.title") + "</b>" + tr("empty.first.text") + "</div>";
  } else {
    list.innerHTML = shown.map(card).join("");
  }
}

function poll() {
  var asked = performance.now();
  return api("/api/tasks").then(function (d) {
    S.online = true;
    document.getElementById("dot").classList.remove("off");
    // Changed in another window since this page was loaded. An answer to a
    // question asked before this window's own change would undo it.
    if (d.language && d.language !== LANG_SETTING && asked > S.langAt) setLang(d.language);
    S.tasks = d.tasks || [];
    S.afterAll = d.after_all;
    if (d.limit && d.limit !== S.limit) { S.limit = d.limit; document.getElementById("limit").value = d.limit; }
    var sp = document.getElementById("speed");
    if (d.speed_limit !== undefined && d.speed_limit !== S.speed && document.activeElement !== sp) {
      S.speed = d.speed_limit;
      sp.innerHTML = speedOptions(S.speed);
    }
    render();
  }).catch(function () {
    S.online = false;
    document.getElementById("dot").classList.add("off");
    document.getElementById("stats").textContent = tr("net.offline");
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
      if (t && t.state !== "done") toast(tr("toast.player"));
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
      toast(r.referrer ? tr("toast.refresh.opened", { host: hostOf(r.referrer) }) : tr("toast.refresh.waiting"));
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
    toast(trn("toast.conns", r.connections));
    poll();
  }).catch(fail);
});

document.getElementById("list").addEventListener("change", function (e) {
  var sel = e.target.closest("select[data-speed]");
  if (!sel) return;
  var id = sel.getAttribute("data-speed"), n = parseInt(sel.value, 10);
  sel.blur();
  post("/api/speed?id=" + q(id) + "&n=" + n).then(function (r) {
    if (!r.ok) return toast(r.error, "err");
    toast(r.speed_limit > 0 ? tr("toast.speed.task", { speed: speedLabel(r.speed_limit) }) : tr("toast.speed.taskNone"));
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
  if (!/^https?:\/\//i.test(url)) return toast(tr("toast.address.bad"), "err");
  hide("addrDlg");
  post("/api/address?id=" + q(S.target), { url: url }).then(function (r) {
    if (!r.ok) return toast(r.error, "err");
    toast(tr("toast.address.check"));
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
(function () {
  var sel = document.getElementById("speed");
  sel.innerHTML = speedOptions(S.speed);
  sel.addEventListener("change", function () {
    post("/api/speed?n=" + sel.value).then(function (r) {
      S.speed = r.speed_limit;
      sel.blur();
      toast(r.speed_limit > 0 ? tr("toast.speed.all", { speed: speedLabel(r.speed_limit) }) : tr("toast.speed.allNone"));
      poll();
    }).catch(fail);
  });
})();
document.getElementById("pauseAll").addEventListener("click", function () { post("/api/pause-all").then(poll).catch(fail); });
document.getElementById("resumeAll").addEventListener("click", function () { post("/api/resume-all").then(poll).catch(fail); });

// ---------- add links ----------
function parseLinks(text) {
  var seen = {}, out = [];
  (text || "").split(/[\s]+/).forEach(function (s) {
    s = s.trim();
    if ((/^https?:\/\/\S+$/i.test(s) || /^magnet:\?\S+$/i.test(s)) && !seen[s]) { seen[s] = 1; out.push(s); }
  });
  return out;
}
function updateAddCount() {
  var n = parseLinks(document.getElementById("addText").value).length;
  document.getElementById("addCount").textContent = n ? trn("add.count", n) : tr("add.count.none");
  var go = document.getElementById("addGo");
  go.disabled = !n;
  go.textContent = n > 1 ? tr("add.goMany", { n: n }) : tr("add.go");
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
    toast(trn("toast.added", n) + ((r.errors || []).length ? " " + tr("toast.rejected", { n: r.errors.length }) : ""));
    document.getElementById("addText").value = "";
    S.tab = "all";
    poll();
  }).catch(fail);
});

// ---------- settings ----------
// Only what the user changed is sent. A setting that changed on its own while
// the dialog was open (a one-shot action that has already fired) must not be
// put back by pressing Save.
var AFTER_LABEL = { nothing: "after.nothing", sleep: "after.sleep", shutdown: "after.shutdown" };
var AFTER_DOES = { sleep: "toast.after.sleep", shutdown: "toast.after.shutdown" };// The kinds match what the daemon sorts by; the names are only what to call them.
var FOLDER_KINDS = [["video", "kind.video"], ["audio", "kind.audio"], ["archive", "kind.archive"], ["doc", "kind.doc"], ["app", "kind.app"], ["image", "kind.image"]];
// A language is named in its own language wherever it is listed, so that it can
// be found when the rest of the page is in one that cannot be read.
var LANG_CHOICES = [["auto", ""], ["en", "English"], ["zh", "简体中文"]];
function folderInputs() { return document.querySelectorAll("#setFolders input"); }
function dimFolders() { document.getElementById("setFolders").classList.toggle("off", !document.getElementById("setSort").checked); }
document.getElementById("setFolders").innerHTML = FOLDER_KINDS.map(function (k) {
  return '<label><span data-i18n="' + k[1] + '"></span><input type="text" data-folder="' + k[0] + '" maxlength="100" data-i18n-ph="set.folder.ph" spellcheck="false"></label>';
}).join("");
document.getElementById("setSort").addEventListener("change", dimFolders);

// fillSettingsText writes the words in the dialog that script rather than a
// data-i18n attribute provides. It runs when the dialog opens and again when
// the language changes, so one left open follows.
function fillSettingsText() {
  var d = S.settings || {};
  document.getElementById("setSortHelp").innerHTML = tr("set.sort.help", { dir: "<b>" + esc(d.out_dir || "") + "</b>" });
  var lang = document.getElementById("setLang"), chosen = lang.value || LANG_SETTING;
  lang.innerHTML = LANG_CHOICES.map(function (c) {
    return '<option value="' + c[0] + '">' + esc(c[1] || tr("set.lang.auto")) + "</option>";
  }).join("");
  lang.value = chosen;
  var after = document.getElementById("setAfter"), picked = after.value || d.after_all;
  after.innerHTML = (d.after_all_options || []).map(function (o) {
    return '<option value="' + o + '">' + esc(AFTER_LABEL[o] ? tr(AFTER_LABEL[o]) : o) + "</option>";
  }).join("");
  after.value = picked;
}

// setLang shows the page in a language, now. It is also how the page follows a
// change made in another window.
function setLang(setting) {
  LANG_SETTING = setting;
  LANG = resolveLang(setting);
  S.langAt = performance.now();
  document.documentElement.lang = LANG === "zh" ? "zh-CN" : "en";
  document.querySelectorAll("[data-i18n]").forEach(function (el) { el.textContent = tr(el.getAttribute("data-i18n")); });
  document.querySelectorAll("[data-i18n-title]").forEach(function (el) { el.title = tr(el.getAttribute("data-i18n-title")); });
  document.querySelectorAll("[data-i18n-ph]").forEach(function (el) { el.placeholder = tr(el.getAttribute("data-i18n-ph")); });
  document.getElementById("speed").innerHTML = speedOptions(S.speed);
  updateAddCount();
  fillSettingsText();
  render();
}

function openSettings() {
  api("/api/settings").then(function (d) {
    S.settings = d;
    document.getElementById("setAwake").checked = !!d.keep_awake;
    document.getElementById("awakeRow").hidden = !d.can_keep_awake;
    fillSettingsText();
    document.getElementById("setLang").value = d.language || "auto";
    document.getElementById("setAfter").value = d.after_all;
    document.getElementById("afterRow").hidden = (d.after_all_options || []).length < 2;
    document.getElementById("setSort").checked = !!d.sort_by_type;
    folderInputs().forEach(function (i) { i.value = (d.folders || {})[i.getAttribute("data-folder")] || ""; });
    dimFolders();
    show("setDlg");
  }).catch(fail);
}
document.getElementById("setBtn").addEventListener("click", openSettings);
document.getElementById("setGo").addEventListener("click", function () {
  var was = S.settings, upd = {};
  var lang = document.getElementById("setLang").value;
  if (lang && lang !== (was.language || "auto")) upd.language = lang;
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
    // Before the toast, so that it is already in the new language.
    if (r.language && r.language !== LANG_SETTING) setLang(r.language);
    toast(upd.after_all && AFTER_DOES[r.after_all] ? tr(AFTER_DOES[r.after_all]) : tr("toast.saved"));
    poll();
  }).catch(fail);
});

// A .torrent file is sent as it is; the daemon keeps its own copy, so the
// original can be deleted afterwards.
document.getElementById("torrentPick").addEventListener("click", function () {
  document.getElementById("torrentFile").click();
});
document.getElementById("torrentFile").addEventListener("change", function (e) {
  var files = Array.prototype.slice.call(e.target.files || []);
  e.target.value = "";
  if (!files.length) return;
  hide("addDlg");
  var added = 0, failed = [];
  files.reduce(function (chain, f) {
    return chain.then(function () {
      return fetch("/api/torrent", {
        method: "POST",
        headers: { "Authorization": "Bearer " + TOKEN, "Content-Type": "application/x-bittorrent" },
        body: f
      }).then(function (r) {
        if (!r.ok) return r.text().then(function (msg) { failed.push(f.name + ": " + msg.trim()); });
        added++;
      }).catch(function (err) { failed.push(f.name + ": " + err.message); });
    });
  }, Promise.resolve()).then(function () {
    if (failed.length) toast((added ? tr("toast.torrent.some", { n: added }) + " " : "") + failed.join("; "), "err");
    else toast(trn("toast.torrent", added));
    S.tab = "all";
    poll();
  });
});

// Pasting links anywhere outside a text field opens the add dialog with them.
document.addEventListener("paste", function (e) {
  var a = document.activeElement;
  if (a && (a.tagName === "INPUT" || a.tagName === "TEXTAREA")) return;
  var text = (e.clipboardData || window.clipboardData).getData("text");
  if (parseLinks(text).length) { e.preventDefault(); openAdd(text); }
});

setLang(LANG_SETTING);
poll();
setInterval(poll, 700);
</script>
</body>
</html>
`

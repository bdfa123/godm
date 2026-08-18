package main

// uiHTML is served at / with __TOKEN__ replaced by the daemon secret so the
// page can call the guarded API without the user pasting anything.
const uiHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>godm</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f6f7f9; --panel: #ffffff; --line: #e2e5ea; --text: #14181f;
    --muted: #6b7480; --accent: #2f6df6; --ok: #1f9d55; --err: #d64545;
    --track: #e8ebf0;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #14171c; --panel: #1b1f26; --line: #2a3038; --text: #e6e9ee;
      --muted: #8a929c; --accent: #5b8cff; --ok: #37b26b; --err: #ef6a6a;
      --track: #262c34;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--text);
    font: 14px/1.5 "Segoe UI", system-ui, -apple-system, sans-serif;
  }
  header {
    display: flex; align-items: center; gap: 12px; padding: 16px 20px;
    border-bottom: 1px solid var(--line); background: var(--panel);
    position: sticky; top: 0; z-index: 2;
  }
  h1 { font-size: 16px; margin: 0; letter-spacing: .5px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--ok); }
  .dot.off { background: var(--err); }
  main { max-width: 900px; margin: 0 auto; padding: 20px; }
  form { display: flex; gap: 8px; margin-bottom: 20px; }
  input[type=text] {
    flex: 1; padding: 9px 12px; border: 1px solid var(--line); border-radius: 6px;
    background: var(--panel); color: var(--text); font-size: 14px;
  }
  input[type=number] { width: 70px; padding: 9px 8px; border: 1px solid var(--line);
    border-radius: 6px; background: var(--panel); color: var(--text); }
  button {
    padding: 9px 16px; border: 1px solid transparent; border-radius: 6px;
    background: var(--accent); color: #fff; cursor: pointer; font-size: 14px;
  }
  button.ghost { background: transparent; color: var(--muted); border-color: var(--line); padding: 5px 10px; font-size: 12px; }
  button:hover { filter: brightness(1.08); }
  .task {
    background: var(--panel); border: 1px solid var(--line); border-radius: 8px;
    padding: 14px 16px; margin-bottom: 10px;
  }
  .row { display: flex; align-items: baseline; gap: 10px; }
  .name { font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1; }
  .meta { color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; white-space: nowrap; }
  .track { height: 6px; background: var(--track); border-radius: 3px; margin: 10px 0 6px; overflow: hidden; }
  .fill { height: 100%; background: var(--accent); border-radius: 3px; transition: width .3s ease; }
  .fill.done { background: var(--ok); }
  .fill.error { background: var(--err); }
  .url { color: var(--muted); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .err { color: var(--err); font-size: 12px; margin-top: 6px; word-break: break-word; }
  .actions { display: flex; gap: 6px; }
  .empty { color: var(--muted); text-align: center; padding: 60px 0; }
  .badge { font-size: 11px; text-transform: uppercase; letter-spacing: .6px;
    color: var(--muted); border: 1px solid var(--line); border-radius: 4px; padding: 1px 6px; }
</style>
</head>
<body>
<header>
  <span class="dot" id="dot"></span>
  <h1>godm</h1>
  <span class="meta" id="summary"></span>
</header>
<main>
  <form id="add">
    <input type="text" id="url" placeholder="Paste a URL to download" autocomplete="off">
    <input type="number" id="conns" value="8" min="1" max="32" title="connections">
    <button type="submit">Add</button>
  </form>
  <div id="list"></div>
</main>
<script>
var TOKEN = "__TOKEN__";

function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({}, opts.headers, {
    "Authorization": "Bearer " + TOKEN,
    "Content-Type": "application/json"
  });
  return fetch(path, opts).then(function (r) {
    if (!r.ok) throw new Error("HTTP " + r.status);
    return r.json();
  });
}

function human(n) {
  if (n === null || n === undefined || n < 0) return "?";
  var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + " " + u[i];
}

function eta(t) {
  if (t.state !== "running" || t.size <= 0 || t.speed <= 0) return "";
  var s = Math.round((t.size - t.received) / t.speed);
  if (s < 60) return s + "s left";
  if (s < 3600) return Math.floor(s / 60) + "m " + (s % 60) + "s left";
  return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m left";
}

function render(tasks) {
  var list = document.getElementById("list");
  if (!tasks.length) {
    list.innerHTML = '<div class="empty">No downloads yet.<br>Browser downloads land here once the extension is installed.</div>';
    return;
  }
  var running = 0, html = "";
  tasks.forEach(function (t) {
    if (t.state === "running") running++;
    var pct = t.size > 0 ? Math.min(100, t.received / t.size * 100) : (t.state === "done" ? 100 : 0);
    var cls = t.state === "done" ? "done" : (t.state === "error" ? "error" : "");
    var right = t.state === "running"
      ? human(t.received) + " / " + human(t.size) + "  ·  " + human(t.speed) + "/s  ·  " + eta(t)
      : human(t.received) + (t.size > 0 ? " / " + human(t.size) : "");

    html += '<div class="task">'
      + '<div class="row">'
      +   '<span class="name">' + esc(t.filename || t.url) + '</span>'
      +   '<span class="badge">' + t.state + '</span>'
      + '</div>'
      + '<div class="track"><div class="fill ' + cls + '" style="width:' + pct.toFixed(1) + '%"></div></div>'
      + '<div class="row">'
      +   '<span class="url">' + esc(t.url) + '</span>'
      +   '<span class="meta">' + esc(right) + '</span>'
      + '</div>'
      + (t.error ? '<div class="err">' + esc(t.error) + '</div>' : '')
      + '<div class="actions" style="margin-top:8px">'
      +   (t.state === "running" || t.state === "queued"
            ? '<button class="ghost" data-cancel="' + t.id + '">Stop</button>' : '')
      +   '<button class="ghost" data-remove="' + t.id + '">Remove</button>'
      + '</div>'
      + '</div>';
  });
  list.innerHTML = html;
  document.getElementById("summary").textContent =
    tasks.length + " task" + (tasks.length === 1 ? "" : "s") +
    (running ? ", " + running + " running" : "");
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"]/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
  });
}

document.getElementById("list").addEventListener("click", function (e) {
  var c = e.target.getAttribute("data-cancel");
  var r = e.target.getAttribute("data-remove");
  if (c) api("/api/cancel?id=" + encodeURIComponent(c), { method: "POST" }).then(poll);
  if (r) api("/api/remove?id=" + encodeURIComponent(r), { method: "POST" }).then(poll);
});

document.getElementById("add").addEventListener("submit", function (e) {
  e.preventDefault();
  var u = document.getElementById("url");
  var n = parseInt(document.getElementById("conns").value, 10) || 8;
  if (!u.value.trim()) return;
  api("/api/download", {
    method: "POST",
    body: JSON.stringify({ url: u.value.trim(), connections: n })
  }).then(function () { u.value = ""; poll(); })
    .catch(function (err) { alert(err.message); });
});

function poll() {
  return api("/api/tasks").then(function (d) {
    document.getElementById("dot").classList.remove("off");
    render(d.tasks || []);
  }).catch(function () {
    document.getElementById("dot").classList.add("off");
  });
}

poll();
setInterval(poll, 700);
</script>
</body>
</html>
`

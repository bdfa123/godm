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
    const right = t.state === "running"
      ? human(t.speed) + "/s"
      : t.state;
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

const box = document.getElementById("enabled");
chrome.storage.sync.get({ enabled: true }, (c) => { box.checked = c.enabled; });
box.addEventListener("change", () => {
  chrome.storage.sync.set({ enabled: box.checked });
});

refresh();
setInterval(refresh, 1000);

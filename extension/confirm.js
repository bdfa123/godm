"use strict";

// The download confirmation dialog. background.js cancelled the browser's own
// download and left the details under the key in the URL; this page lets the
// person adjust them, then asks the worker to send the job to godm. Validation
// comes from handoff.js, which the worker runs again before sending.

const heldKey = new URLSearchParams(location.search).get("k");
const ui = {
  form: document.getElementById("form"),
  url: document.getElementById("url"),
  name: document.getElementById("name"),
  size: document.getElementById("size"),
  dir: document.getElementById("dir"),
  browse: document.getElementById("browse"),
  conns: document.getElementById("conns"),
  dontAsk: document.getElementById("dontAsk"),
  status: document.getElementById("status"),
  cancel: document.getElementById("cancel"),
  start: document.getElementById("start")
};

let note = ""; // something worth saying that is not a mistake in the form
let busy = false; // Start has been pressed and godm has not answered yet
let browsing = false; // the folder chooser is open on the desktop
let dirTouched = false; // the person has typed or picked a folder themselves
let defaultDir = ""; // godm's own download folder, shown as the placeholder
let over = false; // nothing left to do here: the request expired, or went back to Chrome

// The folder goes to godm only when the person chose one. Sending the default
// as if it had been chosen would also switch off godm's sorting by type,
// because an explicit folder always wins over it.
function formValues() {
  return {
    filename: ui.name.value,
    outDir: dirTouched ? ui.dir.value : "",
    connections: ui.conns.value
  };
}

// render decides what the status line says and whether Start can be pressed.
// A mistake in the form outranks a note, because it is what stops Start.
function render() {
  const problem = over ? "" : firstProblem(formValues());
  ui.status.textContent = problem || note;
  ui.status.className = problem ? "err" : "";
  ui.start.disabled = !!problem || busy || browsing || over;
  ui.browse.disabled = busy || browsing || over;
  // Once Start is pressed the job is on its way to godm and closing the window
  // cannot call it back, so Cancel would only look as if it could.
  ui.cancel.disabled = busy;
  for (const el of [ui.name, ui.dir, ui.conns, ui.dontAsk]) el.disabled = busy || over;
}

// Closing the window is how Cancel works: the worker throws away what it was
// holding when it sees the window go.
async function closeSelf() {
  try {
    const w = await chrome.windows.getCurrent();
    await chrome.windows.remove(w.id);
  } catch (e) {
    window.close();
  }
}

// finish ends the dialog's work: what is shown stays as it is and the only thing
// left to do is close the window.
function finish(text) {
  over = true;
  busy = false;
  note = text;
  ui.cancel.textContent = tr("confirm_close");
  render();
}

async function send(message) {
  try {
    return await chrome.runtime.sendMessage(message);
  } catch (e) {
    return { ok: false, error: String(e && e.message ? e.message : e) };
  }
}

ui.form.addEventListener("submit", async (e) => {
  e.preventDefault();
  if (busy || browsing || over) return;
  // Start is disabled while the form has a mistake in it, so getting here with
  // one means the page had not caught up. Say what is wrong rather than leave
  // the person pressing a button that does nothing.
  if (firstProblem(formValues())) {
    render();
    return;
  }
  busy = true;
  note = tr("confirm_sending");
  render();

  const resp = await send(
    Object.assign({ type: "godm-confirm-start", key: heldKey, dontAsk: ui.dontAsk.checked }, formValues())
  );

  if (resp && resp.ok) {
    closeSelf();
    return;
  }
  if (resp && resp.expired) {
    finish(tr("err_request_expired"));
    return;
  }
  if (resp && resp.handedBack) {
    // godm refused the job and the worker has already given the download back
    // to Chrome and sent a notification. Nothing is left to do here.
    finish(tr("confirm_handed_back", resp.error || tr("confirm_handed_back_default")));
    setTimeout(closeSelf, 4000);
    return;
  }
  // Either the worker found a mistake the page missed, or it did not answer at
  // all. Starting again is safe in both cases: it was not consumed, or it
  // reports that it has expired.
  busy = false;
  note = (resp && resp.error) || tr("confirm_no_answer");
  render();
});

ui.cancel.addEventListener("click", () => {
  if (!busy) closeSelf();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !busy) closeSelf();
});

ui.name.addEventListener("input", render);
ui.conns.addEventListener("input", render);
// One listener, in this order: the folder only counts once it is known to have
// been typed, so render has to come after dirTouched or a mistake in what was
// just typed or pasted is not seen until the next key.
ui.dir.addEventListener("input", () => {
  dirTouched = true;
  render();
});

ui.browse.addEventListener("click", async () => {
  if (busy || browsing) return;
  browsing = true;
  note = tr("confirm_choose_folder");
  render();
  const resp = await send({ type: "godm-browse", current: ui.dir.value.trim() || defaultDir });
  browsing = false;
  if (resp && resp.ok) {
    // An empty path is the person closing the chooser; keep what was typed.
    if (resp.path) {
      ui.dir.value = resp.path;
      dirTouched = true;
    }
    note = "";
  } else {
    note = tr("confirm_browse_fail", (resp && resp.error) || tr("err_no_response"));
  }
  render();
});

async function loadConfig() {
  const resp = await send({ type: "godm-config" });
  const cfg = resp && resp.ok && resp.config;
  if (!cfg) {
    note = tr("confirm_config_fail", (resp && resp.error) || tr("err_no_answer"));
    render();
    return;
  }
  // The default is a placeholder, not a value, so leaving it alone lets godm
  // decide, sorting included.
  defaultDir = cfg.out_dir || "";
  if (defaultDir) {
    ui.dir.placeholder = cfg.sort_by_type ? tr("confirm_sorted", defaultDir) : defaultDir;
  }
  if (cfg.can_browse_folders === false) ui.browse.hidden = true;
  render();
}

(async function init() {
  const held = heldKey ? (await chrome.storage.session.get(heldKey))[heldKey] : null;
  if (!held) {
    finish(tr("err_request_expired"));
    return;
  }
  ui.url.textContent = held.url;
  ui.url.title = held.url;
  ui.name.value = held.filename;
  ui.size.textContent = formatSize(held.size);
  const wanted = parseInt(held.connections, 10) || 8;
  ui.conns.value = Math.min(Math.max(wanted, 1), DIALOG_MAX_CONNECTIONS);

  // Select the name but not the extension, so typing replaces the right part.
  const dot = held.filename.lastIndexOf(".");
  ui.name.focus();
  ui.name.setSelectionRange(0, dot > 0 ? dot : held.filename.length);

  render();
  loadConfig();
})();

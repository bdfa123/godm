"use strict";

// The rules behind the download confirmation dialog, kept apart from the
// browser plumbing so they can be run against a table of cases. The dialog page
// uses them to show what is wrong while the person can still fix it, and the
// service worker runs the same checks again before anything is sent to godm:
// the page is only a form, the worker decides what goes out.

// The same ceiling as MaxConnections in engine.go. The daemon clamps anyway;
// this is so the dialog says so instead of quietly using a different number.
const DIALOG_MAX_CONNECTIONS = 32;

// What NTFS refuses in a file name. The daemon would swap these for "_" without
// a word, which leaves the person looking for a file under a different name.
const BAD_NAME_CHARS = /[\\/:*?"<>|\u0000-\u001f]/;

function lastPart(p) {
  return String(p == null ? "" : p).split(/[\\/]/).pop();
}

// suggestName is what the name field opens with. Chrome has already settled on
// a name by the time it asks us (that is why takeover happens in
// onDeterminingFilename), as a full path into its own download folder; only
// the last part is wanted.
function suggestName(item) {
  const fromBrowser = lastPart(item && item.filename).trim();
  if (fromBrowser) return fromBrowser;
  for (const u of [item && item.url, item && item.finalUrl]) {
    try {
      const n = decodeURIComponent(lastPart(new URL(u).pathname)).trim();
      if (n) return n;
    } catch (e) {
      // Not a URL, or a path that does not decode; try the next one.
    }
  }
  return "download";
}

// pickSize returns the byte count when the browser knows it, else 0. totalBytes
// is -1 while the server has not said, which a plain truthiness check would
// treat as a size.
function pickSize(item) {
  if (item && item.totalBytes > 0) return item.totalBytes;
  if (item && item.fileSize > 0) return item.fileSize;
  return 0;
}

function groupDigits(n) {
  return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

function formatSize(bytes) {
  if (!(bytes > 0)) return "Unknown";
  if (bytes < 1024) return bytes + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let v = bytes;
  let i = -1;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  const digits = v >= 100 ? 0 : v >= 10 ? 1 : 2;
  return v.toFixed(digits) + " " + units[i] + " (" + groupDigits(bytes) + " bytes)";
}

// Each validator returns "" when the value is fine, else a sentence for the
// person.

function validateName(raw) {
  const name = String(raw == null ? "" : raw).trim();
  if (!name) return "Enter a file name.";
  if (BAD_NAME_CHARS.test(name)) return 'A file name cannot contain \\ / : * ? " < > |';
  if (name.endsWith(".")) return "A file name cannot end with a dot.";
  return "";
}

// An empty folder is allowed and means godm's own default. Anything else must
// be a full path: the daemon would otherwise resolve it against whatever
// directory it happened to be started from.
function validateDir(raw) {
  const dir = String(raw == null ? "" : raw).trim();
  if (!dir) return "";
  if (/[\u0000-\u001f]/.test(dir)) return "The folder path has characters that cannot be used.";
  const drive = /^[A-Za-z]:[\\/]/.test(dir);
  const unc = /^\\\\[^\\/?]+[\\/][^\\/]+/.test(dir);
  if (drive || unc) {
    // The drive colon is the one colon a path may have.
    if (/[*?"<>|:]/.test(dir.replace(/^[A-Za-z]:/, ""))) {
      return 'A folder name cannot contain : * ? " < > |';
    }
    return "";
  }
  if (dir.startsWith("/")) return "";
  return "Enter the full folder path, such as C:\\Downloads.";
}

// parseConnections reads the field as typed. parseInt would accept "8abc" and
// "2.9"; a number box can hold both.
function parseConnections(raw) {
  const s = String(raw == null ? "" : raw).trim();
  const bad = "Connections must be a whole number from 1 to " + DIALOG_MAX_CONNECTIONS + ".";
  if (!/^\d+$/.test(s)) return { error: bad };
  const n = parseInt(s, 10);
  if (n < 1 || n > DIALOG_MAX_CONNECTIONS) return { error: bad };
  return { value: n };
}

// firstProblem is what the dialog shows under the form and what disables Start.
function firstProblem(form) {
  return (
    validateName(form.filename) ||
    validateDir(form.outDir) ||
    parseConnections(form.connections).error ||
    ""
  );
}

// buildJob turns what was held back when the download was cancelled in the
// browser, plus what the person chose, into the message for the host. Cookies
// and the user agent are added by the caller, which is the only place that can
// read them. Returns {error} or {job}.
function buildJob(pending, form) {
  const problem = firstProblem(form || {});
  if (problem) return { error: problem };
  return {
    job: {
      type: "download",
      url: pending.url,
      filename: String(form.filename).trim(),
      referrer: pending.referrer || "",
      connections: parseConnections(form.connections).value,
      outDir: String(form.outDir == null ? "" : form.outDir).trim()
    }
  };
}

// dialogBounds places the dialog over the middle of the browser window it came
// from, nudged for each dialog already waiting so a burst of downloads fans out
// instead of stacking exactly. Without a usable parent it asks for a size only
// and lets the browser choose.
function dialogBounds(parent, waiting, width, height) {
  const out = { width: width, height: height };
  const usable =
    parent &&
    parent.state !== "minimized" &&
    [parent.left, parent.top, parent.width, parent.height].every(Number.isFinite);
  if (!usable) return out;
  const nudge = 28 * (waiting % 8);
  out.left = Math.round(parent.left + (parent.width - width) / 2) + nudge;
  out.top = Math.round(parent.top + (parent.height - height) / 2) + nudge;
  return out;
}

// Node runs this file directly to test the rules; a service worker or a page
// has no module object and skips it.
if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    DIALOG_MAX_CONNECTIONS,
    buildJob,
    dialogBounds,
    firstProblem,
    formatSize,
    parseConnections,
    pickSize,
    suggestName,
    validateDir,
    validateName
  };
}

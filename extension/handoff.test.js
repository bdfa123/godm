"use strict";

// Run with: node --test extension/handoff.test.js
const test = require("node:test");
const assert = require("node:assert");
const {
  DIALOG_MAX_CONNECTIONS,
  buildJob,
  dialogBounds,
  formatSize,
  parseConnections,
  pickSize,
  suggestName,
  validateDir,
  validateName
} = require("./handoff.js");

test("the suggested name is the last part of the path Chrome picked", () => {
  assert.equal(suggestName({ filename: "C:\\Users\\me\\Downloads\\setup.exe" }), "setup.exe");
  assert.equal(suggestName({ filename: "/home/me/Downloads/setup.exe" }), "setup.exe");
  assert.equal(suggestName({ filename: "setup.exe" }), "setup.exe");
});

test("with no name from the browser the URL path supplies one, decoded", () => {
  assert.equal(suggestName({ filename: "", url: "https://x.test/files/My%20Video.mp4?sig=1" }), "My Video.mp4");
  // The original link is preferred, the redirect target is the fallback.
  assert.equal(
    suggestName({ url: "https://x.test/", finalUrl: "https://cdn.test/a/b/real.zip" }),
    "real.zip"
  );
  // A path that does not decode must not throw.
  assert.equal(suggestName({ url: "https://x.test/%E0%A4%A" }), "download");
  assert.equal(suggestName({}), "download");
  assert.equal(suggestName(null), "download");
});

test("size is only reported when the browser really knows it", () => {
  assert.equal(pickSize({ totalBytes: 5000 }), 5000);
  assert.equal(pickSize({ totalBytes: -1 }), 0);
  assert.equal(pickSize({ totalBytes: 0, fileSize: 777 }), 777);
  assert.equal(pickSize({ totalBytes: -1, fileSize: -1 }), 0);
  assert.equal(pickSize({}), 0);
});

test("sizes read the way people expect", () => {
  assert.equal(formatSize(0), "Unknown");
  assert.equal(formatSize(-1), "Unknown");
  assert.equal(formatSize(undefined), "Unknown");
  assert.equal(formatSize(512), "512 B");
  assert.equal(formatSize(1536), "1.50 KB (1,536 bytes)");
  assert.equal(formatSize(5 * 1024 * 1024), "5.00 MB (5,242,880 bytes)");
  assert.equal(formatSize(123456789), "118 MB (123,456,789 bytes)");
  assert.equal(formatSize(12.5 * 1024 ** 3), "12.5 GB (13,421,772,800 bytes)");
});

test("a file name NTFS would refuse is caught before it is sent", () => {
  assert.equal(validateName("movie.mkv"), "");
  assert.equal(validateName("  spaced name (1).zip  "), "");
  assert.equal(validateName(".bashrc"), "");
  for (const bad of ["", "   ", "a/b.zip", "a\\b.zip", "a:b", "a*b", "a?b", 'a"b', "a<b", "a>b", "a|b", "a\u0001b", "name.", ".."]) {
    assert.notEqual(validateName(bad), "", JSON.stringify(bad) + " should be refused");
  }
});

test("a folder must be a full path, or empty for godm's default", () => {
  for (const good of ["", "   ", "C:\\Downloads", "d:/media/videos", "E:\\", "\\\\nas\\share\\files", "/srv/downloads"]) {
    assert.equal(validateDir(good), "", JSON.stringify(good) + " should be accepted");
  }
  for (const bad of ["Downloads", ".\\here", "..\\up", "C:", "C:folder", "C:\\a:b", "C:\\wh*t", "D:\\a<b", "C:\\x\u0000y", "\\\\server"]) {
    assert.notEqual(validateDir(bad), "", JSON.stringify(bad) + " should be refused");
  }
});

test("connections are whole numbers within the ceiling", () => {
  assert.deepEqual(parseConnections("8"), { value: 8 });
  assert.deepEqual(parseConnections(" 1 "), { value: 1 });
  assert.deepEqual(parseConnections(32), { value: 32 });
  for (const bad of ["", "0", "33", "-2", "2.9", "8abc", "abc", null, undefined]) {
    assert.ok(parseConnections(bad).error, JSON.stringify(bad) + " should be refused");
  }
  assert.equal(DIALOG_MAX_CONNECTIONS, 32);
});

const pending = {
  url: "https://example.test/dl?id=7",
  backUrl: "https://cdn.example.test/signed/abc",
  referrer: "https://example.test/page"
};

test("the job carries what was held back and what the person chose", () => {
  const { job, error } = buildJob(pending, {
    filename: "  renamed.zip ",
    outDir: " E:\\Stuff ",
    connections: "16",
    dontAsk: true
  });
  assert.equal(error, undefined);
  assert.deepEqual(job, {
    type: "download",
    url: pending.url,
    filename: "renamed.zip",
    referrer: pending.referrer,
    connections: 16,
    outDir: "E:\\Stuff"
  });
});

test("an empty folder goes through as empty so the daemon uses its own", () => {
  const { job } = buildJob(pending, { filename: "a.zip", outDir: "", connections: "8" });
  assert.equal(job.outDir, "");
});

test("the dialog's own flags never leak into the job", () => {
  const { job } = buildJob(pending, { filename: "a.zip", outDir: "", connections: "8", dontAsk: true, key: "confirm:1" });
  assert.deepEqual(Object.keys(job).sort(), ["connections", "filename", "outDir", "referrer", "type", "url"]);
});

test("a form that fails validation produces no job, whatever the page claims", () => {
  const cases = [
    { filename: "", outDir: "", connections: "8" },
    { filename: "../../evil.exe", outDir: "", connections: "8" },
    { filename: "a.zip", outDir: "relative\\dir", connections: "8" },
    { filename: "a.zip", outDir: "", connections: "999" },
    { filename: "a.zip", outDir: "", connections: "" }
  ];
  for (const form of cases) {
    const r = buildJob(pending, form);
    assert.ok(r.error, JSON.stringify(form));
    assert.equal(r.job, undefined);
  }
  assert.ok(buildJob(pending, undefined).error);
});

test("dialogs open over the middle of the browser window", () => {
  const parent = { left: 100, top: 50, width: 1000, height: 800, state: "normal" };
  assert.deepEqual(dialogBounds(parent, 0, 500, 400), { width: 500, height: 400, left: 350, top: 250 });
});

test("waiting dialogs fan out instead of stacking exactly", () => {
  const parent = { left: 0, top: 0, width: 1000, height: 800, state: "normal" };
  const a = dialogBounds(parent, 0, 500, 400);
  const b = dialogBounds(parent, 1, 500, 400);
  const c = dialogBounds(parent, 8, 500, 400);
  assert.equal(b.left - a.left, 28);
  assert.equal(b.top - a.top, 28);
  assert.deepEqual(c, a, "the fan wraps round rather than walking off the screen");
});

test("without a usable browser window only a size is asked for", () => {
  const sizeOnly = { width: 500, height: 400 };
  assert.deepEqual(dialogBounds(null, 0, 500, 400), sizeOnly);
  assert.deepEqual(dialogBounds({ state: "minimized", left: -32000, top: -32000, width: 160, height: 28 }, 0, 500, 400), sizeOnly);
  assert.deepEqual(dialogBounds({ state: "normal" }, 0, 500, 400), sizeOnly);
  // A window on a monitor left of the primary has negative coordinates; that is valid.
  assert.equal(dialogBounds({ left: -1920, top: 0, width: 1920, height: 1000, state: "maximized" }, 0, 500, 400).left, -1210);
});

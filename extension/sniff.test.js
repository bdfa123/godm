"use strict";

// Run with: node --test extension/
const test = require("node:test");
const assert = require("node:assert");
const { classifyMedia, mediaEntry, shouldRecord } = require("./sniff.js");

const MB = 1 << 20;

test("a playlist is recognised by its type even with no extension in the URL", () => {
  const cases = [
    ["https://v.example/hls/master.m3u8", "application/vnd.apple.mpegurl", 2048, "hls"],
    ["https://v.example/hls/master.m3u8", "text/plain", 2048, "hls"],
    ["https://cdn.example/stream?id=42", "application/x-mpegURL; charset=utf-8", 900, "hls"],
    ["https://cdn.example/stream?id=42", "application/vnd.apple.mpegurl", 900, "hls"],
    ["https://v.example/dash/manifest.mpd", "application/dash+xml", 4096, "dash"],
    ["https://cdn.example/m", "application/dash+xml", 4096, "dash"]
  ];
  for (const [url, ct, len, want] of cases) {
    assert.equal(classifyMedia(url, ct, len), want, url + " as " + ct);
  }
});

test("segments of a stream are never offered as downloads of their own", () => {
  const cases = [
    ["https://v.example/hls/seg00042.ts", "video/mp2t", 4 * MB],
    ["https://v.example/hls/chunk-7.m4s", "video/mp4", 6 * MB],
    ["https://v.example/x/v.cmfv", "video/mp4", 6 * MB],
    ["https://v.example/x/a.cmfa", "audio/mp4", 2 * MB],
    // No extension at all, but the type gives it away.
    ["https://v.example/segment?n=9", "video/mp2t", 4 * MB]
  ];
  for (const [url, ct, len] of cases) {
    assert.equal(classifyMedia(url, ct, len), "", url);
  }
});

test("a plain media response counts only once it is big enough to care about", () => {
  assert.equal(classifyMedia("https://v.example/clip.mp4", "video/mp4", 50 * MB), "file");
  assert.equal(classifyMedia("https://v.example/song.m4a", "audio/mp4", 8 * MB), "file");
  // An ad bumper, a preview loop or a notification sound.
  assert.equal(classifyMedia("https://v.example/bump.mp4", "video/mp4", 200 * 1024), "");
  assert.equal(classifyMedia("https://v.example/page", "text/html", 50 * MB), "");
  assert.equal(classifyMedia("https://v.example/app.js", "application/javascript", 50 * MB), "");
});

// ---------- what makes it into the list ----------

function entry(url, kind) {
  return mediaEntry(url, kind, kind === "file" ? "video/mp4" : "", 0, "", 1000);
}

test("a master playlist and the variants it names are one entry, not four", () => {
  const list = [];
  const master = entry("https://v.example/hls/master.m3u8", "hls");
  assert.equal(shouldRecord(list, master), true);
  list.push(master);

  for (const name of ["v360.m3u8", "v720.m3u8", "v1080.m3u8"]) {
    assert.equal(shouldRecord(list, entry("https://v.example/hls/" + name, "hls")), false, name);
  }
  // A player re-fetching the same playlist as it goes must not pile up either.
  assert.equal(shouldRecord(list, master), false, "the same URL again");
});

test("two videos in different directories are two entries", () => {
  const list = [entry("https://v.example/a/master.m3u8", "hls")];
  assert.equal(shouldRecord(list, entry("https://v.example/b/master.m3u8", "hls")), true);
  assert.equal(shouldRecord(list, entry("https://other.example/a/master.m3u8", "hls")), true);
});

test("media from a host already serving us a playlist is that stream's segments", () => {
  const list = [entry("https://v.example/hls/master.m3u8", "hls")];
  // Fragmented MP4 segments are video/mp4 and can be several MB each, so size
  // and type alone cannot tell them from a real file.
  assert.equal(shouldRecord(list, entry("https://v.example/hls/part1", "file")), false);
  // A different host is a different thing entirely.
  assert.equal(shouldRecord(list, entry("https://files.example/clip.mp4", "file")), true);
});

test("plain files stand on their own", () => {
  const list = [entry("https://files.example/one.mp4", "file")];
  assert.equal(shouldRecord(list, entry("https://files.example/two.mp4", "file")), true);
  assert.equal(shouldRecord(list, entry("https://files.example/one.mp4", "file")), false);
});

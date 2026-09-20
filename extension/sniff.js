"use strict";

// The rules that decide whether a response is a video worth offering, kept
// apart from the browser plumbing so they can be run against a table of cases
// instead of only against a live page.

// Below this a lone media response is a preview clip, an ad bumper or a
// notification sound rather than something worth a download manager.
const MEDIA_MIN_BYTES = 1 << 20;

const PLAYLIST_TYPE = /^(application\/(vnd\.apple\.mpegurl|x-mpegurl|mpegurl)|audio\/(x-)?mpegurl)$/i;
const DASH_TYPE = /^application\/dash\+xml$/i;
// Segments are media responses too. Without this, every .ts in a two-hour film
// would be offered as its own download.
const SEGMENT_PATH = /\.(ts|m4s|cmfv|cmfa|fmp4)$/i;
const SEGMENT_TYPE = /^video\/(mp2t|iso\.segment)$/i;

function pathOf(u) {
  try {
    return decodeURIComponent(new URL(u).pathname);
  } catch (e) {
    return "";
  }
}

function originOf(u) {
  try {
    return new URL(u).origin;
  } catch (e) {
    return "";
  }
}

// streamGroup is the directory a playlist lives in. A master playlist and the
// variant playlists it names almost always share one.
function streamGroup(u) {
  try {
    const p = new URL(u);
    return p.origin + p.pathname.replace(/[^/]*$/, "");
  } catch (e) {
    return u;
  }
}

function headerMap(list) {
  const h = {};
  for (const item of list || []) h[String(item.name).toLowerCase()] = item.value;
  return h;
}

// classifyMedia returns "hls", "dash", "file", or "" for anything that is not
// worth recording. The content type is trusted ahead of the path: a CDN often
// serves a playlist from a URL with no extension at all.
function classifyMedia(url, contentType, length) {
  const path = pathOf(url);
  const ct = String(contentType || "").split(";")[0].trim().toLowerCase();
  if (/\.m3u8?$/i.test(path) || PLAYLIST_TYPE.test(ct)) return "hls";
  if (/\.mpd$/i.test(path) || DASH_TYPE.test(ct)) return "dash";
  if (SEGMENT_PATH.test(path) || SEGMENT_TYPE.test(ct)) return "";
  if (/^(video|audio)\//i.test(ct) && length >= MEDIA_MIN_BYTES) return "file";
  return "";
}

// shouldRecord decides whether a newly seen item adds anything to what the tab
// already has. One video must not arrive as hundreds of entries.
function shouldRecord(list, item) {
  if (list.some((m) => m.url === item.url)) return false;
  if (item.kind === "file") {
    // A media response from a host already serving this tab a playlist is one
    // of that stream's segments, not a file of its own.
    return !list.some((m) => m.kind !== "file" && m.origin === item.origin);
  }
  // The master playlist is requested before the variants it names, and a
  // player re-fetches the playlist as it goes. First one in a directory wins,
  // so a stream shows up as a single entry.
  return !list.some((m) => m.kind !== "file" && m.group === item.group);
}

// mediaEntry is the shape stored per tab.
function mediaEntry(url, kind, contentType, length, title, now) {
  return {
    url: url,
    kind: kind,
    type: String(contentType || "").split(";")[0].trim().toLowerCase(),
    size: length,
    at: now,
    group: streamGroup(url),
    origin: originOf(url),
    title: title || ""
  };
}

// Node runs this file directly to test the rules; a service worker has no
// module object and skips it.
if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    MEDIA_MIN_BYTES,
    classifyMedia,
    headerMap,
    mediaEntry,
    originOf,
    pathOf,
    shouldRecord,
    streamGroup
  };
}

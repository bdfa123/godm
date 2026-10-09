# godm

[![CI](https://github.com/bdfa123/godm/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/bdfa123/godm/actions/workflows/ci.yml)

A segmented, resumable download manager that takes over browser downloads —
the IDM architecture, minus the DLL injection that modern Chrome no longer
allows. Windows is the main target; the CLI and the daemon also build for
Linux and macOS.

- **Fast and resumable**: up to 32 connections per file, written straight into
  one preallocated file, resumed per segment after a crash, a reboot or a
  dropped connection.
- **Takes over the browser**: a Chrome/Edge extension hands each download to
  godm with the cookies, referer and user agent it needs.
- **Finds video**: the extension spots HLS playlists and media on a page; godm
  downloads HLS itself and can drive yt-dlp for the sites that need it.
- **Play while it downloads**: open a file in mpv or VLC before it has
  finished, and seek anywhere — the connections follow the player.
- **Lives in the tray**, with a manager window, a queue and notifications.

```
Chrome/Edge extension  ──native messaging──>  godm.exe (thin host)
   cancels the download                            │ HTTP + bearer token
   grabs cookies / referer / UA                    ▼
                                          godm daemon (owns transfers)
                                          ├── ranged GETs, N segments
                                          ├── pwrite into one file, no merge
                                          ├── .godm sidecar for resume
                                          └── /stream/ for a media player
```

The only Go dependency is [go-webview2](https://github.com/jchv/go-webview2)
for the manager window, which is pure Go, so no cgo is needed.

## Download

Windows builds are on the [Releases page](https://github.com/bdfa123/godm/releases/latest):
`godm-<version>-windows-amd64.zip` for most PCs and `windows-arm64` for Windows
on ARM. `SHA256SUMS.txt` next to them lists the checksums.

1. Unzip it into a folder you will keep, such as `C:\Tools\godm`. The browser
   remembers where the extension was loaded from, so do not move it afterwards.
2. Double-click `godm.exe`. The manager window opens and the background service
   starts.
3. To take over browser downloads, load the `extension/` folder unpacked and run
   `godm.exe install --ext-id <id>` from a terminal in that folder. The steps
   are under [Browser takeover](#browser-takeover).

The builds are not code-signed, so Windows SmartScreen may say "Windows
protected your PC" the first time. Choose **More info**, then **Run anyway**.

## Build

```bash
go build -o godm.exe .
```

Double-clicking `godm.exe` opens the manager window and starts the daemon in
the background.

## Use it as a CLI first

```bash
./godm.exe get https://example.com/big.iso -o ~/Downloads -n 8
```

Interrupt it with Ctrl-C and run the same command again — it resumes from the
`.godm` sidecar rather than starting over.

## Browser takeover

The extension ID is generated from the unpacked folder path, so the extension
has to be loaded **before** the native host can be registered.

1. Open `chrome://extensions`, turn on **Developer mode**, click
   **Load unpacked**, and select the `extension/` folder.
2. Copy the extension ID it shows (also printed on the options page).
3. Register the native messaging host:

```bash
./godm.exe install --ext-id <paste-the-id-here>
```

4. Restart the browser.

Open the extension options page — it says whether the native host and daemon are
reachable. Download something matching the file-type list and it will land in
the godm manager instead of the browser's download bar.

A managed browser can be set to ignore per-user native hosts; `install
--machine` registers for the whole machine instead (needs an Administrator
terminal). `godm uninstall` removes the registration.

### What the extension sends

The extension decides in `chrome.downloads.onDeterminingFilename`, once the
real filename is known, cancels and erases the browser's own download, then
hands over the URL along with:

- **Cookies** via `chrome.cookies.getAll`, including httpOnly ones. Page JS can
  never read those; without them any authenticated download returns a login page.
- **Referer**, because plenty of sites check it for hotlink protection.
- **User-Agent**, matching the browser exactly.

If the handoff fails for any reason, the extension hands the download back to the
browser and shows a notification. It never silently drops a file.

Right-click a page or a selection to pick several links at once, filtered by
type, and queue them in one go.

## Video

- **Sniffing**: while you browse, the extension watches responses for HLS
  playlists, DASH manifests and large media files. The toolbar badge shows how
  many it found, and the popup lists them with their qualities.
- **HLS**: godm reads master and media playlists, fetches segments in
  parallel, decrypts AES-128, honours `EXT-X-BYTERANGE` and `EXT-X-MAP`, and
  writes segments strictly in playlist order. A quality whose audio is a
  separate rendition is refused rather than saved silent.
- **yt-dlp**: for sites that guard their video behind their own protocol, godm
  can hand the page to [yt-dlp](https://github.com/yt-dlp/yt-dlp) if it is
  installed, and still shows progress and pausing in the same list.
- DRM-protected streams (Widevine, PlayReady, FairPlay) and live streams are
  refused with a reason.

## Play while downloading

Press **Play now** on a video in the manager. godm opens
[mpv](https://mpv.io) (or VLC) on a local address, and the player reads the
file as it arrives:

- Bytes already on disk are served at once; for the rest the request waits
  while the connections are pulled to where the player reads.
- The connections line up one behind another just ahead of the playhead.
  Pieces start at 512 KB right at the player and grow to 8 MB further out, so
  the bytes needed first never wait on a single connection.
- Seeking works anywhere, including the Matroska index at the very end of the
  file that a seek reads first.
- The player reads from the daemon, not the server, so it uses none of the
  connections a host allows.

In a test against a host that allows eight connections, each slower than the
film's bitrate, jumping to a part not yet downloaded resumed playback in about
1.5 s with no stalls afterwards. Streams (HLS) and yt-dlp downloads can only be
played once they finish.

## Commands

| Command | What it does |
|---|---|
| `godm get <url>` | Download one URL, with a progress bar. Flags may go before or after the URL. |
| `godm app` | Open the manager window (what double-clicking does). |
| `godm daemon` | Run the background service (auto-started when needed). |
| `godm ui` | Open the web manager in your browser. |
| `godm status` | Print daemon state and tasks. |
| `godm install --ext-id <id>` | Register the native messaging host. |
| `godm uninstall` | Remove the registration. |

`godm get` flags: `-o <dir>`, `-n <connections>`, `-H "Name: value"` (repeatable),
`--name <filename>`. `godm daemon` flags: `-port`, `-o`, `-parallel`.

## Engine behaviour

- **Probe** is a `GET` with `Range: bytes=0-0`. HEAD is unreliable in the wild:
  CDNs 405 it, and some report `Accept-Ranges` on HEAD then ignore `Range` on GET.
  Only a real `206` plus a parseable `Content-Range` total enables segmentation.
- **Segments** are written with positional writes into a preallocated file, so
  there is no merge step and no `.part` files.
- **Dynamic splitting**: when a connection runs out of work, it takes the back
  half of the largest range still in progress, so one slow connection is never
  left alone with the end of the file. The connection count can be changed while
  a download runs.
- **Resume** is per-segment. A retry re-reads how far that segment got, so a
  dropped connection refetches only the bytes after the last successful write.
- **Retry policy** is decided on the status code. `429`, `503`, `502`, `504`,
  `408` back off and retry, honouring `Retry-After`; `403`, `404` and friends
  fail immediately, because an expired signed URL will not heal itself.
- **Connection limits**: some hosts allow only so many connections per link and
  answer the next one with `503`. A connection that keeps being refused while
  others work just leaves, and the download carries on with as many as the host
  accepts instead of failing.
- **Expired links**: a `401/403/404/410`, or a web page where the file should be,
  keeps the progress and waits for a fresh link to the same file. Click the
  download again in the browser and the waiting task adopts it.
- **Sidecar invalidation**: a changed `ETag` or size discards the old state
  rather than stitching new bytes into a stale file.
- **Never overwrites**: an existing file gets a numbered name, `name (1).ext`.
- Servers that ignore `Range` fall back to a single stream with no resume.

## Security notes

- The daemon binds `127.0.0.1` only and requires a bearer token stored at
  `%LOCALAPPDATA%\godm\token` (mode 0600).
- Requests carrying a browser `Origin` from anywhere except the extension or
  loopback are rejected outright, so a random web page cannot queue downloads.
- The extension never sees the token; the native host injects it into the
  manager URL when the popup asks for it.

## Known limits

- Firefox needs a different manifest shape (`allowed_extensions` with addon IDs)
  and is not wired up.
- DASH (`.mpd`) is detected but not downloaded: separate audio and video
  streams need merging, which most likely means ffmpeg.
- No global speed limit, scheduling, or queue priorities.
- No code signing, so SmartScreen will warn on a machine that has not seen the
  binary before.
- The browser has already started its own transfer by the time the extension
  can take it over, so its download bubble flashes briefly. Every MV3 download
  manager has this; the blocking `webRequest` API that avoided it is gone.

## Tests

```bash
go test ./...
node --test extension/sniff.test.js
```

The Go suite runs a local HTTP server that serves real `206` responses, hangs up
mid-segment, rate-limits with `429 Retry-After`, refuses connections past a limit,
and expires a signed URL mid-transfer, then checks the downloaded bytes against a
SHA-256 of the source. The playback tests stream ranges from a download in
progress and check that the connections gather where the player reads.

## License

[MIT](LICENSE). The libraries godm is built from are under their own licenses
(MIT, BSD, ISC, Apache-2.0 and MPL-2.0); their notices, and where to get the
source of the MPL-2.0 ones, are in `THIRD_PARTY_NOTICES.txt` in every release
zip, next to `LICENSE`.

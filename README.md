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
- **Torrents and magnet links**, with play-while-downloading too, and no
  uploading once a torrent is done unless you ask for it.
- **Lives in the tray**, with a manager window, a queue, notifications, speed
  limits, sorting into folders by type, and an English or Simplified Chinese
  interface.

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

The Go dependencies are [go-webview2](https://github.com/jchv/go-webview2) for
the manager window and [anacrolix/torrent](https://github.com/anacrolix/torrent)
for BitTorrent. Everything is pure Go and builds with `CGO_ENABLED=0`.

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
  godm sends them, and any `Authorization` header, only to the host the link
  was on and hosts below it, and never over plain http when the link was https:
  a redirect to a file host, or a playlist's segments on a CDN, does not get
  them. A site that needs its login on a sibling host (`files.site.com` for a
  link on `www.site.com`) will answer as if you were logged out.
- **Referer**, because plenty of sites check it for hotlink protection.
- **User-Agent**, matching the browser exactly.

If the handoff fails for any reason, the extension hands the download back to the
browser and shows a notification. It never silently drops a file.

By default a small dialog asks first, the way IDM does: change the file name,
the folder or the connection count, then **Start**; **Cancel** or closing it
downloads nothing. The folder chosen is filled in the next time; empty the
field to go back to godm's own. "Don't ask again" turns it off, and the options
page turns it back on. Links picked in bulk skip the dialog.

**Add to list** in that dialog, in **Add links**, in the window for picking
links, or on a link's right-click menu puts the download on godm's **download
list** instead, the way IDM's "Download Later" does. godm tries it at once: it
fetches about 8 MB, or for 30 seconds if the link is slow (a little more on a
fast link, since it checks a few times a second), which shows the link works,
how large the file is and how fast it comes. Then the download waits on the list's tab until
**Start** on its card, **Start all**, or the time set beside them (every day or
once; godm has to be running then, and a start missed by more than half an
hour is skipped). Tries run beside the queue, two at a time, so a list can be
built while other downloads fill every slot. A download started from the list
stays on its tab until it is done; while it waits for a slot, the list's bar
says that every slot is taken, with the number of downloads at once beside it
to raise. **Resume all** leaves the list alone.

Right-click a page or a selection to pick several links at once, filtered by
type, and queue them in one go. The list also holds the videos the page played
while you watched it, which are not in its HTML: files and HLS streams, in a
group of their own at the top (a selection leaves them out). Each stream's
playlist is read as the list opens, as the popup does, so its row says whether
it is one quality or the best of several and how long it runs, and a stream
godm cannot download (a live one, a protected one, one whose sound is kept
apart) gives its reason and cannot be ticked. A stream is queued at its best
quality.

A magnet link never turns into a download the extension could see, so a small
script in every page watches for a click on one. It acts only on a click you
make, never on one a page's own script makes, and pages that were open before
the extension was loaded need reloading first.

## Video

- **Sniffing**: while you browse, the extension watches responses for HLS
  playlists, DASH manifests and large media files. The toolbar badge shows how
  many it found, and the popup lists them with their qualities.
- **HLS**: godm reads master and media playlists, fetches segments in
  parallel, decrypts AES-128, honours `EXT-X-BYTERANGE` and `EXT-X-MAP`, and
  writes segments strictly in playlist order. A quality whose audio is a
  separate rendition is refused rather than saved silent. A master playlist
  added without a quality choice (through Add links, say) downloads the best
  one, as the extension does. A playlist behind a redirect is read from where
  it ended up.
- **yt-dlp**: for sites that guard their video behind their own protocol, godm
  can hand the page to [yt-dlp](https://github.com/yt-dlp/yt-dlp) if it is
  installed, and still shows progress and pausing in the same list. It reads
  your own yt-dlp configuration (`%APPDATA%\yt-dlp\config` and the other user
  locations) and no other: a `yt-dlp.conf` in the download folder or beside
  `yt-dlp.exe` is ignored, because a download could have put it there.
  A yt-dlp installed after godm started (with `winget install yt-dlp.yt-dlp`,
  say) is found at once: godm reads the system's PATH again from the registry
  when it does not find it on its own, and gives yt-dlp that PATH, so the Deno
  and ffmpeg that winget installs beside it are found too. No restart of the
  browser is needed. Each download works in a folder of its own,
  `.godm-<task id>`, inside the download folder, and the finished file is moved
  out of it under a name nobody is using (`name (1).ext` if there is one, as
  for any other download). What yt-dlp leaves in that folder besides the
  result (the streams `--keep-video` asks it to keep, say) is deleted with it
  when the download is done.
- **Keeping yt-dlp current**: the sites yt-dlp is used for change often, and a
  copy a few months old stops working on them with an error that does not say
  so. The **Video sites (yt-dlp)** section of Settings shows the installed
  version (`yt-dlp --version`) and the newest release, with **Check for
  updates** and **Update** buttons. The newest release is the latest stable one
  on GitHub (`api.github.com/repos/yt-dlp/yt-dlp/releases/latest`), asked for
  through godm's usual connection, so the system proxy applies; the answer is
  kept for six hours, and a check that fails is shown as failed, never as
  being up to date. How an update is done follows yt-dlp's own README
  ("Update"): a copy installed with winget (it lives under
  `...\WinGet\Packages\yt-dlp.yt-dlp_*`) is updated with `winget upgrade --id
  yt-dlp.yt-dlp`, and so is Deno with `DenoLand.Deno` when that came from winget
  too, since yt-dlp asks for a newer Deno from time to time (so **Update** stays
  available when yt-dlp is the newest but Deno is not); any other release
  binary (`yt-dlp.exe` downloaded from GitHub) updates itself with `yt-dlp -U`;
  a copy from pip, Scoop, Chocolatey, Homebrew or another package manager is
  left alone and the page gives the command to run. winget's list follows
  GitHub's by a day or so, and then the page says that the newest release is
  not in winget yet. An update is refused while a yt-dlp download or a page
  being read is running (Windows cannot replace a running `yt-dlp.exe`), and
  downloads that start while one is under way wait for it. Quitting godm from
  the tray while an update runs waits for it, for up to two minutes, rather
  than cut winget or `yt-dlp -U` off halfway through replacing the file. When a yt-dlp
  download or inspection fails and the installed version is older than the
  newest release godm knows of, the reason ends with a note that yt-dlp may be
  out of date, and where to update it. It says nothing when the versions
  cannot be compared or the newest one could not be learnt.
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
played once they finish. Torrents can be played while they download too: the
largest video in the torrent is served, the pieces just ahead of the player
come first, and only pieces whose hash has been checked reach the player.

### What is downloaded already

When the download is slower than the film, it matters which parts play
without waiting. In mpv, godm draws a map of the file over the film, a slim
bar just above mpv's own controls, and above the subtitles while there are
any: green is on disk, blue marks where the connections are downloading now
(what they bring in over the next few seconds), dark grey is still to come,
and the white tick is the playhead. The line above it says how far the film
plays from here without waiting ("Plays without waiting to 1:23:45 (4:12 from
here)"), or that this part is still downloading, and the download speed
against the rate the film needs, marked when it is too slow. It shows while
the film is paused or waiting for data, for a few seconds after the mouse
moves or a seek, and all the time while switched on with **h** (mpv binds
nothing to `h` by default; in your `input.conf` it is
`script-binding godm_map/toggle`, and a binding of your own for `h` comes
first). It speaks the language godm is set to.

- The bar is the file in bytes, and the playhead is the byte mpv is playing.
  The times are close rather than exact: the bytes on disk ahead of the
  playhead are taken to play at the film's average rate, and a film's
  bitrate varies. What mpv has already buffered counts too. Once the film
  has stopped to wait for data, the line says so, whatever is on disk
  further on.
- The overlay is a script godm writes to `%LOCALAPPDATA%\godm\mpv\` and passes
  to mpv with `--script`, alongside your own scripts and settings. It reads
  one small file there that godm rewrites about once a second while mpv
  streams the download and deletes afterwards; the script makes no network
  requests and is given no token.
- A torrent gets the same map, from its checked pieces. VLC gets none, and a
  finished file opened with Play has nothing to map.

The manager draws the same map on the card of an ordinary download that is
not done, and while a player reads it, how far the player has read and how
much is downloaded after that. A player reads ahead into its own buffer, so
that is not the picture: where the film itself is, mpv's map shows. The
card's connection count keeps the number you chose, and says when the server
allows fewer (PikPak allows eight to a link).

## Torrents and magnet links

Paste magnet links into **Add links**, pick `.torrent` files with
**Open .torrent…**, or just click a magnet link or a `.torrent` link in the
browser: the extension hands either to godm as a torrent, whatever its size.
With the download dialog on, both open it first, as a magnet chosen from the
right-click menu does; for a torrent it asks only where to save, since a torrent
names itself and is fetched from peers. With the dialog off, a clicked magnet is
added at once and a notification names what was added and the site it came
from. godm fetches the `.torrent` before it accepts it, and one it cannot get (from a
tracker that only lets a browser in, say) goes back to Chrome. Start in the
dialog downloads what the torrent holds, not the `.torrent` file; **Save the
.torrent only** hands the file back to Chrome to keep instead. Hold Ctrl or
Shift while clicking to leave a magnet to the browser, or turn off taking over
magnet links or `.torrent` files (each has its own switch) on the extension's
options page; right-clicking a magnet link and choosing
**Download with godm** works either way. A magnet shows "Fetching metadata"
until peers send the file list; if nobody answers within five minutes it stops
and frees its place in the queue, and Resume tries again.

- **Uploading**: by default a torrent stops uploading the moment it finishes.
  Settings can make it seed to a ratio of 1.0 or keep seeding. While a torrent
  downloads it uploads to its peers, as BitTorrent requires, and your IP address
  is visible to them.
- **Network**: the torrent client starts with the first torrent, listens on TCP
  port 16881 and uses UDP for DHT. It never opens a port on your router (no
  UPnP or NAT-PMP). The first torrent makes Windows Firewall ask whether godm
  may use the network; refusing still lets torrents download, from fewer peers.
- File names inside a torrent are cleaned so none can land outside the download
  folder, and names that would collide on Windows get numbered. A torrent of a
  single file is saved as that file, not in a folder of its own.
- **Disk**: on Windows a torrent's files are sparse while it downloads, as other
  downloads are, since its pieces arrive in any order, and ordinary once it is
  complete. A torrent that would not fit on the disk stops with an error as
  soon as its file list is known.

## Settings

The **Settings** dialog and the top bar hold the choices that apply to every
download:

- **Speed limit**, for all downloads together and, on each card, for one
  download. Torrents keep to the same limit on their own, so with torrents and
  other downloads running at once the total can reach about twice it; yt-dlp
  downloads are not limited, and the per-download limit is for plain files.
- **Keep this PC awake** while anything downloads (on by default). The screen
  may still turn off.
- **When all downloads finish**: sleep or shut down, once. Shutting down gives a
  60-second warning that `shutdown /a` cancels. It is never remembered across a
  restart.
- **Download folder**: where a download goes when nothing names another. Left
  empty it is the folder godm was started with (`godm daemon -o`, else
  `GODM_DOWNLOAD_DIR`, else your Downloads folder). godm makes it if it is not
  there and checks it can write to it before taking it.
- **Sort into folders by type** (off by default): Video, Music, Archives,
  Documents, Programs and Images, under the download folder, each name editable.
  A folder chosen in the download dialog always wins.
- **Language**: follow Windows, English, or 简体中文. The extension follows the
  browser's language unless its options page names English or 简体中文 (the
  name on the browser's extensions page always follows the browser).
- **After a torrent finishes**: stop uploading, seed to ratio 1.0, or keep seeding.
- **Video sites (yt-dlp)**: the installed and the newest version, a button to
  check and one to update; see *Keeping yt-dlp current* above. These two act at
  once and are not part of Save.

### The yt-dlp part of the API

Like every address under `/api/`, these need the bearer token (see *Security
notes*).

| Request | What it does |
|---|---|
| `GET /api/ytdlp` | What is known without asking anyone: whether yt-dlp was found and where, how it was installed (`winget`, `standalone`, `scoop`, `chocolatey`, `brew`, `pipx`, `pip` or `package`), whether godm updates that kind (`can_update`) or else the `command` to run by hand, the `version` and `latest` found so far (with `version_error` and `latest_error` when looking failed), `deno_via_winget` where an update brings Deno along, `outdated` (`true`, `false`, or `null` when the two cannot be compared), `busy` while yt-dlp is in use, and `updating` with the `step` and, afterwards, for half an hour, the `result` of the last update. Every error is in English; where godm worded it itself it comes with an `..._error_code` and an `..._error_arg` as well (`latest_error_code`, `error_code` of the result, ...), for a page to word it again in its own language. |
| `POST /api/ytdlp/check` | Reads the installed version and asks GitHub for the newest release, unless that was done within the last six hours; `?force=1` always looks again, and forgets how the last update went. Answers like `GET /api/ytdlp`. |
| `POST /api/ytdlp/update` | Starts an update in the background and answers at once with the status; follow it with `GET /api/ytdlp`. When it cannot start it answers `{"ok": false, "reason": ..., "error": ...}`, the reason being `missing`, `manual`, `winget`, `busy` or `updating`. |

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
  there is no merge step and no `.part` files. On Windows the file is sparse
  until it is finished: in an ordinary file NTFS fills in zeros up to a write
  before the write completes, and the connections write all over the file from
  the start, so on a hard disk the first write near the end of a big file held
  every connection for minutes. Each connection gathers what it reads and
  writes it a megabyte at a time, or at once while a player waits on it.
- **Disk space**: a sparse file takes its space only as it is written, so a
  download that would not fit is refused before it starts, counting what a
  partial file already holds, rather than found out at 90 percent. What the
  downloads and torrents already running on the same disk still have to write
  counts as taken, so of three big downloads that do not all fit, the last to
  start is refused instead of all three running out of room together part of
  the way through.
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
  accepts instead of failing. One more connection is tried after 30 seconds, and
  then ever less often up to every 5 minutes, in case the refusal was a passing
  one.
- **Stalled connections**: a connection that gets no bytes for 30 seconds
  without the server closing it is dropped and reopened from where it stopped,
  so a download never sits at 99% forever.
- **Proxy**: `HTTPS_PROXY`/`HTTP_PROXY` if set, otherwise the proxy in Windows
  settings, including its bypass list. Loopback is never proxied.
- **Expired links**: a `401/403/404/410`, or a web page where the file should be,
  keeps the progress and waits for a fresh link to the same file. Click the
  download again in the browser and the waiting task adopts it. A new address
  given to a download that was paused, queued or never got going still
  continues from the saved progress, and a stream (HLS) continues from its
  saved segments. A different stream, or another quality, is refused rather than
  saved under `name (1).ts`.
- **Sidecar invalidation**: a changed `ETag` or size discards the old state
  rather than stitching new bytes into a stale file.
- **Never overwrites**: an existing file gets a numbered name, `name (1).ext`.
- **Removing with the file**: if another program has the file open, godm keeps
  trying for 15 seconds and keeps the `.godm` sidecar until the file is gone. An
  unfinished yt-dlp download is cleaned up too: its folder, with the `.part`
  and fragment files in it, is removed as a whole.
- **Durability**: the data is flushed to disk before the sidecar that says it is
  there, about every 2 seconds. After a crash or power cut a resume may fetch up
  to that much again, and never trusts bytes that did not reach the disk.
- A server that answers `416` to the probe still gets its file (an empty file
  included), and a plain `Content-Disposition` name with percent escapes or
  encoded words is saved decoded.
- Servers that ignore `Range` fall back to a single stream with no resume.

## Security notes

- The daemon binds `127.0.0.1` only and requires a bearer token stored at
  `%LOCALAPPDATA%\godm\token` (mode 0600). The manager page holds the token, so
  it too is served only to a caller that already has it: `godm app`, `godm ui`
  and the tray put it in the address, and the page takes it out of the address
  bar once it has loaded.
- A request has to be addressed to `127.0.0.1:<port>` or `localhost:<port>`.
  Any other `Host` is refused, so a web page that points a name of its own at
  loopback (DNS rebinding) gets nothing.
- Requests carrying a browser `Origin` from anywhere except the extension or
  loopback are rejected outright, so a random web page cannot queue downloads.
- The extension never sees the token. The native host keeps it and speaks to
  the daemon for the extension, and nothing it answers carries the token or an
  address with the token in it. When the popup asks for the manager, the host
  opens the manager itself, the same way the tray icon does.

## Known limits

- Firefox needs a different manifest shape (`allowed_extensions` with addon IDs)
  and is not wired up.
- DASH (`.mpd`) is detected but not downloaded: separate audio and video
  streams need merging, which most likely means ffmpeg.
- No scheduling or queue priorities, and no per-file selection inside a torrent.
- Proxy auto-config (PAC) scripts are not read; a proxy set by address is.
- No code signing, so SmartScreen will warn on a machine that has not seen the
  binary before.
- The browser has already started its own transfer by the time the extension
  can take it over, so its download bubble flashes briefly. Every MV3 download
  manager has this; the blocking `webRequest` API that avoided it is gone.

## Tests

```bash
CGO_ENABLED=0 go test ./...
node --test "extension/*.test.js"
```

The Go suite runs a local HTTP server that serves real `206` responses, hangs up
mid-segment, rate-limits with `429 Retry-After`, refuses connections past a limit,
and expires a signed URL mid-transfer, then checks the downloaded bytes against a
SHA-256 of the source. The playback tests stream ranges from a download in
progress and check that the connections gather where the player reads. Where
mpv is installed, one test also runs the download map's script in it, with no
window and no sound, and asks it over mpv's IPC what it worked out.

## License

[MIT](LICENSE). The libraries godm is built from are under their own licenses
(MIT, BSD, ISC, Apache-2.0 and MPL-2.0); their notices, and where to get the
source of the MPL-2.0 ones, are in `THIRD_PARTY_NOTICES.txt` in every release
zip, next to `LICENSE`.

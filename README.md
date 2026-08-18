# godm

A segmented, resumable download manager that takes over browser downloads —
the IDM architecture, minus the DLL injection that modern Chrome no longer allows.

```
Chrome/Edge extension  ──native messaging──>  godm.exe (thin host)
   cancels the download                            │ HTTP + bearer token
   grabs cookies / referer / UA                    ▼
                                          godm daemon (owns transfers)
                                          ├── ranged GETs, N segments
                                          ├── pwrite into one file, no merge
                                          └── .godm sidecar for resume
```

Zero external Go dependencies — everything is stdlib.

## Build

```bash
go build -o godm.exe .
```

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

`godm uninstall` removes the registry entries.

### What the extension sends

`chrome.downloads.onCreated` fires, the extension cancels and erases the
browser's own download, then hands over the URL along with:

- **Cookies** via `chrome.cookies.getAll`, including httpOnly ones. Page JS can
  never read those; without them any authenticated download returns a login page.
- **Referer**, because plenty of sites check it for hotlink protection.
- **User-Agent**, matching the browser exactly.

If the handoff fails for any reason, the extension hands the download back to the
browser and shows a notification. It never silently drops a file.

## Commands

| Command | What it does |
|---|---|
| `godm get <url>` | Download one URL, with a progress bar. Flags may go before or after the URL. |
| `godm daemon` | Run the background service (auto-started when needed). |
| `godm ui` | Open the web manager in your browser. |
| `godm status` | Print daemon state and tasks. |
| `godm install --ext-id <id>` | Register the native messaging host. |
| `godm uninstall` | Remove the registration. |

`godm get` flags: `-o <dir>`, `-n <connections>`, `-H "Name: value"` (repeatable),
`--name <filename>`.

## Engine behaviour

- **Probe** is a `GET` with `Range: bytes=0-0`. HEAD is unreliable in the wild:
  CDNs 405 it, and some report `Accept-Ranges` on HEAD then ignore `Range` on GET.
  Only a real `206` plus a parseable `Content-Range` total enables segmentation.
- **Segments** are written with positional writes into a preallocated file, so
  there is no merge step and no `.part` files.
- **Resume** is per-segment. A retry re-reads how far that segment got, so a
  dropped connection refetches only the bytes after the last successful write.
- **Retry policy** is decided on the status code. `429`, `503`, `502`, `504`,
  `408` back off and retry, honouring `Retry-After`; `403`, `404` and friends
  fail immediately, because an expired signed URL will not heal itself.
- **Sidecar invalidation**: a changed `ETag` or size discards the old state
  rather than stitching new bytes into a stale file.
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
- No m3u8/DASH video sniffing yet — that is a separate, much larger job
  (playlist parsing, AES-128 decryption, ffmpeg muxing).
- No global speed limit, scheduling, or queue priorities.
- No code signing, so SmartScreen will warn on a machine that has not seen the
  binary before.
- `chrome.downloads.onCreated` fires after the browser has already started the
  transfer, so the download shelf flashes briefly before the takeover. Every
  MV3 download manager has this; the blocking `webRequest` API that avoided it
  is gone.

## Tests

```bash
go test ./...
```

The suite runs a local HTTP server that serves real `206` responses, hangs up
mid-segment, rate-limits with `429 Retry-After`, and expires a signed URL
mid-transfer, then checks the downloaded bytes against a SHA-256 of the source.

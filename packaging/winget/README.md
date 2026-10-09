# winget manifests

Templates for publishing godm to the Windows Package Manager community
repository, [microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs),
as `bdfa123.godm`. Nothing here is submitted automatically.

| File | What it is |
|---|---|
| `bdfa123.godm.yaml` | The version manifest. |
| `bdfa123.godm.installer.yaml` | The installers: each release zip is a `zip` holding a `portable` `godm.exe`. |
| `bdfa123.godm.locale.en-US.yaml` | The name, description, license and links. |

The three files go together. They use these placeholders:

| Placeholder | Fill in with |
|---|---|
| `__VERSION__` | The version without the `v`, so `0.1.0` for the tag `v0.1.0`. It appears in the URLs too, where the `v` is already written. |
| `__RELEASE_DATE__` | The release date as `YYYY-MM-DD`. |
| `__SHA256_AMD64__`, `__SHA256_ARM64__` | The checksums of the two zips, from the release's `SHA256SUMS.txt`. |

## After a release

Do this once the release is published, because winget's checks download the
zips from the URLs in the manifest and compare their hashes.

1. Copy the three files into an empty folder and replace the placeholders.
2. Check them: `winget validate --manifest <folder>`. To try the install, run
   `winget settings --enable LocalManifestFiles` once, then
   `winget install --manifest <folder>`.
3. Submit the first version with [wingetcreate](https://github.com/microsoft/winget-create)
   (`winget install Microsoft.WingetCreate`):

   ```powershell
   wingetcreate submit --prtitle "New package: bdfa123.godm version 0.1.0" <folder>
   ```

   It asks you to sign in to GitHub and opens a pull request on winget-pkgs. You
   can also fork winget-pkgs, put the three files in
   `manifests/b/bdfa123/godm/0.1.0/` and open the pull request yourself.
4. Wait for the pull request's checks (schema, URL, hash and a malware scan)
   and for a moderator. The builds are unsigned, so if the scan complains the
   pull request thread is where it gets sorted out.

## Later releases

Once the first version is merged, `wingetcreate` can update the package from the
new release's zips, and open the pull request in the same step:

```powershell
wingetcreate update bdfa123.godm --version 0.2.0 `
  --urls https://github.com/bdfa123/godm/releases/download/v0.2.0/godm-v0.2.0-windows-amd64.zip `
         https://github.com/bdfa123/godm/releases/download/v0.2.0/godm-v0.2.0-windows-arm64.zip `
  --submit
```

It takes the architecture from `amd64` and `arm64` in the file names and copies
everything else from the previous version, so these templates only matter for
the first submission, or when the packaging changes.

## What winget users get

`winget install bdfa123.godm` unpacks the zip into winget's package folder and
puts a `godm` command on the PATH. The browser extension is in the `extension`
folder beside the real `godm.exe`, to be loaded unpacked as the README describes.
That layout has not been tried yet, so check it with the first release.

## Install

Download the zip that matches your PC, unzip it anywhere and double-click `godm.exe`:

- `godm-<tag>-windows-amd64.zip` for most PCs
- `godm-<tag>-windows-arm64.zip` for Windows on ARM

To take over browser downloads, load the `extension/` folder unpacked in Chrome or Edge, then run `godm.exe install --ext-id <the id the browser shows>`. The README in the zip has the details.

## Unsigned builds

These builds are **not code-signed**. Windows SmartScreen may show "Windows protected your PC" the first time you run `godm.exe`: choose **More info**, then **Run anyway**.

To check that a download is intact, compare it with `SHA256SUMS.txt`:

```powershell
Get-FileHash .\godm-<tag>-windows-amd64.zip -Algorithm SHA256
```


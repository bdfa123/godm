<#
.SYNOPSIS
  Builds the godm Windows release zips and SHA256SUMS.txt.

.DESCRIPTION
  This is what the release workflow (.github/workflows/release.yml) runs, so a
  local run produces the same files a tagged release does. For each
  architecture it embeds the icon, the version info and an application
  manifest with go-winres, builds godm.exe, and packs godm-<version>-windows-<arch>.zip
  with godm.exe, extension/, README.md, LICENSE and THIRD_PARTY_NOTICES.txt at
  the top level.

  The extension in the zip is stamped with the release's version: v1.2.3 makes
  its manifest.json say 1.2.3, so Chrome's extension page shows which release
  it came from. A tag with a suffix, such as v1.2.3-rc.1, gives 1.2.3 for the
  number Chrome compares (it only accepts up to four integers) and the whole
  tag as the version_name it displays. Only the copy in the zip is changed, not
  the one in the repository, and a version that is not vMAJOR.MINOR.PATCH (a
  plain `git describe` hash, say) leaves it as it is.

  THIRD_PARTY_NOTICES.txt is written by scripts/notices from the modules the
  exe really links and their license files in the module cache. It stops the
  release when it meets a license it does not know, because the licenses of
  those modules require their notices to ship with the exe.

  The exe stays a console-subsystem binary on purpose: the native messaging
  host and the CLI need the console, and godm hides it itself when it is
  double-clicked. The manifest is go-winres' "cli" one, which keeps the process
  system-DPI-aware (what the tray code already asks for at run time) and
  leaves out the per-monitor DPI and common-controls settings the code was not
  written for.

.PARAMETER Version
  Version baked into the exe and used in the file names, such as v0.1.0.
  Defaults to `git describe --tags --always --dirty`, or "dev" outside git.

.PARAMETER OutDir
  Where the zips and SHA256SUMS.txt go. Defaults to dist\ in the repository,
  which is git-ignored.

.PARAMETER Arch
  GOARCH values to build for. Defaults to amd64 and arm64. Given as -Arch
  amd64,arm64 it works under powershell -File as well, which hands the list
  over as one string.

.PARAMETER RequireWinres
  Fail when go-winres cannot be found instead of building without the icon and
  version info. The release workflow sets this so a release is never published
  without them.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1 -Version v0.1.0
#>
[CmdletBinding()]
param(
    [string]$Version,
    [string]$OutDir,
    [string[]]$Arch = @('amd64', 'arm64'),
    [switch]$RequireWinres
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Run as powershell -File script.ps1 -Arch amd64,arm64, PowerShell does not
# make a list of the comma: the parameter arrives as the one string
# "amd64,arm64", and that would be taken for a GOARCH. Split it, whichever way
# the script was started.
$Arch = @($Arch | ForEach-Object { "$_" -split '[,;\s]+' } | Where-Object { $_ } | Select-Object -Unique)
if ($Arch.Count -eq 0) { throw '-Arch needs at least one architecture, such as amd64 or arm64.' }
foreach ($a in $Arch) {
    # It ends up in an environment variable and in file names.
    if ($a -notmatch '^[0-9a-z]+$') { throw "Architecture '$a' is not a GOARCH name such as amd64 or arm64." }
}

# Keep in step with GO_WINRES_VERSION in .github/workflows/release.yml.
$WinresVersion = 'v0.3.3'

$root = Split-Path -Parent $PSScriptRoot

if (-not $OutDir) {
    $OutDir = Join-Path $root 'dist'
}
elseif (-not [System.IO.Path]::IsPathRooted($OutDir)) {
    $OutDir = Join-Path (Get-Location).Path $OutDir
}

if (-not $Version) {
    try {
        $Version = (& git -C $root describe --tags --always --dirty 2>$null)
        if ($LASTEXITCODE -ne 0) { $Version = $null }
    }
    catch {
        $Version = $null
    }
    if (-not $Version) { $Version = 'dev' }
}
$Version = "$Version".Trim()
# The version ends up in a linker flag and in file names, so keep it plain.
if ($Version -notmatch '^[0-9A-Za-z][0-9A-Za-z._+-]*$') {
    throw "Version '$Version' has characters that do not belong in a file name or a linker flag."
}

# The numeric file version has to be four numbers; anything that is not
# vMAJOR.MINOR.PATCH gets zeros rather than a guess.
$fileVersion = '0.0.0.0'
$productVersion = '0.0.0.0'
if ($Version -match '^v?(\d+)\.(\d+)\.(\d+)') {
    $fileVersion = '{0}.{1}.{2}.0' -f $Matches[1], $Matches[2], $Matches[3]
    $productVersion = $Version -replace '^v', ''
}

# What the extension's manifest should say for this release, or $null to leave
# it alone. Chrome takes "version" as one to four integers from 0 to 65535 and
# nothing else, and shows "version_name" instead when there is one. So v1.2.3 is
# 1.2.3, and a suffix (v1.2.3-rc.1, or the v1.2.3-4-gabc1234 that git describe
# makes) is dropped from the number and kept in the name. Leading zeros are not
# allowed in the number, so it is rebuilt from the integers.
function Get-ExtensionVersion {
    param([string]$Version)
    if ($Version -notmatch '^v?(\d+)\.(\d+)\.(\d+)(.*)$') { return $null }
    $parts = foreach ($digits in $Matches[1], $Matches[2], $Matches[3]) {
        $n = 0
        if (-not [int]::TryParse($digits, [ref]$n) -or $n -gt 65535) {
            throw "Version '$Version' has a number ($digits) that Chrome does not accept in an extension version; each must be 0 to 65535."
        }
        $n
    }
    $number = $parts -join '.'
    $name = $null
    if ($Matches[4]) { $name = $Version -replace '^v', '' }
    return [pscustomobject]@{ Number = $number; Name = $name }
}

$extStamp = Get-ExtensionVersion $Version

# Writes the version into a copy of manifest.json, as text, so that the rest of
# the file keeps its layout and line endings.
function Set-ExtensionVersion {
    param([string]$ManifestPath, $Stamp)
    $text = [System.IO.File]::ReadAllText($ManifestPath)
    if ($text -match '"version_name"') {
        throw "$ManifestPath already has a version_name; teach scripts/build-release.ps1 how to stamp it."
    }
    $line = [regex]'(?m)^([ \t]*)"version"[ \t]*:[ \t]*"[^"]*"'
    if (-not $line.IsMatch($text)) { throw "$ManifestPath has no `"version`" line to stamp." }
    $replacement = '${1}"version": "' + $Stamp.Number + '"'
    if ($Stamp.Name) {
        $newline = if ($text.Contains("`r`n")) { "`r`n" } else { "`n" }
        $replacement += ',' + $newline + '${1}"version_name": "' + $Stamp.Name + '"'
    }
    $text = $line.Replace($text, $replacement, 1)
    [System.IO.File]::WriteAllText($ManifestPath, $text, (New-Object System.Text.UTF8Encoding($false)))
    $written = (Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json).version
    if ($written -ne $Stamp.Number) { throw "$ManifestPath says version '$written' after stamping, expected '$($Stamp.Number)'." }
}

function Find-Winres {
    $cmd = Get-Command go-winres -CommandType Application -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $gopath = ((& go env GOPATH) -split [System.IO.Path]::PathSeparator)[0]
    if ($gopath) {
        $candidate = Join-Path (Join-Path $gopath 'bin') 'go-winres.exe'
        if (Test-Path -LiteralPath $candidate) { return $candidate }
    }
    return $null
}

Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem

# Packs the contents of a directory (not the directory itself) into a zip.
# ZipFile.CreateFromDirectory and Compress-Archive write backslashes into the
# entry names on Windows PowerShell 5.1, which other unzip tools take as part
# of the file name, so the entries are named by hand with forward slashes.
function New-ZipFromDirectory {
    param([string]$Directory, [string]$ZipPath)
    $base = (Resolve-Path -LiteralPath $Directory).Path.TrimEnd('\')
    $files = Get-ChildItem -LiteralPath $base -Recurse -File |
        Sort-Object -Property FullName
    $stream = [System.IO.File]::Open($ZipPath, [System.IO.FileMode]::CreateNew)
    try {
        $archive = New-Object System.IO.Compression.ZipArchive($stream, [System.IO.Compression.ZipArchiveMode]::Create)
        try {
            foreach ($file in $files) {
                $entryName = $file.FullName.Substring($base.Length + 1).Replace('\', '/')
                [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile(
                    $archive, $file.FullName, $entryName, [System.IO.Compression.CompressionLevel]::Optimal) | Out-Null
            }
        }
        finally {
            $archive.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }
}

$savedEnv = @{}
foreach ($name in 'GOOS', 'GOARCH', 'CGO_ENABLED') {
    $savedEnv[$name] = [System.Environment]::GetEnvironmentVariable($name)
}

# Scratch space for assembling each zip, named so it cannot be mistaken for
# anything already in a shared output folder.
$stageRoot = Join-Path $OutDir ('.stage-' + [guid]::NewGuid().ToString('N').Substring(0, 8))

Push-Location $root
try {
    if (-not (Test-Path -LiteralPath 'assets\godm.ico')) {
        throw 'assets\godm.ico is missing.'
    }

    $winres = Find-Winres
    if (-not $winres) {
        $how = "Install it with: go install github.com/tc-hib/go-winres@$WinresVersion"
        if ($RequireWinres) {
            throw "go-winres was not found on PATH or in GOPATH\bin, and -RequireWinres is set. $how"
        }
        Write-Warning ("go-winres was not found on PATH or in GOPATH\bin, so this build has NO icon, " +
            "version info or manifest. $how")
    }

    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

    Write-Host "godm $Version  (file version $fileVersion)  ->  $OutDir"

    if ($winres) {
        Write-Host "go-winres: $winres"
        # One .syso per architecture, named so that go build only links the
        # one that matches the target.
        $winresArgs = @(
            'simply',
            '--icon', 'assets/godm.ico',
            '--manifest', 'cli',
            '--arch', ($Arch -join ','),
            '--file-version', $fileVersion,
            '--product-version', $productVersion,
            '--file-description', 'godm download manager',
            '--product-name', 'godm',
            '--original-filename', 'godm.exe',
            '--copyright', 'Copyright (c) 2026 bdfa123'
        )
        & $winres @winresArgs
        if ($LASTEXITCODE -ne 0) { throw "go-winres failed with exit code $LASTEXITCODE" }
    }

    # Made once for all the architectures, outside the folders that get zipped.
    # It runs on this machine, whatever GOOS and GOARCH the caller has set; the
    # target is given as flags instead.
    New-Item -ItemType Directory -Force -Path $stageRoot | Out-Null
    $notices = Join-Path $stageRoot 'THIRD_PARTY_NOTICES.txt'
    $env:GOOS = $null
    $env:GOARCH = $null
    $env:CGO_ENABLED = '0'
    & go run ./scripts/notices -o $notices -goos windows -goarch ($Arch -join ',')
    if ($LASTEXITCODE -ne 0) { throw "scripts/notices failed with exit code $LASTEXITCODE, so there are no third-party notices to ship" }

    $zips = @()
    foreach ($a in $Arch) {
        Write-Host ''
        Write-Host "== windows/$a"
        $stage = Join-Path $stageRoot $a
        New-Item -ItemType Directory -Force -Path $stage | Out-Null
        $exe = Join-Path $stage 'godm.exe'

        $env:GOOS = 'windows'
        $env:GOARCH = $a
        $env:CGO_ENABLED = '0'
        & go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $exe .
        if ($LASTEXITCODE -ne 0) { throw "go build failed for windows/$a" }

        if ($winres) {
            $info = (Get-Item -LiteralPath $exe).VersionInfo
            if ($info.ProductName -ne 'godm') {
                throw "godm.exe for $a has no version info: go-winres ran but its resources were not linked in."
            }
            Write-Host ("   version info: {0} {1}, file version {2}" -f $info.ProductName, $info.ProductVersion, $info.FileVersion)
        }

        # An unsigned exe that has never run can only be checked here on the
        # machine's own architecture.
        if ($env:PROCESSOR_ARCHITECTURE -eq 'AMD64' -and $a -eq 'amd64') {
            $said = (& $exe version | Out-String).Trim()
            if ($said -ne "godm $Version") {
                throw "godm.exe version printed '$said', expected 'godm $Version'"
            }
            Write-Host "   godm.exe version -> $said"
        }

        Copy-Item -LiteralPath 'README.md', 'LICENSE', $notices -Destination $stage
        Copy-Item -LiteralPath 'extension' -Destination (Join-Path $stage 'extension') -Recurse
        # The unit tests and the icon generator are for developers, not for a
        # user loading the extension unpacked.
        Get-ChildItem -LiteralPath (Join-Path $stage 'extension') -Recurse -File |
            Where-Object { $_.Name -like '*.test.js' -or $_.Extension -eq '.py' } |
            Remove-Item -Force
        # The copy in the stage, never the one in the repository.
        if ($extStamp) {
            Set-ExtensionVersion -ManifestPath (Join-Path $stage 'extension\manifest.json') -Stamp $extStamp
        }

        $zip = Join-Path $OutDir ("godm-{0}-windows-{1}.zip" -f $Version, $a)
        if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
        New-ZipFromDirectory -Directory $stage -ZipPath $zip

        $archive = [System.IO.Compression.ZipFile]::OpenRead($zip)
        $packedManifest = $null
        try {
            $names = @($archive.Entries | ForEach-Object { $_.FullName })
            $entry = $archive.GetEntry('extension/manifest.json')
            if ($entry) {
                $reader = New-Object System.IO.StreamReader($entry.Open(), [System.Text.Encoding]::UTF8)
                try { $packedManifest = $reader.ReadToEnd() | ConvertFrom-Json } finally { $reader.Dispose() }
            }
        }
        finally {
            $archive.Dispose()
        }
        foreach ($needed in 'godm.exe', 'README.md', 'LICENSE', 'THIRD_PARTY_NOTICES.txt', 'extension/manifest.json') {
            if ($names -notcontains $needed) { throw "$zip is missing $needed" }
        }
        if ($names | Where-Object { $_ -match '\\' }) { throw "$zip has backslashes in entry names" }
        if ($extStamp -and $packedManifest.version -ne $extStamp.Number) {
            throw "$zip has an extension that says version '$($packedManifest.version)', expected '$($extStamp.Number)'"
        }
        Write-Host ("   {0}: {1} entries, extension version {2}" -f (Split-Path -Leaf $zip), $names.Count, $packedManifest.version)
        $zips += $zip
    }

    # Same layout as sha256sum, so `sha256sum -c SHA256SUMS.txt` works on the
    # downloaded files. LF line endings and no byte order mark, for that reason.
    $sums = foreach ($zip in $zips) {
        '{0}  {1}' -f (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant(), (Split-Path -Leaf $zip)
    }
    $sumsFile = Join-Path $OutDir 'SHA256SUMS.txt'
    [System.IO.File]::WriteAllText($sumsFile, ((@($sums) -join "`n") + "`n"), (New-Object System.Text.UTF8Encoding($false)))

    Write-Host ''
    Write-Host 'Done:'
    foreach ($zip in $zips) {
        Write-Host ("  {0}  ({1:N1} MB)" -f $zip, ((Get-Item -LiteralPath $zip).Length / 1MB))
    }
    Write-Host "  $sumsFile"
    if (-not $winres) {
        Write-Warning 'These zips were built WITHOUT the icon, version info and manifest (go-winres missing). Do not publish them.'
    }
}
finally {
    foreach ($name in $savedEnv.Keys) {
        [System.Environment]::SetEnvironmentVariable($name, $savedEnv[$name])
    }
    Get-ChildItem -LiteralPath $root -Filter 'rsrc_windows_*.syso' -ErrorAction SilentlyContinue | Remove-Item -Force
    if (Test-Path -LiteralPath $stageRoot) { Remove-Item -LiteralPath $stageRoot -Recurse -Force }
    Pop-Location
}

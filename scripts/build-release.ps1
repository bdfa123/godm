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
  GOARCH values to build for. Defaults to amd64 and arm64.

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

        $zip = Join-Path $OutDir ("godm-{0}-windows-{1}.zip" -f $Version, $a)
        if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
        New-ZipFromDirectory -Directory $stage -ZipPath $zip

        $archive = [System.IO.Compression.ZipFile]::OpenRead($zip)
        try {
            $names = @($archive.Entries | ForEach-Object { $_.FullName })
        }
        finally {
            $archive.Dispose()
        }
        foreach ($needed in 'godm.exe', 'README.md', 'LICENSE', 'THIRD_PARTY_NOTICES.txt', 'extension/manifest.json') {
            if ($names -notcontains $needed) { throw "$zip is missing $needed" }
        }
        if ($names | Where-Object { $_ -match '\\' }) { throw "$zip has backslashes in entry names" }
        Write-Host ("   {0}: {1} entries" -f (Split-Path -Leaf $zip), $names.Count)
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

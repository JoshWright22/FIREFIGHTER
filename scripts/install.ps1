# firefighter installer for Windows:
#   irm https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/main/scripts/install.ps1 | iex
# Puts firefighter.exe in %LOCALAPPDATA%\Programs\firefighter, adds it to your PATH, then runs
# `firefighter setup` (start at logon, MCP for agent CLIs, shared-machine rules in their global
# instructions). Undo: firefighter uninstall.
$ErrorActionPreference = 'Stop'
# Not from an admin shell: the old binary it stops is user-writable, and setup edits your own
# configs. firefighter runs as you.
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if ($admin -and $env:FIREFIGHTER_ALLOW_ELEVATED -ne '1') {
    Write-Host 'Run this from a normal (non-administrator) PowerShell window. firefighter runs as you.'
    return
}
$repo = 'JoshWright22/FIREFIGHTER'
# FIREFIGHTER_INSTALL_DIR and FIREFIGHTER_NO_SETUP=1 are for trying the installer out: another
# folder, and no PATH change or `firefighter setup` at the end.
$dir = if ($env:FIREFIGHTER_INSTALL_DIR) { $env:FIREFIGHTER_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\firefighter' }
New-Item -ItemType Directory -Force $dir | Out-Null
$exe = Join-Path $dir 'firefighter.exe'
# Old copies from earlier upgrades: gone once nothing runs them any more.
Get-ChildItem $dir -Filter 'firefighter.exe.old*' -ErrorAction SilentlyContinue | ForEach-Object {
    try { Remove-Item $_.FullName -Force -ErrorAction Stop } catch {}
}

# Agent sessions keep firefighter.exe open (its MCP server), so an upgrade renames the running exe
# (Windows allows that) to a unique name instead of overwriting it, and stops the old daemon cleanly.
function Install-Exe($src) {
    if (Test-Path $exe) {
        try { $null = & $exe daemon --stop 2>&1 } catch {}
        Move-Item $exe "$exe.old-$([DateTime]::Now.ToString('yyyyMMddHHmmss'))" -Force
    }
    Copy-Item $src $exe -Force
}

# Runs a native program and returns its exit code and everything it printed. Windows PowerShell 5.1
# turns a native program's stderr into a terminating error under $ErrorActionPreference = 'Stop'
# (so `gh ... 2>&1` threw before its exit code could be read): the preference is lowered around it.
function Invoke-Native($file, [string[]]$arguments) {
    $old = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = (& $file @arguments 2>&1 | ForEach-Object { "$_" }) -join "`n"
        return @{ Code = $LASTEXITCODE; Out = $out }
    } catch {
        return @{ Code = 1; Out = "$_" }
    } finally {
        $ErrorActionPreference = $old
    }
}

# Signed build provenance, when the GitHub CLI is installed and signed in: 'verified', 'none' (this
# file has no attestation: a release from before signing, or one made by hand), 'failed' (it has
# one that doesn't check out), or 'skipped'.
function Test-Provenance($file) {
    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { return 'skipped' }
    if ((Invoke-Native 'gh' @('auth', 'status')).Code -ne 0) { return 'skipped' }
    $r = Invoke-Native 'gh' @('attestation', 'verify', $file, '--repo', $repo)
    if ($r.Code -eq 0) { return 'verified' }
    if ($r.Out -match 'HTTP 404|no attestations? found|Not Found') { return 'none' }
    Write-Host $r.Out
    return 'failed'
}

# Whether Windows lets the downloaded program run at all. Antivirus (Microsoft Defender's
# machine-learning rules) sometimes blocks or deletes a new unsigned release.
function Test-Runs($path) {
    if (-not (Test-Path $path)) { return $false }
    return (Invoke-Native $path @('version')).Code -eq 0
}

# The same release built on this PC from its tagged source, when Go is installed: a local build is
# the user's own program, not a download with no reputation, and antivirus leaves it alone.
function Build-FromSource($out) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { return $false }
    $tag = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest" -UseBasicParsing).tag_name
    if (-not $tag) { return $false }
    Write-Host "building firefighter $tag from source with Go (a minute or two)"
    $oldBin = $env:GOBIN
    $env:GOBIN = $out
    try {
        $r = Invoke-Native 'go' @('install', '-ldflags', "-X main.version=$($tag.TrimStart('v'))", "github.com/JoshWright22/firefighter/cmd/firefighter@$tag")
    } finally {
        $env:GOBIN = $oldBin
    }
    if ($r.Code -ne 0) { Write-Host $r.Out }
    return ($r.Code -eq 0) -and (Test-Path (Join-Path $out 'firefighter.exe'))
}

if ($env:FIREFIGHTER_LOCAL_BINARY) {
    Install-Exe $env:FIREFIGHTER_LOCAL_BINARY
} else {
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
    $name = "firefighter_windows_$arch.zip"
    $base = "https://github.com/$repo/releases/latest/download"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("ff-" + [guid]::NewGuid())
    New-Item -ItemType Directory $tmp | Out-Null
    try {
        Write-Host "downloading $name"
        Invoke-WebRequest "$base/$name" -OutFile "$tmp\$name" -UseBasicParsing
        Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt" -UseBasicParsing
        $want = ((Get-Content "$tmp\checksums.txt") | Where-Object { $_ -match " $([regex]::Escape($name))$" }).Split(' ')[0]
        $got = (Get-FileHash "$tmp\$name" -Algorithm SHA256).Hash.ToLower()
        if ($want -ne $got) { throw "checksum mismatch for $name" }
        # With the GitHub CLI signed in, also check the release's signed build provenance. A release
        # with none (made before releases were signed) still installs on its checksum, with a note;
        # FIREFIGHTER_REQUIRE_PROVENANCE=1 refuses it.
        switch (Test-Provenance "$tmp\$name") {
            'verified' { Write-Host 'verified signed build provenance' }
            'failed' { throw "provenance check failed for $name" }
            'none' {
                if ($env:FIREFIGHTER_REQUIRE_PROVENANCE -eq '1') { throw "no signed build provenance for $name" }
                Write-Host "note: this release has no signed build provenance for $name; installing on its checksum alone"
            }
        }
        $src = "$tmp\firefighter.exe"
        try { Expand-Archive "$tmp\$name" -DestinationPath $tmp -Force } catch { Write-Host "could not unpack ${name}: $_" }
        if (-not (Test-Runs $src)) {
            # Blocked or removed on arrival. Don't ask for an antivirus exclusion: build the same
            # release from source instead, when Go is here.
            Write-Host 'Windows security blocked the downloaded firefighter.exe (a false positive on a new unsigned program).'
            $built = Join-Path $tmp 'built'
            New-Item -ItemType Directory $built | Out-Null
            if (-not (Build-FromSource $built)) {
                throw "firefighter.exe was blocked by antivirus. Install Go (https://go.dev/dl) and run this installer again to build it from source, or report the false positive: https://github.com/$repo/issues"
            }
            $src = Join-Path $built 'firefighter.exe'
        }
        Install-Exe $src
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}

if ($env:FIREFIGHTER_NO_SETUP -eq '1') {
    Write-Host "installed $exe (no PATH change, no setup)"
    return
}
# Edit the raw user PATH (keeps %VAR% entries unexpanded) and write it back as REG_EXPAND_SZ.
$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
$raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
if (($raw -split ';') -notcontains $dir) {
    $new = if ($raw) { $raw.TrimEnd(';') + ";$dir" } else { $dir }
    $key.SetValue('Path', $new, [Microsoft.Win32.RegistryValueKind]::ExpandString)
}
$key.Close()
$env:Path = "$dir;$env:Path"
& $exe setup
if ($LASTEXITCODE -ne 0) { throw "firefighter setup failed" }

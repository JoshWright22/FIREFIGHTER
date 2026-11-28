# firefighter installer for Windows:
#   irm https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/v0.3.3/scripts/install.ps1 | iex
# Installs firefighter.exe in %LOCALAPPDATA%\Programs\firefighter, adds it to PATH and creates a
# per-user Startup shortcut. Then it runs setup for MCP and agent rules. Undo: firefighter uninstall.
$ErrorActionPreference = 'Stop'
# Not from an admin shell: the old binary it stops is user-writable, and setup edits your own
# configs. firefighter runs as you.
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if ($admin -and $env:FIREFIGHTER_ALLOW_ELEVATED -ne '1') {
    Write-Host 'Run this from a normal (non-administrator) PowerShell window. firefighter runs as you.'
    return
}
$repo = 'JoshWright22/FIREFIGHTER'
# The release this script installs: pinned, never "latest" (security audit H1: a new tag alone must
# not reach new installs). release.yml refuses to publish a tag this line doesn't name.
$version = if ($env:FIREFIGHTER_VERSION) { $env:FIREFIGHTER_VERSION } else { 'v0.3.3' }
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
    # The logon launcher shipped alongside the CLI in current Windows releases.
    $w = Join-Path (Split-Path $src) 'firefighterw.exe'
    if (Test-Path $w) { Copy-Item $w (Join-Path $dir 'firefighterw.exe') -Force }
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

function Install-LogonShortcut {
    $startup = [Environment]::GetFolderPath([Environment+SpecialFolder]::Startup, [Environment+SpecialFolderOption]::Create)
    if (-not $startup) { throw 'Could not locate the Startup folder for this user.' }
    $link = Join-Path $startup 'firefighter.lnk'
    $launcher = Join-Path $dir 'firefighterw.exe'
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut($link)
    if (Test-Path -LiteralPath $launcher -PathType Leaf) {
        $shortcut.TargetPath = $launcher
        $shortcut.Arguments = ''
    } else {
        $shortcut.TargetPath = $exe
        $shortcut.Arguments = 'daemon'
    }
    $shortcut.WorkingDirectory = $dir
    $shortcut.Description = 'Start the FIREFIGHTER guard at sign-in'
    $shortcut.Save()
    Write-Host "set up logon startup at $link"

    # Migrate entries created by previous versions after the new shortcut has been saved.
    $runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
    if (Get-ItemProperty -LiteralPath $runKey -Name 'firefighter' -ErrorAction SilentlyContinue) {
        Remove-ItemProperty -LiteralPath $runKey -Name 'firefighter'
    }
    $task = Get-ScheduledTask -TaskName 'firefighter' -TaskPath '\' -ErrorAction SilentlyContinue
    if ($task) { Unregister-ScheduledTask -InputObject $task -Confirm:$false }
}

if ($env:FIREFIGHTER_LOCAL_BINARY) {
    Install-Exe $env:FIREFIGHTER_LOCAL_BINARY
} else {
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
    $name = "firefighter_windows_$arch.zip"
    $base = "https://github.com/$repo/releases/download/$version"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("ff-" + [guid]::NewGuid())
    New-Item -ItemType Directory $tmp | Out-Null
    try {
        Write-Host "downloading $name"
        Invoke-WebRequest "$base/$name" -OutFile "$tmp\$name" -UseBasicParsing
        Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt" -UseBasicParsing
        $want = ((Get-Content "$tmp\checksums.txt") | Where-Object { $_ -match " $([regex]::Escape($name))$" }).Split(' ')[0]
        $got = (Get-FileHash "$tmp\$name" -Algorithm SHA256).Hash.ToLower()
        if ($want -ne $got) { throw "checksum mismatch for $name" }
        # With the GitHub CLI signed in, also check the release's signed build provenance. Every release
        # this installer can fetch (v0.2.0 on) is signed, so a file with none is refused: someone who
        # could edit the release would replace the archive and checksums.txt, and a missing
        # attestation is exactly what that looks like (review 11/20).
        switch (Test-Provenance "$tmp\$name") {
            'verified' { Write-Host 'verified signed build provenance' }
            'failed' { throw "provenance check failed for $name" }
            'none' { throw "no signed build provenance for ${name}: refusing to install it (report it: https://github.com/$repo/issues)" }
        }
        $src = "$tmp\firefighter.exe"
        Expand-Archive "$tmp\$name" -DestinationPath $tmp -Force
        $r = Invoke-Native $src @('version')
        if ($r.Code -ne 0 -or $r.Out.Trim() -ne "firefighter $($version.TrimStart('v'))") {
            throw "firefighter.exe from $version did not pass the launch check. Check Windows Security Protection History and report the result at https://github.com/$repo/issues. $($r.Out)"
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
if (-not $env:FIREFIGHTER_NO_AUTOSTART) { Install-LogonShortcut }

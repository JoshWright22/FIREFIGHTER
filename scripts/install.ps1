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
$dir = Join-Path $env:LOCALAPPDATA 'Programs\firefighter'
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
        # With the GitHub CLI signed in, also check the release's signed build provenance.
        if (Get-Command gh -ErrorAction SilentlyContinue) {
            $null = & gh auth status 2>&1
            if ($LASTEXITCODE -eq 0) {
                $null = & gh attestation verify "$tmp\$name" --repo $repo 2>&1
                if ($LASTEXITCODE -ne 0) { throw "provenance check failed for $name" }
                Write-Host 'verified signed build provenance'
            }
        }
        Expand-Archive "$tmp\$name" -DestinationPath $tmp -Force
        Install-Exe "$tmp\firefighter.exe"
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
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

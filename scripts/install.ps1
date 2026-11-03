# firefighter installer for Windows:
#   irm https://raw.githubusercontent.com/JoshWright22/firefighter/main/scripts/install.ps1 | iex
# Puts firefighter.exe in %LOCALAPPDATA%\Programs\firefighter, adds it to your PATH, then runs
# `firefighter setup` (start at logon, MCP for agent CLIs, shared-machine rules in their global
# instructions). Undo: firefighter uninstall.
$ErrorActionPreference = 'Stop'
$repo = 'JoshWright22/firefighter'
$dir = Join-Path $env:LOCALAPPDATA 'Programs\firefighter'
New-Item -ItemType Directory -Force $dir | Out-Null
$exe = Join-Path $dir 'firefighter.exe'

if ($env:FIREFIGHTER_LOCAL_BINARY) {
    Copy-Item $env:FIREFIGHTER_LOCAL_BINARY $exe -Force
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
        if (Get-Process firefighter -ErrorAction SilentlyContinue) { & $exe daemon --stop 2>$null }
        Expand-Archive "$tmp\$name" -DestinationPath $tmp -Force
        Copy-Item "$tmp\firefighter.exe" $exe -Force
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($userPath -split ';') -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ";$dir"), 'User')
}
$env:Path = "$dir;$env:Path"
& $exe setup
if ($LASTEXITCODE -ne 0) { throw "firefighter setup failed" }

<#
Salad (salad.com) as firefighter's band-4 harvester on Windows.

  salad.ps1 setup    once, from an elevated PowerShell: Salad's service starts only when asked
                     (Manual) and this user may start and stop it without a UAC prompt
  salad.ps1 start    firefighter allows Salad: service up, app open (minimized)
  salad.ps1 stop     firefighter forbids it: container jobs stopped (WSL distro terminated),
                     app closed, service stopped
  salad.ps1 running  exit 0 while Salad's container work runs (its WSL distro), 1 if not
  salad.ps1 status   what's found, for a human

firefighter.toml:
  [harvester]
  start   = 'powershell -NoProfile -ExecutionPolicy Bypass -File "C:\path\salad.ps1" start'
  stop    = 'powershell -NoProfile -ExecutionPolicy Bypass -File "C:\path\salad.ps1" stop'
  running = 'powershell -NoProfile -ExecutionPolicy Bypass -File "C:\path\salad.ps1" running'
  processes = ["vmmemWSL", "vmmem", "Salad.Bowl.Service"]

Salad has no documented command to start or stop chopping. So firefighter decides when Salad may
run (service and app up or down), and Salad's own Auto Start, set to its shortest delay in the
app, begins chopping inside that window. Salad's Auto Launch at logon should be off (setup removes
its Run entry): firefighter launches it.
#>
param([Parameter(Mandatory)][ValidateSet('setup', 'start', 'stop', 'running', 'status')][string]$Action)
$ErrorActionPreference = 'SilentlyContinue'
$env:WSL_UTF8 = '1' # wsl.exe prints UTF-16 otherwise

$Distro = 'salad-enterprise-linux'

function Get-SaladService {
    Get-CimInstance Win32_Service | Where-Object { $_.PathName -match 'Salad' -or $_.Name -match 'Salad' } | Select-Object -First 1
}

function Get-SaladApp {
    foreach ($root in @("$env:ProgramFiles\Salad", "$env:LOCALAPPDATA\Programs\Salad", "$env:LOCALAPPDATA\Programs\salad")) {
        if (Test-Path "$root\Salad.exe") { return "$root\Salad.exe" }
    }
    foreach ($hive in 'HKLM:', 'HKCU:') {
        $loc = (Get-ItemProperty "$hive\Software\7a0ebc42-7f71-5caa-9738-b7dda7589c77").InstallLocation
        if ($loc -and (Test-Path "$loc\Salad.exe")) { return "$loc\Salad.exe" }
    }
}

function Test-DistroRunning {
    $running = wsl.exe -l --running -q 2>$null
    return [bool]($running | Where-Object { $_.Trim() -ieq $Distro })
}

switch ($Action) {
    'setup' {
        $id = [Security.Principal.WindowsIdentity]::GetCurrent()
        if (-not ([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
            Write-Error 'run setup from an elevated PowerShell (once)'; exit 1
        }
        $svc = Get-SaladService
        if (-not $svc) { Write-Error 'Salad service not found: install Salad first'; exit 1 }
        sc.exe config $svc.Name start= demand | Out-Null
        # Let this user start (RP), stop (WP) and query (LC, RC) the service.
        $sid = $id.User.Value
        $sddl = (sc.exe sdshow $svc.Name | Where-Object { $_ -match 'D:' }).Trim()
        if ($sddl -notmatch [regex]::Escape($sid)) {
            $ace = "(A;;RPWPLCRC;;;$sid)"
            $new = $sddl -replace '^(D:)', "`$1$ace"
            sc.exe sdset $svc.Name $new | Out-Null
        }
        # Salad's own Auto Launch at logon: firefighter launches it instead.
        Remove-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'Salad'
        Remove-ItemProperty 'HKLM:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'Salad'
        Write-Output "service '$($svc.Name)': Manual, startable by $($id.Name); Salad's logon auto-launch removed"
        Write-Output 'In the Salad app: Settings > Auto Start ON with the shortest delay; Auto Launch OFF.'
    }
    'start' {
        $svc = Get-SaladService
        if ($svc -and $svc.State -ne 'Running') { sc.exe start $svc.Name | Out-Null }
        $app = Get-SaladApp
        if ($app -and -not (Get-Process -Name 'Salad')) { Start-Process $app -WindowStyle Minimized }
    }
    'stop' {
        wsl.exe --terminate $Distro 2>$null | Out-Null
        Get-Process -Name 'Salad' | Stop-Process -Force
        $svc = Get-SaladService
        if ($svc -and $svc.State -ne 'Stopped') { sc.exe stop $svc.Name | Out-Null }
        # A service that won't stop (rights not granted by setup) still loses its workload above.
        Start-Sleep -Seconds 5
        if (Test-DistroRunning) { wsl.exe --terminate $Distro 2>$null | Out-Null }
    }
    'running' {
        # Only real work counts: the app left open by hand isn't stopped, a container job is.
        if (Test-DistroRunning) { exit 0 }
        exit 1
    }
    'status' {
        $svc = Get-SaladService
        [pscustomobject]@{
            App           = Get-SaladApp
            AppOpen       = [bool](Get-Process -Name 'Salad')
            Service       = if ($svc) { "$($svc.Name) ($($svc.State), $($svc.StartMode))" } else { 'not found' }
            DistroRunning = Test-DistroRunning
            AutoLaunchRun = [bool](Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run').Salad
        } | Format-List
    }
}
exit 0

# install_autostart.ps1 - Register Windows Scheduled Tasks for auto-start
# Run as Administrator:
#   powershell -ExecutionPolicy Bypass -File scripts\install_autostart.ps1
# Uninstall:
#   powershell -ExecutionPolicy Bypass -File scripts\uninstall_autostart.ps1
#
# Honors env vars (set BEFORE running this script):
#   GATEWAY_HMAC_SECRET    - shared secret for worker auth (recommended)
#   GATEWAY_TLS_CERT/KEY   - TLS cert/key paths (for HTTPS)
#   GATEWAY_PEERS          - comma-separated peer URLs for HA mode
#   GATEWAY_TOKEN          - bearer token for the admin API
#   HA_MODE                - "1" to also register a 2nd gateway on :13001
#
# ASCII-only output by design. PowerShell 5.1 on Chinese Windows reads .ps1
# as GBK; non-ASCII Write-Host strings can crash the parser.

$ErrorActionPreference = "Continue"

# Check admin
$currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "Run this script as Administrator."
    exit 1
}

$ProjectRoot = Split-Path -Parent $PSScriptRoot
$ScriptDir   = "$ProjectRoot\scripts"
$LogDir      = "$ProjectRoot\logs"
if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir | Out-Null }

$TaskPrefix = "deskgateway"

# Decide task list based on env. HA_MODE=1 adds a 2nd gateway.
$HasHA = ($env:HA_MODE -eq "1") -or ($env:GATEWAY_PEERS)

# Build task list. (name, script, trigger, delaySec, type, peer-list)
$Tasks = @(
    @{ Name = "$TaskPrefix-gateway";      Script = "$ScriptDir\run_gateway.cmd";      Trigger = 1; DelaySec = 0;  Type = "cmd"; Peers = @() }
)
if ($HasHA) {
    $Tasks += @{ Name = "$TaskPrefix-gateway-2"; Script = "$ScriptDir\run_gateway_2.cmd"; Trigger = 1; DelaySec = 0; Type = "cmd"; Peers = @() }
}
$Tasks += @(
    @{ Name = "$TaskPrefix-worker-code";  Script = "$ScriptDir\run_worker_code.cmd";  Trigger = 1; DelaySec = 30; Type = "cmd"; Peers = @() },
    @{ Name = "$TaskPrefix-worker-doc";   Script = "$ScriptDir\run_worker_doc.cmd";   Trigger = 1; DelaySec = 30; Type = "cmd"; Peers = @() },
    @{ Name = "$TaskPrefix-worker-csv";   Script = "$ScriptDir\run_worker_csv.cmd";   Trigger = 1; DelaySec = 30; Type = "cmd"; Peers = @() },
    @{ Name = "$TaskPrefix-worker-image"; Script = "$ScriptDir\run_worker_image.cmd"; Trigger = 1; DelaySec = 30; Type = "cmd"; Peers = @() },
    @{ Name = "$TaskPrefix-worker-audio"; Script = "$ScriptDir\run_worker_audio.cmd"; Trigger = 1; DelaySec = 30; Type = "cmd"; Peers = @() },
    @{ Name = "$TaskPrefix-worker-video"; Script = "$ScriptDir\run_worker_video.cmd"; Trigger = 1; DelaySec = 30; Type = "cmd"; Peers = @() },
    @{ Name = "$TaskPrefix-watchdog";     Script = "$ScriptDir\watchdog.ps1";        Trigger = 2; DelaySec = 0;  Type = "ps1"; Peers = @() }
)

Write-Host "[install_autostart] Project root: $ProjectRoot"

# Detect what's enabled so the operator gets a clear summary
$HasHMAC = [bool]$env:GATEWAY_HMAC_SECRET
$HasTLS  = [bool]($env:GATEWAY_TLS_CERT -and $env:GATEWAY_TLS_KEY)
$HasToken = [bool]$env:GATEWAY_TOKEN
Write-Host "[install_autostart] Detected: HMAC=$HasHMAC TLS=$HasTLS Token=$HasToken HA=$HasHA"

# Verify scripts exist
foreach ($t in $Tasks) {
    if (-not (Test-Path $t.Script)) {
        Write-Warning "Script not found (skipping): $($t.Script)"
    }
}

# 1. Clean existing tasks (idempotent re-run)
Write-Host "[install] Removing existing tasks..."
foreach ($t in $Tasks) {
    try {
        Unregister-ScheduledTask -TaskName $t.Name -Confirm:$false -ErrorAction Stop
        Write-Host "  removed: $($t.Name)"
    } catch {
        Write-Host "  (none):  $($t.Name)"
    }
}

# 2. Register new tasks
Write-Host "[install] Registering tasks..."
foreach ($t in $Tasks) {
    if (-not (Test-Path $t.Script)) {
        Write-Host "  skip (missing): $($t.Name)"
        continue
    }
    try {
        if ($t.Type -eq "cmd") {
            $action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$($t.Script)`""
        } else {
            $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-ExecutionPolicy Bypass -NoProfile -File `"$($t.Script)`""
        }

        if ($t.Trigger -eq 1) {
            $trigger = New-ScheduledTaskTrigger -AtStartup
            if ($t.DelaySec -gt 0) {
                $trigger.Delay = "PT$($t.DelaySec)S"
            }
        } else {
            $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) `
                -RepetitionInterval (New-TimeSpan -Minutes 1) `
                -RepetitionDuration (New-TimeSpan -Days 3650)
        }

        $settings = New-ScheduledTaskSettingsSet
        $settings.RestartCount = 3

        Register-ScheduledTask `
            -TaskName $t.Name `
            -Action $action `
            -Trigger $trigger `
            -Settings $settings `
            -User "SYSTEM" `
            -RunLevel Highest `
            -Description "deskgrid-cloud $($t.Name)" `
            -Force | Out-Null

        Write-Host "  registered: $($t.Name)"
    } catch {
        Write-Error "Failed to register $($t.Name): $($_.Exception.Message)"
    }
}

# 3. Trigger all tasks now
Write-Host "[install] Triggering tasks now..."
foreach ($t in $Tasks) {
    if (-not (Test-Path $t.Script)) { continue }
    try {
        Start-ScheduledTask -TaskName $t.Name -ErrorAction Stop
        Write-Host "  started: $($t.Name)"
    } catch {
        Write-Host "  [warn]  start failed: $($t.Name) ($($_.Exception.Message))"
    }
    Start-Sleep -Milliseconds 500
}

# 4. Verify
Write-Host ""
Write-Host "==========================================="
Write-Host " Install complete. Task status:"
Write-Host "==========================================="
foreach ($t in $Tasks) {
    if (-not (Test-Path $t.Script)) { continue }
    try {
        $info = Get-ScheduledTask -TaskName $t.Name -ErrorAction Stop
        $state = $info.State.ToString()
        Write-Host "  $($t.Name) -> $state"
    } catch {
        Write-Host "  $($t.Name) -> (query failed)"
    }
}
Write-Host ""
Write-Host " Logs: $LogDir"
Write-Host " Uninstall: powershell -ExecutionPolicy Bypass -File scripts\uninstall_autostart.ps1"
Write-Host "==========================================="
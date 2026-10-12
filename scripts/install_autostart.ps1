# install_autostart.ps1 - Register Windows Scheduled Tasks for auto-start
# Run as Administrator:
#   powershell -ExecutionPolicy Bypass -File scripts\install_autostart.ps1
# Uninstall:
#   powershell -ExecutionPolicy Bypass -File scripts\uninstall_autostart.ps1
#
# Uses PowerShell's ScheduledTasks module instead of schtasks.exe directly,
# which avoids schtasks' notoriously finicky argument parsing.
#
# NOTE: ASCII-only output by design. PowerShell 5.1 on Chinese Windows reads
# .ps1 as GBK; non-ASCII Write-Host strings can crash the parser.

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
# name, script, trigger (1=ONSTART, 2=MINUTE), delaySec, type (cmd|ps1)
$Tasks = @(
    @{ Name = "$TaskPrefix-gateway";     Script = "$ScriptDir\run_gateway.cmd";     Trigger = 1; DelaySec = 0;  Type = "cmd" },
    @{ Name = "$TaskPrefix-worker-code"; Script = "$ScriptDir\run_worker_code.cmd"; Trigger = 1; DelaySec = 30; Type = "cmd" },
    @{ Name = "$TaskPrefix-worker-doc";  Script = "$ScriptDir\run_worker_doc.cmd";  Trigger = 1; DelaySec = 30; Type = "cmd" },
    @{ Name = "$TaskPrefix-worker-csv";  Script = "$ScriptDir\run_worker_csv.cmd";  Trigger = 1; DelaySec = 30; Type = "cmd" },
    @{ Name = "$TaskPrefix-watchdog";    Script = "$ScriptDir\watchdog.ps1";        Trigger = 2; DelaySec = 0;  Type = "ps1" }
)

Write-Host "[install_autostart] Project root: $ProjectRoot"

# Verify scripts exist
foreach ($t in $Tasks) {
    if (-not (Test-Path $t.Script)) {
        Write-Error "Script not found: $($t.Script)"
        exit 1
    }
}

# 1. Clean existing tasks (idempotent re-run)
Write-Host "[install_autostart] Removing existing tasks..."
foreach ($t in $Tasks) {
    try {
        Unregister-ScheduledTask -TaskName $t.Name -Confirm:$false -ErrorAction Stop
        Write-Host "  removed: $($t.Name)"
    } catch {
        Write-Host "  (none):  $($t.Name)"
    }
}

# 2. Register new tasks
Write-Host "[install_autostart] Registering tasks..."
foreach ($t in $Tasks) {
    try {
        if ($t.Type -eq "cmd") {
            $action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$($t.Script)`""
        } else {
            $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-ExecutionPolicy Bypass -NoProfile -File `"$($t.Script)`""
        }

        if ($t.Trigger -eq 1) {
            $trigger = New-ScheduledTaskTrigger -AtStartup
            if ($t.DelaySec -gt 0) {
                # ISO 8601 duration: 30 seconds = "PT30S"
                $trigger.Delay = "PT$($t.DelaySec)S"
            }
        } else {
            # Watchdog: every 1 minute, indefinite
            $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) `
                -RepetitionInterval (New-TimeSpan -Minutes 1) `
                -RepetitionDuration (New-TimeSpan -Days 3650)
        }

        # Settings: keep most defaults. Add restart-on-crash (3x).
# Other defaults are sensible: AllowStartIfOnBatteries=true, StartWhenAvailable, etc.
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
        exit 1
    }
}

# 3. Trigger all tasks now (start everything without rebooting)
Write-Host "[install_autostart] Triggering all tasks..."
foreach ($t in $Tasks) {
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
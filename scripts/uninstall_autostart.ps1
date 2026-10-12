# uninstall_autostart.ps1 - Remove scheduled tasks created by install_autostart.ps1
# Run as Administrator:
#   powershell -ExecutionPolicy Bypass -File scripts\uninstall_autostart.ps1
$ErrorActionPreference = "Continue"

$currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "Run this script as Administrator."
    exit 1
}

$TaskPrefix = "deskgateway"
$TaskNames = @(
    "$TaskPrefix-gateway",
    "$TaskPrefix-worker-code",
    "$TaskPrefix-worker-doc",
    "$TaskPrefix-worker-csv",
    "$TaskPrefix-watchdog"
)

Write-Host "[uninstall_autostart] Removing scheduled tasks..."
foreach ($name in $TaskNames) {
    try {
        Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction Stop
        Write-Host "  removed: $name"
    } catch {
        Write-Host "  skipped: $name (not present)"
    }
}
Write-Host "[uninstall_autostart] Done."
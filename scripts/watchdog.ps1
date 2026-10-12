# watchdog.ps1 - Runs every minute via Task Scheduler (SYSTEM account).
# Detects dead processes by port listening state, restarts via schtasks /Run.
#
# Watch policy: port LISTEN state
#   13000 -> deskgateway-gateway
#   3001  -> deskgateway-worker-code
#   3002  -> deskgateway-worker-doc
#   3003  -> deskgateway-worker-csv
#
# ASCII-only output: PowerShell 5.1 on Chinese Windows reads .ps1 as GBK by
# default; non-ASCII Write-Host strings can crash the parser.

$ErrorActionPreference = "SilentlyContinue"

$ProjectRoot = "E:\deskgrid-cloud"
$LogDir      = "$ProjectRoot\logs"
$LogFile     = "$LogDir\watchdog.log"
$LockFile    = "$env:TEMP\deskgateway_watchdog.lock"

if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir -Force | Out-Null }

# Single-instance lock: don't pile up runs if a previous one is still going.
if (Test-Path $LockFile) {
    $age = (Get-Date) - (Get-Item $LockFile).LastWriteTime
    if ($age.TotalMinutes -lt 5) {
        # lock is fresh -> previous run still in progress, skip this one
        exit 0
    } else {
        # lock too old -> previous run probably crashed, force-remove
        Remove-Item $LockFile -Force -ErrorAction SilentlyContinue
    }
}
New-Item -ItemType File -Path $LockFile -Force | Out-Null

try {
    $Services = @(
        @{ Port = 13000; Task = "deskgateway-gateway";     Label = "gateway"     },
        @{ Port = 3001;  Task = "deskgateway-worker-code"; Label = "worker-code" },
        @{ Port = 3002;  Task = "deskgateway-worker-doc";  Label = "worker-doc"  },
        @{ Port = 3003;  Task = "deskgateway-worker-csv";  Label = "worker-csv"  }
    )

    $ts    = Get-Date -Format "yyyy-MM-dd HH:mm:ss"
    $report = "[$ts] watchdog check"

    foreach ($s in $Services) {
        $listening = $false
        try {
            $conn = Get-NetTCPConnection -LocalPort $s.Port -State Listen -ErrorAction Stop
            if ($conn) { $listening = $true }
        } catch { }

        if ($listening) {
            $report += "`n  [OK]   port $($s.Port) ($($s.Label)) listening"
        } else {
            $report += "`n  [DOWN] port $($s.Port) ($($s.Label)) -> restart task $($s.Task)"
            $proc = Start-Process -FilePath "schtasks.exe" `
                -ArgumentList @("/Run", "/TN", $s.Task) `
                -Wait -PassThru -NoNewWindow -RedirectStandardError "$LogDir\watchdog.err.log"
            if ($proc.ExitCode -eq 0) {
                $report += "  [OK]   schtasks /Run triggered"
            } else {
                $report += "  [FAIL] schtasks exit=$($proc.ExitCode)"
            }
        }
    }

    Add-Content -Path $LogFile -Value $report
}
finally {
    Remove-Item $LockFile -Force -ErrorAction SilentlyContinue
}
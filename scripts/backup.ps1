# backup.ps1 - Snapshot data/, artifacts/ (recent), logs/ (recent), bin\gateway.exe
# into a single zip under E:\deskgrid-cloud\backups\. Keeps the last 30 backups.
#
# Run weekly via Task Scheduler (SYSTEM) - see DEPLOY.md section 5.
#
# Usage: powershell -ExecutionPolicy Bypass -File scripts\backup.ps1
$ErrorActionPreference = "Continue"

$ProjectRoot = Split-Path -Parent $PSScriptRoot
$Stamp       = Get-Date -Format "yyyy-MM-dd_HHmmss"
$BackupDir   = Join-Path $ProjectRoot "backups"
$BackupFile  = Join-Path $BackupDir ("backup-" + (Get-Date -Format "yyyy-MM-dd") + ".zip")
$TmpDir      = Join-Path $env:TEMP "deskgrid-backup-$Stamp"

# Create dirs
if (-not (Test-Path $BackupDir)) { New-Item -ItemType Directory -Path $BackupDir -Force | Out-Null }
if (Test-Path $TmpDir) { Remove-Item -Recurse -Force $TmpDir }
New-Item -ItemType Directory -Path $TmpDir -Force | Out-Null

# Helper: copy only files modified in last N days
function Copy-Recent($src, $dst, $days) {
    if (-not (Test-Path $src)) { return }
    $cutoff = (Get-Date).AddDays(-$days)
    Get-ChildItem -Path $src -Recurse -File -ErrorAction SilentlyContinue |
        Where-Object { $_.LastWriteTime -ge $cutoff } |
        ForEach-Object {
            $rel = $_.FullName.Substring($src.Length).TrimStart('\','/')
            $destDir = Join-Path $dst $rel.Substring(0, [Math]::Max(0, $rel.LastIndexOf('\')))
            if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }
            Copy-Item $_.FullName -Destination (Join-Path $dst $rel) -Force
        }
}

# 1. data/ - all of it (small, critical)
$dataSrc = Join-Path $ProjectRoot "data"
$dataDst = Join-Path $TmpDir "data"
if (Test-Path $dataSrc) {
    New-Item -ItemType Directory -Path $dataDst -Force | Out-Null
    Copy-Item "$dataSrc\*" -Destination $dataDst -Recurse -Force
}

# 2. artifacts/ - last 7 days only
Copy-Recent (Join-Path $ProjectRoot "artifacts") (Join-Path $TmpDir "artifacts") 7

# 3. logs/ - last 7 days only
Copy-Recent (Join-Path $ProjectRoot "logs") (Join-Path $TmpDir "logs") 7

# 4. bin\gateway.exe - the compiled binary (in case rebuild fails later)
$binSrc = Join-Path $ProjectRoot "bin"
$binDst = Join-Path $TmpDir "bin"
if (Test-Path "$binSrc\gateway.exe") {
    New-Item -ItemType Directory -Path $binDst -Force | Out-Null
    Copy-Item "$binSrc\gateway.exe" -Destination $binDst -Force
}

# 5. snapshot of git commit for traceability
$gitHead = Join-Path $TmpDir "GIT_HEAD.txt"
try {
    $commit = (git -C $ProjectRoot rev-parse HEAD 2>$null)
    $branch = (git -C $ProjectRoot rev-parse --abbrev-ref HEAD 2>$null)
    "branch=$branch`ncommit=$commit" | Out-File -FilePath $gitHead -Encoding ASCII -Force
} catch {
    "(no git info)" | Out-File -FilePath $gitHead -Encoding ASCII -Force
}

# 6. zip it
if (Test-Path $BackupFile) { Remove-Item $BackupFile -Force }
Add-Type -AssemblyName "System.IO.Compression.FileSystem"
[IO.Compression.ZipFile]::CreateFromDirectory($TmpDir, $BackupFile)

# 7. cleanup tmp
Remove-Item -Recurse -Force $TmpDir

# 8. retention: keep last 30 backups
Get-ChildItem -Path $BackupDir -Filter "backup-*.zip" |
    Sort-Object LastWriteTime -Descending |
    Select-Object -Skip 30 |
    ForEach-Object { Remove-Item $_.FullName -Force }

Write-Host "[backup] created: $BackupFile"
Write-Host "[backup] retained: $((Get-ChildItem $BackupDir -Filter 'backup-*.zip').Count) files"
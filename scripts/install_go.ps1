# install_go.ps1 — 解压 Go 1.23.4 到 E:\Go 并设置 PATH
# 用法: powershell -ExecutionPolicy Bypass -File scripts\install_go.ps1

$ErrorActionPreference = "Stop"
$zipPath = "C:\Users\Administrator\AppData\Local\Temp\go.zip"
$destRoot = "E:\"

Write-Host "[install_go] extracting $zipPath to $destRoot ..."
Expand-Archive -Path $zipPath -DestinationPath $destRoot -Force

$goBin = "E:\Go\bin\go.exe"
if (-not (Test-Path $goBin)) {
    Write-Error "Go binary not found at $goBin"
    exit 1
}

# Get current user PATH
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*E:\Go\bin*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;E:\Go\bin", "User")
    Write-Host "[install_go] added E:\Go\bin to user PATH"
}

# Set GOROOT for current session
$env:GOROOT = "E:\Go"
$env:Path = "$env:Path;E:\Go\bin"

Write-Host "[install_go] Go version: $((& $goBin version))"
Write-Host "[install_go] done. Restart your terminal to pick up the new PATH."

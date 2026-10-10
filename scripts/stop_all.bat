@echo off
REM 停止所有 deskgrid-cloud 相关进程
echo [stop_all] stopping gateway ...
taskkill /FI "WINDOWTITLE eq deskgrid-cloud-gateway*" /T /F >nul 2>&1
echo [stop_all] stopping workers ...
taskkill /FI "WINDOWTITLE eq deskgrid-cloud-worker-code*" /T /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq deskgrid-cloud-worker-doc*"  /T /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq deskgrid-cloud-worker-csv*"  /T /F >nul 2>&1
echo [stop_all] done.

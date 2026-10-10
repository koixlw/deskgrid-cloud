@echo off
setlocal
cd /d "%~dp0\.."
if not exist "logs" mkdir logs
start "deskgrid-cloud-gateway" /B cmd /c "bin\gateway.exe > logs\gateway.out.log 2>&1"
echo [start_gateway] launched, see logs\gateway.out.log
endlocal

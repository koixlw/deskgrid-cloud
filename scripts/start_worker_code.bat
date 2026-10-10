@echo off
setlocal
cd /d "%~dp0\.."
if not exist "logs" mkdir logs
start "deskgrid-cloud-worker-code" /B cmd /c "python workers\code\main.py > logs\worker-code.out.log 2>&1"
echo [start_worker_code] launched, see logs\worker-code.out.log
endlocal

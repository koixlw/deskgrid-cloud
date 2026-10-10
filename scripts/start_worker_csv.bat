@echo off
setlocal
cd /d "%~dp0\.."
if not exist "logs" mkdir logs
start "deskgrid-cloud-worker-csv" /B cmd /c "python workers\csv\main.py > logs\worker-csv.out.log 2>&1"
echo [start_worker_csv] launched, see logs\worker-csv.out.log
endlocal

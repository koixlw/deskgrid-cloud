@echo off
setlocal
cd /d "%~dp0\.."
if not exist "logs" mkdir logs
start "deskgrid-cloud-worker-doc" /B cmd /c "python workers\doc\main.py > logs\worker-doc.out.log 2>&1"
echo [start_worker_doc] launched, see logs\worker-doc.out.log
endlocal

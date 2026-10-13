@echo off
REM run_worker_csv.cmd - csv worker (port 3003)
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set PORT=3003
set WORKER_NAME=csv-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\csv\main.py >> logs\worker-csv.log 2>&1
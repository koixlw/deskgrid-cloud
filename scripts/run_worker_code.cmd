@echo off
REM run_worker_code.cmd - code worker (port 3001)
REM Reads GATEWAY / GATEWAY_HMAC_SECRET / INSECURE_SKIP_TLS_VERIFY from caller.
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set PORT=3001
set WORKER_NAME=code-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\code\main.py >> logs\worker-code.log 2>&1
@echo off
REM run_worker_code.cmd - code worker (port 3001)
REM SYSTEM account has empty APPDATA so we point it to the admin profile where
REM the Python site-packages (uvicorn, fastapi, psutil) are actually installed.
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set APPDATA=C:\Users\Administrator\AppData\Roaming
set LOCALAPPDATA=C:\Users\Administrator\AppData\Local
set USERPROFILE=C:\Users\Administrator
set HOMEDRIVE=C:
set HOMEPATH=\Users\Administrator
set GATEWAY=http://127.0.0.1:13000
set PORT=3001
set WORKER_NAME=code-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\code\main.py >> logs\worker-code.out.log 2>&1
@echo off
REM run_worker_doc.cmd - doc worker (port 3002)
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set APPDATA=C:\Users\Administrator\AppData\Roaming
set LOCALAPPDATA=C:\Users\Administrator\AppData\Local
set USERPROFILE=C:\Users\Administrator
set HOMEDRIVE=C:
set HOMEPATH=\Users\Administrator
set GATEWAY=http://127.0.0.1:13000
set PORT=3002
set WORKER_NAME=doc-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\doc\main.py >> logs\worker-doc.out.log 2>&1
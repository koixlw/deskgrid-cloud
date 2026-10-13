@echo off
REM run_worker_doc.cmd - doc worker (port 3002)
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set PORT=3002
set WORKER_NAME=doc-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\doc\main.py >> logs\worker-doc.log 2>&1
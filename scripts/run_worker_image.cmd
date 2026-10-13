@echo off
REM run_worker_image.cmd - image worker (port 3004)
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set PORT=3004
set WORKER_NAME=image-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\image\main.py >> logs\worker-image.log 2>&1
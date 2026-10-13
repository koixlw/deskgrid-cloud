@echo off
REM run_worker_video.cmd - video worker (port 3006)
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set PORT=3006
set WORKER_NAME=video-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\video\main.py >> logs\worker-video.log 2>&1
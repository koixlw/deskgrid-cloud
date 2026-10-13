@echo off
REM run_worker_audio.cmd - audio worker (port 3005)
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set PORT=3005
set WORKER_NAME=audio-node-01
"C:\Users\Administrator\AppData\Local\Programs\Python\Python311\python.exe" workers\audio\main.py >> logs\worker-audio.log 2>&1
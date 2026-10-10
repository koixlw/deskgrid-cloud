@echo off
REM ============================================
REM  共享算力 — 一键启动 (Windows)
REM  启动顺序：先 Gateway，等 3 秒后启动 3 个 Worker
REM ============================================
setlocal
cd /d "%~dp0\.."

set LOG_DIR=logs
if not exist "%LOG_DIR%" mkdir "%LOG_DIR%"

REM 1. 启动 Gateway
echo [start_all] launching gateway on :13000 ...
start "deskgrid-cloud-gateway" /B cmd /c "bin\gateway.exe > %LOG_DIR%\gateway.out.log 2>&1"

REM 2. 等待 gateway 就绪
echo [start_all] waiting for gateway to be ready...
set /a MAX_RETRY=20
:wait_loop
set /a MAX_RETRY-=1
if %MAX_RETRY% LEQ 0 goto wait_done
powershell -Command "try { (Invoke-WebRequest -Uri http://127.0.0.1:13000/health -UseBasicParsing -TimeoutSec 2).StatusCode } catch { exit 1 }" >nul 2>&1
if errorlevel 1 (
  timeout /t 1 /nobreak >nul
  goto wait_loop
)
:wait_done
echo [start_all] gateway ready.

REM 3. 启动 3 个 Worker
echo [start_all] launching 3 workers ...
start "deskgrid-cloud-worker-code" /B cmd /c "python workers\code\main.py > %LOG_DIR%\worker-code.out.log 2>&1"
timeout /t 1 /nobreak >nul
start "deskgrid-cloud-worker-doc"  /B cmd /c "python workers\doc\main.py  > %LOG_DIR%\worker-doc.out.log  2>&1"
timeout /t 1 /nobreak >nul
start "deskgrid-cloud-worker-csv"  /B cmd /c "python workers\csv\main.py  > %LOG_DIR%\worker-csv.out.log  2>&1"

echo.
echo ============================================
echo  共享算力已启动
echo    总控面板:  http://localhost:13000/
echo    OpenAI API: http://localhost:13000/v1/chat/completions
echo    Code 节点:  http://localhost:3001
echo    Doc  节点:  http://localhost:3002
echo    CSV  节点:  http://localhost:3003
echo  日志目录:    %CD%\%LOG_DIR%\
echo  停止所有:    scripts\stop_all.bat
echo ============================================
endlocal

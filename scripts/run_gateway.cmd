@echo off
REM run_gateway.cmd — 由 Task Scheduler 调用的网关启动包装器
REM 输出重定向到 logs\gateway.out.log，崩溃退出后由 watchdog 拉起
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
bin\gateway.exe >> logs\gateway.out.log 2>&1
@echo off
REM run_gateway_2.cmd - secondary gateway for HA mode (port 13001)
REM Set GATEWAY_PEERS in env before calling start_all.py --ha.
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
set GATEWAY_ADDR=:13001
REM The caller (start_all.py --ha) sets GATEWAY_PEERS so this gateway
REM replicates to the primary. Gateway renames itself via log file.
bin\gateway.exe >> logs\gateway-13001.log 2>&1
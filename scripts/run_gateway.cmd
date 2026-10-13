@echo off
REM run_gateway.cmd - generic gateway launcher
REM Picks up env vars from caller (HMAC, TLS, PEERS, etc.).
setlocal
cd /d E:\deskgrid-cloud
if not exist logs mkdir logs
REM Pass through all env; the gateway reads GATEWAY_* directly.
bin\gateway.exe >> logs\gateway.log 2>&1
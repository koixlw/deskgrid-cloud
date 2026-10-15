@echo off
REM demo_run.bat - 双击运行的示例
REM 把这个 .bat 放到跟 deskgrid-client.exe 同目录
REM 双击它，会弹一个黑窗，让你敲一行命令
REM 然后 client.exe 自动判断本机闲/忙，决定本地跑还是甩远端

setlocal
cd /d "%~dp0"

echo.
echo ============================================================
echo  deskgrid-client demo
echo  敲一行命令，按回车执行。client 会自动判断：
echo    本机闲 =^> 本地跑
echo    本机忙 =^> 提交到集群
echo.
echo  退出 =^> Ctrl+C 或直接关窗
echo ============================================================
echo.

:loop
set /p CMDLINE=^> 请输入命令 (例: python -c "print(2+2)"):
if "%CMDLINE%"=="" goto loop

deskgrid-client.exe --watch --cmd %CMDLINE%
echo.
echo -------------------- 完成 --------------------
echo.
goto loop

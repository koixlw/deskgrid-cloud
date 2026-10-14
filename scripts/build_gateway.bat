@echo off
REM Build deskgrid-cloud gateway.exe — Go static binary with embedded icon.
REM
REM Steps:
REM   1. Install go-winres (one-time) — embeds .ico + RT_VERSION info into the .exe
REM   2. Regenerate gateway\rsrc*.syso from gateway\winres\winres.json + the .ico
REM   3. go build — picks up the .syso automatically
REM
REM Usage:  scripts\build_gateway.bat
REM Output: bin\gateway.exe

setlocal
cd /d "%~dp0\.."

REM ---------- Resolve go.exe via PATH (handles E:\Go\bin, C:\Go\bin, etc.) ----------
for /f "delims=" %%i in ('where go') do set "GO=%%i" & goto :got_go
:got_go
if "%GO%"=="" (
    echo [build] ERROR: go.exe not found in PATH. Run scripts\install_go.ps1 first.
    exit /b 1
)
echo [build] go: %GO%

REM ---------- Resolve go-winres.exe ----------
set WINRES=%USERPROFILE%\go\bin\go-winres.exe
if not exist "%WINRES%" (
    echo [build] go-winres not found - installing...
    "%GO%" install github.com/tc-hib/go-winres@latest || (
        echo [build] ERROR: failed to install go-winres.
        exit /b 1
    )
)
echo [build] winres: %WINRES%

REM ---------- Regenerate .syso ----------
echo [build] regenerating gateway\rsrc*.syso ...
pushd gateway
"%WINRES%" simply --icon ..\assets\deskgrid-gateway.ico --file-description "deskgrid-cloud Gateway" --product-name "deskgrid-cloud Gateway" --copyright "MIT License" || (
    popd
    echo [build] ERROR: go-winres failed.
    exit /b 1
)

REM ---------- Build ----------
echo [build] running go build ...
"%GO%" build -o ..\bin\gateway.exe . || (
    popd
    echo [build] ERROR: go build failed.
    exit /b 1
)
popd

echo.
echo [build] done. Output: bin\gateway.exe
dir bin\gateway.exe
endlocal
@echo off
REM Build deskgrid-client.exe — small (~10–15 MB) CLI for end-users.
REM
REM Usage:  scripts\build_client_exe.bat
REM Output: dist\deskgrid-client.exe + dist\client.conf.example

setlocal
cd /d "%~dp0\.."

echo [build] checking PyInstaller...
python -c "import PyInstaller" 2>nul || (
    echo [build] PyInstaller not installed - installing...
    pip install pyinstaller || (
        echo [build] ERROR: pip install pyinstaller failed.
        exit /b 1
    )
)

echo [build] checking runtime deps (requests, psutil)...
python -c "import requests" 2>nul || (
    echo [build] requests not installed - installing...
    pip install requests || exit /b 1
)
python -c "import psutil" 2>nul || (
    echo [build] psutil not installed - installing...
    pip install psutil || exit /b 1
)

if not exist assets\deskgrid-client.ico (
    echo [build] WARNING: assets\deskgrid-client.ico missing, falling back to default icon.
)

echo [build] cleaning previous artifacts...
if exist build rmdir /s /q build
if exist dist\deskgrid-client.exe del /q dist\deskgrid-client.exe
if exist dist\deskgrid-client.pkg del /q dist\deskgrid-client.pkg

echo [build] running PyInstaller --onefile...
python -m PyInstaller --clean deskgrid-client.spec || (
    echo [build] ERROR: PyInstaller failed.
    exit /b 1
)

echo [build] copying conf template to dist\...
if not exist dist mkdir dist
copy /y client.conf.example dist\client.conf.example >nul

echo.
echo [build] done.
dir dist\deskgrid-client.exe
echo.
echo [build] Usage:
echo   copy dist\deskgrid-client.exe  somewhere\deskgrid-client.exe
echo   copy dist\client.conf.example  somewhere\client.conf   ^& rem edit gateway url
echo   somewhere\deskgrid-client.exe --cmd "python train.py" --watch
endlocal

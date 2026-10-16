@echo off
REM Build deskgrid-all.exe — triple-role launcher (gateway + worker + web panel).
REM
REM Usage:  scripts\build_all_exe.bat
REM Output: dist\deskgrid-all.exe  (~80 MB)

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

echo [build] checking worker runtime deps (edge-tts, imageio_ffmpeg)...
python -c "import edge_tts" 2>nul || (
    echo [build] edge-tts not installed - installing...
    pip install edge_tts || exit /b 1
)
python -c "import imageio_ffmpeg" 2>nul || (
    echo [build] imageio_ffmpeg not installed - installing...
    pip install imageio imageio_ffmpeg || exit /b 1
)

echo [build] checking gateway binary...
if not exist bin\gateway.exe (
    echo [build] ERROR: bin\gateway.exe not found. Build it first:
    echo        cd gateway && go build -o ..\bin\gateway.exe .
    exit /b 1
)

if not exist assets\deskgrid-all.ico (
    echo [build] WARNING: assets\deskgrid-all.ico missing, falling back to default icon.
)

echo [build] cleaning previous artifacts...
if exist build rmdir /s /q build
if exist dist\deskgrid-all.exe del /q dist\deskgrid-all.exe
if exist dist\deskgrid-all.pkg del /q dist\deskgrid-all.pkg

echo [build] running PyInstaller --onefile...
python -m PyInstaller --clean deskgrid-all.spec || (
    echo [build] ERROR: PyInstaller failed.
    exit /b 1
)

echo.
echo [build] done. Output: dist\deskgrid-all.exe
dir dist\deskgrid-all.exe
echo.
echo [build] Usage:
echo   dist\deskgrid-all.exe                        REM  gateway + worker + open browser
echo   dist\deskgrid-all.exe --type code --port 3001
echo   dist\deskgrid-all.exe --worker-only --gateway http://other-host:13000
echo   dist\deskgrid-all.exe --client-only          REM  only gateway + panel, no worker
endlocal

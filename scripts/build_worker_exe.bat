@echo off
REM Build deskgrid-worker.exe — single-file Windows executable that runs any
REM worker type (code/doc/csv/image/audio/video) on a node without Python.
REM
REM Usage:  scripts\build_worker_exe.bat
REM Output: dist\deskgrid-worker.exe  (~30 MB)

setlocal
cd /d "%~dp0\.."

echo [build] checking PyInstaller...
python -c "import PyInstaller" 2>nul || (
    echo [build] PyInstaller not installed — installing...
    pip install pyinstaller || (
        echo [build] ERROR: pip install pyinstaller failed.
        exit /b 1
    )
)

echo [build] cleaning previous build artifacts (preserves worker.conf / README)...
if exist build rmdir /s /q build
if exist dist\deskgrid-worker.exe del /q dist\deskgrid-worker.exe
if exist dist\deskgrid-worker.pkg del /q dist\deskgrid-worker.pkg

echo [build] running PyInstaller --onefile...
REM --onefile: bundle everything into a single .exe
REM --icon: custom multi-resolution .ico
REM --add-data "workers;workers": include the workers package so imports work
REM --hidden-import: PyInstaller can't always see dynamic imports, list them
python -m PyInstaller ^
    --onefile ^
    --name deskgrid-worker ^
    --icon assets\deskgrid-worker.ico ^
    --add-data "workers;workers" ^
    --hidden-import "workers.code.tasks" ^
    --hidden-import "workers.doc.tasks" ^
    --hidden-import "workers.csv.tasks" ^
    --hidden-import "workers.image.tasks" ^
    --hidden-import "workers.audio.tasks" ^
    --hidden-import "workers.video.tasks" ^
    --hidden-import "common.base" ^
    --hidden-import "common.hardware" ^
    --hidden-import "requests" ^
    --hidden-import "urllib3" ^
    --hidden-import "psutil" ^
    --hidden-import "fastapi" ^
    --hidden-import "fastapi.responses" ^
    --hidden-import "starlette" ^
    --hidden-import "starlette.responses" ^
    --hidden-import "uvicorn.logging" ^
    --hidden-import "uvicorn.loops" ^
    --hidden-import "uvicorn.loops.auto" ^
    --hidden-import "uvicorn.protocols" ^
    --hidden-import "uvicorn.protocols.http" ^
    --hidden-import "uvicorn.protocols.http.auto" ^
    --hidden-import "uvicorn.protocols.websockets" ^
    --hidden-import "uvicorn.protocols.websockets.auto" ^
    --hidden-import "uvicorn.lifespan" ^
    --hidden-import "uvicorn.lifespan.on" ^
    --hidden-import "multipart" ^
    worker_launcher.py || (
        echo [build] ERROR: PyInstaller failed.
        exit /b 1
    )

echo.
echo [build] done. Output: dist\deskgrid-worker.exe
dir dist\deskgrid-worker.exe
endlocal
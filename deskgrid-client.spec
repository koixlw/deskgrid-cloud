# -*- mode: python ; coding: utf-8 -*-
#
# PyInstaller spec for deskgrid-client.exe.
# Stripped-down bundle: only the client + requests/psutil.
# No fastapi / uvicorn / edge-tts → ~10–15 MB instead of 75 MB.

a = Analysis(
    ['client_wrapper.py'],
    pathex=[],
    binaries=[],
    datas=[],
    hiddenimports=[
        'requests',
        'urllib3',
        'urllib3.util',
        'urllib3.util.retry',
        'urllib3.connectionpool',
        'urllib3.poolmanager',
        'psutil',
        'certifi',
        'charset_normalizer',
        'charset_normalizer.md',
        'idna',
        'idna.core',
        'hashlib',
        'hmac',
    ],
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=[
        # Avoid pulling in optional-but-heavy deps we don't use.
        'numpy', 'scipy', 'PIL', 'Pillow', 'cv2',
        'tkinter', 'matplotlib', 'pandas', 'torch',
    ],
    noarchive=False,
    optimize=0,
)
pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    a.binaries,
    a.datas,
    [],
    name='deskgrid-client',
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=True,
    upx_exclude=[],
    runtime_tmpdir=None,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
    icon=['assets/deskgrid-client.ico'],
)

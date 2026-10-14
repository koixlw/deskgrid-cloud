"""deskgrid-cloud Worker Launcher — single-file .exe entry point.

This script is the entry point for the bundled PyInstaller .exe. It can
launch any of the six worker types (code / doc / csv / image / audio / video)
by reading its config from CLI args, environment variables, or a
`worker.conf` file next to the executable.

It also supports `--install` / `--uninstall` to register or remove a
Windows scheduled task that auto-starts the worker on user logon and
restarts it if it crashes.

Usage examples (after building dist/deskgrid-worker.exe):
    # Run with config from worker.conf next to the .exe
    deskgrid-worker.exe

    # Override via command line
    deskgrid-worker.exe --type code --gateway http://192.168.1.178:13000 ^
                        --hmac-secret SECRET --name office-pc-02-code

    # Install as auto-start (creates scheduled task, no immediate run)
    deskgrid-worker.exe --install

    # Uninstall (removes scheduled task)
    deskgrid-worker.exe --uninstall

    # Show current effective config
    deskgrid-worker.exe --show-config
"""
import argparse
import json
import logging
import multiprocessing
import os
import subprocess
import sys
from pathlib import Path

# --------------------------------------------------------------------------
# PyInstaller bootstrap: when running as a --onefile bundle, Python source
# and data files live in sys._MEIPASS. Make `common`, `tasks`, etc.
# importable as they are in the source tree.
# --------------------------------------------------------------------------
if getattr(sys, "frozen", False):
    # Bundled — sys._MEIPASS contains the extracted tree
    BUNDLE_DIR = sys._MEIPASS  # type: ignore[attr-defined]
    sys.path.insert(0, os.path.join(BUNDLE_DIR, "workers"))
else:
    # Running from source — workers/ is a sibling directory
    HERE = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(HERE, "workers"))


# ---------- Per-type task dispatch ----------
# Maps worker type -> {task_name: callable}. The dispatch is registered
# at module import time so BaseWorker can pick the right task function
# up by string name.
DISPATCH_BY_TYPE: dict[str, dict[str, str]] = {
    "code":  {"code_format": "format_code", "code_ast": "count_ast"},
    "doc":   {"doc_summary": "summarize",   "doc_wordfreq": "wordfreq"},
    "csv":   {"csv_montecarlo": "montecarlo", "csv_stats": "stats"},
    "image": {
        "image_txt2img": "txt2img",
        "image_img2img": "img2img",
        "image_inpaint": "inpaint",
        "image_controlnet": "controlnet",
    },
    "audio": {"audio_tts_line": "tts_line", "audio_tts_emotion": "tts_with_emotion"},
    "video": {
        "video_assemble_clip": "assemble_clip",
        "video_add_subtitle": "add_subtitle",
        "video_kenburns": "kenburns",
    },
}

DEFAULT_PORT_BY_TYPE = {
    "code":  3001, "doc": 3002, "csv": 3003,
    "image": 3004, "audio": 3005, "video": 3006,
}


def _executable_path() -> str:
    """Return absolute path to this .exe (or .py when running from source)."""
    if getattr(sys, "frozen", False):
        return sys.executable
    return os.path.abspath(__file__)


def _exe_dir() -> Path:
    """Directory containing this .exe."""
    return Path(_executable_path()).parent


# --------------------------------------------------------------------------
# Config loading: precedence CLI > worker.conf > env > defaults
# --------------------------------------------------------------------------
def load_conf_file(path: Path) -> dict:
    """Parse a `KEY=VALUE` style config file. Lines starting with # are comments."""
    cfg: dict = {}
    if not path.exists():
        return cfg
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            continue
        k, v = line.split("=", 1)
        cfg[k.strip().upper()] = v.strip()
    return cfg


def _find_conf(explicit: str | None) -> Path | None:
    """Locate worker.conf: explicit path > exe dir > cwd."""
    if explicit:
        return Path(explicit)
    candidates = [
        _exe_dir() / "worker.conf",
        Path.cwd() / "worker.conf",
    ]
    for p in candidates:
        if p.exists():
            return p
    return candidates[0]  # default to exe_dir (will report as missing)


def resolve_config(args: argparse.Namespace) -> dict:
    """Merge CLI args > worker.conf > env vars into one config dict."""
    conf_path = _find_conf(args.config)
    file_cfg = load_conf_file(conf_path) if conf_path and conf_path.exists() else {}

    def pick(key: str, cli_val: str | None, default: str = "") -> str:
        if cli_val:
            return cli_val
        if key in file_cfg:
            return file_cfg[key]
        return os.environ.get(key, default)

    wtype = pick("TYPE", args.type).lower()
    gateway = pick("GATEWAY", args.gateway)
    secret = pick("GATEWAY_HMAC_SECRET", args.hmac_secret)
    name = pick("WORKER_NAME", args.name) or f"{wtype or 'worker'}-node"
    host = pick("WORKER_HOST", args.host, "0.0.0.0")
    port_str = pick("PORT", args.port, "")
    if not port_str and wtype in DEFAULT_PORT_BY_TYPE:
        port_str = str(DEFAULT_PORT_BY_TYPE[wtype])
    port = int(port_str) if port_str else 0

    return {
        "type": wtype,
        "gateway": gateway,
        "hmac_secret": secret,
        "name": name,
        "host": host,
        "port": port,
    }


# --------------------------------------------------------------------------
# Windows scheduled-task install / uninstall
# --------------------------------------------------------------------------
TASK_NAME = "DeskGridCloudWorker"


def _schtasks(*args: str) -> tuple[int, str]:
    """Run schtasks and return (rc, combined_output)."""
    proc = subprocess.run(
        ["schtasks"] + list(args),
        capture_output=True, text=True,
    )
    return proc.returncode, (proc.stdout or "") + (proc.stderr or "")


def install_autostart(cfg: dict) -> int:
    """Register a Windows scheduled task that runs the worker on user logon
    and restarts it on failure. Returns process exit code."""
    if sys.platform != "win32":
        print("[install] ERROR: --install is only supported on Windows.", file=sys.stderr)
        return 2

    exe = _executable_path()
    # /XML is more controllable than /TR for embedded args; here we just
    # pass the .exe path and let it read worker.conf from the same dir.
    rc, out = _schtasks(
        "/Create", "/TN", TASK_NAME,
        "/TR", f'"{exe}"',
        "/SC", "ONLOGON",
        "/RL", "LIMITED",
        "/F",  # overwrite if exists
    )
    if rc != 0:
        print(f"[install] ERROR: schtasks /Create failed:\n{out}", file=sys.stderr)
        return rc

    # Configure restart-on-failure every 1 minute, up to 3 attempts.
    settings_xml = (
        '<Settings>'
        '<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>'
        '<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>'
        '<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>'
        '<AllowHardTerminate>true</AllowHardTerminate>'
        '<StartWhenAvailable>true</StartWhenAvailable>'
        '<RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>'
        '<AllowStartOnDemand>true</AllowStartOnDemand>'
        '<Enabled>true</Enabled>'
        '<Hidden>false</Hidden>'
        '<RunOnlyIfIdle>false</RunOnlyIfIdle>'
        '<WakeToRun>false</WakeToRun>'
        '<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>'
        '<Priority>7</Priority>'
        '<RestartOnFailure>'
        '<Interval>PT1M</Interval>'
        '<Count>3</Count>'
        '</RestartOnFailure>'
        '</Settings>'
    )
    rc, out = _schtasks(
        "/Create", "/TN", TASK_NAME,
        "/TR", f'"{exe}"',
        "/SC", "ONLOGON",
        "/RL", "LIMITED",
        "/F",
    )
    # Note: schtasks /Create doesn't accept custom <Settings> XML on the
    # command line — we'd need the full /XML form. Fall back to a small
    # PowerShell snippet that adjusts RestartCount/TryInterval on the
    # existing task. Best-effort, not fatal.
    if rc == 0:
        ps_cmd = (
            f"$t = Get-ScheduledTask -TaskName '{TASK_NAME}' -ErrorAction SilentlyContinue; "
            f"if ($t) {{ "
            f"  $t.Settings.RestartCount = 3; "
            f"  $t.Settings.RestartInterval = 'PT1M'; "
            f"  Set-ScheduledTask -InputObject $t | Out-Null "
            f"}}"
        )
        subprocess.run(
            ["powershell", "-NoProfile", "-Command", ps_cmd],
            capture_output=True, text=True,
        )

    exe_display = exe.replace("\\", "/")
    conf_path = str(_exe_dir() / "worker.conf")
    print(f"[install] OK — scheduled task '{TASK_NAME}' created.")
    print(f"[install] Trigger: on user logon, restart-on-failure up to 3x every 1 min.")
    print(f"[install] Command: {exe_display}")
    print(f"[install] Make sure '{conf_path}' is filled in.")
    return 0


def uninstall_autostart() -> int:
    """Remove the scheduled task."""
    if sys.platform != "win32":
        print("[uninstall] ERROR: --uninstall is only supported on Windows.", file=sys.stderr)
        return 2
    rc, out = _schtasks("/Delete", "/TN", TASK_NAME, "/F")
    if rc == 0:
        print(f"[uninstall] OK — scheduled task '{TASK_NAME}' removed.")
    else:
        print(f"[uninstall] ERROR: schtasks /Delete failed:\n{out}", file=sys.stderr)
    return rc


# --------------------------------------------------------------------------
# Worker run
# --------------------------------------------------------------------------
def run_worker(cfg: dict) -> int:
    if not cfg["type"] or cfg["type"] not in DISPATCH_BY_TYPE:
        valid = ", ".join(sorted(DISPATCH_BY_TYPE))
        print(f"[run] ERROR: --type must be one of: {valid}", file=sys.stderr)
        return 2
    if not cfg["gateway"]:
        print("[run] ERROR: --gateway (or GATEWAY in worker.conf) is required.", file=sys.stderr)
        return 2

    # Set env so common/base.py picks up HMAC + workder host/port
    os.environ["GATEWAY_HMAC_SECRET"] = cfg["hmac_secret"]
    os.environ["WORKER_HOST"] = cfg["host"]
    os.environ["PORT"] = str(cfg["port"])
    os.environ["GATEWAY"] = cfg["gateway"]
    os.environ["WORKER_NAME"] = cfg["name"]

    import uvicorn  # imported late so --install doesn't need it

    # Lazy imports so --install/--uninstall don't need fastapi/uvicorn
    from common.base import BaseWorker
    import importlib

    tasks_mod = importlib.import_module(f"workers.{cfg['type']}.tasks")
    dispatch_names = DISPATCH_BY_TYPE[cfg["type"]]
    dispatch = {task: getattr(tasks_mod, fn_name) for task, fn_name in dispatch_names.items()}

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
    )

    worker = BaseWorker(
        worker_type=cfg["type"],
        gateway=cfg["gateway"],
        port=cfg["port"],
        name=cfg["name"],
        dispatch=dispatch,
    )
    worker.register()
    worker.start_heartbeat()

    print(f"[run] {cfg['name']} ({cfg['type']}) on :{cfg['port']} -> {cfg['gateway']}")
    uvicorn.run(worker.app, host=cfg["host"], port=cfg["port"], log_level="warning")
    return 0


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------
def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="deskgrid-cloud worker (single .exe launcher)",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--type", choices=sorted(DISPATCH_BY_TYPE),
                   help="worker type (code/doc/csv/image/audio/video)")
    p.add_argument("--gateway", help="gateway URL, e.g. http://192.168.1.178:13000")
    p.add_argument("--hmac-secret", help="GATEWAY_HMAC_SECRET shared with the gateway")
    p.add_argument("--name", help="WORKER_NAME displayed in the panel")
    p.add_argument("--host", help="WORKER_HOST (default 0.0.0.0)")
    p.add_argument("--port", help="PORT (auto from type if not set)")
    p.add_argument("--config", help="path to worker.conf (default: ./worker.conf)")
    p.add_argument("--install", action="store_true",
                   help="register Windows scheduled task for auto-start")
    p.add_argument("--uninstall", action="store_true",
                   help="remove the scheduled task")
    p.add_argument("--show-config", action="store_true",
                   help="print resolved config and exit")
    return p.parse_args()


def main() -> int:
    # Required for multiprocessing.spawn() to work inside a PyInstaller --onefile
    # bundle on Windows. When a task runs, the worker spawns a child process;
    # without freeze_support() the child re-invokes the .exe with
    # --multiprocessing-fork args that argparse rejects.
    multiprocessing.freeze_support()

    args = parse_args()
    cfg = resolve_config(args)

    if args.show_config:
        print(json.dumps(cfg, indent=2))
        return 0
    if args.install:
        return install_autostart(cfg)
    if args.uninstall:
        return uninstall_autostart()
    return run_worker(cfg)


if __name__ == "__main__":
    sys.exit(main())
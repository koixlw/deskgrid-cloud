"""deskgrid-all — one-file triple-role launcher.

When you double-click this .exe, it:
  1. Extracts the embedded `bin/gateway.exe` (bundled as PyInstaller data)
     and starts it as a detached subprocess on :13000.
  2. Polls /health until the gateway is ready.
  3. Opens the operator's default browser to the web panel
     (http://127.0.0.1:13000/) — tasks + workers + live updates live there.
  4. Turns THIS process into a worker (default --type exec) and blocks on
     uvicorn. So you become both the central node AND a compute node at
     the same time.

Modes:
  (no flag)           : full triple-role (gateway + worker + open browser)
  --client-only       : only start the gateway + open browser (no worker)
  --worker-only       : only run as a worker, assume gateway is elsewhere
  --no-browser        : same as default but skip the auto browser launch

Examples:
    deskgrid-all.exe                                ; full triple-role, type=exec
    deskgrid-all.exe --type code --port 3001        ; worker on a different port
    deskgrid-all.exe --type video --gateway-port 13001
    deskgrid-all.exe --client-only                  ; just the panel
    deskgrid-all.exe --worker-only --gateway http://office-pc-01:13000
"""
import argparse
import importlib
import multiprocessing
import os
import subprocess
import sys
import threading
import time
import webbrowser
from pathlib import Path

# --------------------------------------------------------------------------
# PyInstaller bootstrap. When frozen, _MEIPASS is the temp dir that contains
# the extracted workers/, common/, bin/gateway.exe tree. When running from
# source, fall back to the project root.
# --------------------------------------------------------------------------
if getattr(sys, "frozen", False):
    BUNDLE_DIR = Path(getattr(sys, "_MEIPASS"))
    sys.path.insert(0, str(BUNDLE_DIR / "workers"))
else:
    HERE = Path(__file__).resolve().parent
    sys.path.insert(0, str(HERE / "workers"))


# --------------------------------------------------------------------------
# Find the embedded gateway.exe inside the bundle.
# --------------------------------------------------------------------------
def extract_gateway_exe() -> Path:
    """Return the path to the bundled gateway.exe. PyInstaller puts it
    under _MEIPASS/bin/gateway.exe when we ship it as a data file."""
    bundle = Path(getattr(sys, "_MEIPASS", os.path.dirname(os.path.abspath(__file__))))
    candidates = [
        bundle / "bin" / "gateway.exe",
        bundle / "gateway.exe",
    ]
    for c in candidates:
        if c.exists():
            return c
    raise FileNotFoundError(
        f"gateway.exe not found in bundle ({bundle}). "
        "Make sure the PyInstaller spec includes ('bin/gateway.exe', 'bin') in datas."
    )


def start_gateway(gateway_exe: Path, port: int) -> subprocess.Popen:
    env = os.environ.copy()
    env["GATEWAY_ADDR"] = f":{port}"
    # Inherit stdout/stderr to the console — the gateway's own logs help
    # the operator debug connection / auth issues.
    return subprocess.Popen(
        [str(gateway_exe)],
        env=env,
        cwd=str(gateway_exe.parent.parent),
        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0,
    )


def wait_for_gateway(port: int, timeout: int = 30) -> bool:
    """Poll /health until the gateway is up (or the timeout expires)."""
    import requests
    url = f"http://127.0.0.1:{port}/health"
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(url, timeout=1)
            if r.status_code == 200:
                return True
        except Exception:
            pass
        time.sleep(0.5)
    return False


def open_browser(port: int):
    """Open the operator's default browser on the web panel."""
    url = f"http://127.0.0.1:{port}/"
    try:
        webbrowser.open(url)
    except Exception as e:
        print(f"[all] could not open browser: {e}", file=sys.stderr)


# --------------------------------------------------------------------------
# Worker mode — runs the same code path as deskgrid-worker.exe.
# --------------------------------------------------------------------------
def run_worker(args, gateway_url: str):
    os.environ["GATEWAY"] = gateway_url
    os.environ["PORT"] = str(args.port)
    os.environ["WORKER_NAME"] = args.name
    os.environ["WORKER_HOST"] = args.worker_host

    import uvicorn
    from common.base import BaseWorker
    from worker_launcher import DISPATCH_BY_TYPE

    tasks_mod = importlib.import_module(f"workers.{args.type}.tasks")
    dispatch_names = DISPATCH_BY_TYPE[args.type]
    dispatch = {task: getattr(tasks_mod, fn_name) for task, fn_name in dispatch_names.items()}

    worker = BaseWorker(
        worker_type=args.type,
        gateway=gateway_url,
        port=args.port,
        name=args.name,
        dispatch=dispatch,
    )
    worker.register()
    worker.start_heartbeat()

    print(f"[all] worker online: {args.name} ({args.type}) on :{args.port} -> {gateway_url}")
    print(f"[all] type 'Ctrl+C' (or close the window) to stop everything")
    uvicorn.run(worker.app, host=args.worker_host, port=args.port, log_level="warning")


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------
def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="deskgrid-all",
        description="Triple-role launcher: gateway + worker + web panel in one .exe",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--type", default="exec",
                   choices=["code", "doc", "csv", "image", "audio", "video", "exec"],
                   help="worker type to run as (default: exec)")
    p.add_argument("--port", type=int, default=3007,
                   help="worker port (default: 3007)")
    p.add_argument("--name", default=None,
                   help="worker name (default: deskgrid-all-<type>)")
    p.add_argument("--worker-host", default="127.0.0.1",
                   help="worker bind address (default 127.0.0.1; use 0.0.0.0 to be reachable from LAN)")
    p.add_argument("--gateway-port", type=int, default=13000,
                   help="gateway listen port (default 13000)")
    p.add_argument("--no-browser", action="store_true",
                   help="don't open the web panel automatically")
    p.add_argument("--client-only", action="store_true",
                   help="start gateway + open browser, but do NOT run a worker")
    p.add_argument("--worker-only", action="store_true",
                   help="skip gateway; assume it's running elsewhere")
    p.add_argument("--gateway", default=None,
                   help="in --worker-only mode, connect to this gateway URL")
    return p.parse_args()


# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
def main() -> int:
    # Required for multiprocessing.spawn() inside a PyInstaller --onefile
    # bundle on Windows. When a task runs, the worker spawns a child
    # process; without freeze_support() the child re-invokes the .exe
    # with --multiprocessing-fork args that argparse rejects.
    multiprocessing.freeze_support()

    args = parse_args()
    if args.name is None:
        args.name = f"deskgrid-all-{args.type}"

    print("=" * 64)
    print(f" deskgrid-all : triple-role launcher")
    print(f"   type        : {args.type}")
    print(f"   worker port : {args.port}")
    print(f"   gw port     : {args.gateway_port}")
    print("=" * 64)

    gw_proc: subprocess.Popen | None = None
    gateway_url = f"http://127.0.0.1:{args.gateway_port}"

    # 1) Start gateway (unless --worker-only)
    if not args.worker_only:
        try:
            gw_exe = extract_gateway_exe()
        except FileNotFoundError as e:
            print(f"[all] {e}")
            if not args.client_only:
                print("[all] gateway missing — refusing to start a worker with no controller.")
                return 1
        else:
            print(f"[all] launching gateway: {gw_exe}")
            gw_proc = start_gateway(gw_exe, args.gateway_port)
            if not wait_for_gateway(args.gateway_port):
                print(f"[all] gateway failed to start on :{args.gateway_port} within 30s")
                gw_proc.terminate()
                return 1
            print(f"[all] gateway ready: {gateway_url}/")

        # 2) Open browser after a short delay (let page load)
        if not args.no_browser:
            threading.Timer(1.0, open_browser, args=(args.gateway_port,)).start()
            print(f"[all] opening browser on {gateway_url}/")
    else:
        # --worker-only: connect to an existing gateway
        gateway_url = args.gateway or gateway_url
        print(f"[all] --worker-only: connecting to {gateway_url}")

    # 3) If --client-only, just stay alive (gateway already running, no worker)
    if args.client_only:
        print("[all] --client-only: gateway is up, browser is open, no worker started.")
        print("[all] press Ctrl+C to stop")
        try:
            while True:
                time.sleep(60)
        except KeyboardInterrupt:
            print("\n[all] stopping...")
        finally:
            if gw_proc:
                gw_proc.terminate()
                try:
                    gw_proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    gw_proc.kill()
        return 0

    # 4) Run the worker in this process
    try:
        run_worker(args, gateway_url)
    finally:
        if gw_proc:
            print("[all] stopping gateway...")
            gw_proc.terminate()
            try:
                gw_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                gw_proc.kill()
    return 0


if __name__ == "__main__":
    sys.exit(main())

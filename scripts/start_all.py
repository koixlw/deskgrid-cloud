#!/usr/bin/env python
"""Start gateway + 3 workers for the deskgrid-cloud cluster.

Usage: python scripts/start_all.py [--stop]
"""
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error


ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOG_DIR = os.path.join(ROOT, "logs")
GATEWAY_EXE = os.path.join(ROOT, "bin", "gateway.exe")
# Default scheme is http. If TLS is enabled via env, switch to https and tell
# workers to skip cert verification (self-signed certs are common for LAN).
_scheme = "https" if os.environ.get("GATEWAY_TLS_CERT") else "http"
if _scheme == "https":
    os.environ.setdefault("INSECURE_SKIP_TLS_VERIFY", "1")
GATEWAY_URL = f"{_scheme}://127.0.0.1:13000"
GATEWAY_PORT = 13000
WORKERS = [
    ("code",  os.path.join(ROOT, "workers", "code",  "main.py"), 3001),
    ("doc",   os.path.join(ROOT, "workers", "doc",   "main.py"), 3002),
    ("csv",   os.path.join(ROOT, "workers", "csv",   "main.py"), 3003),
    ("image", os.path.join(ROOT, "workers", "image", "main.py"), 3004),
    ("audio", os.path.join(ROOT, "workers", "audio", "main.py"), 3005),
    ("video", os.path.join(ROOT, "workers", "video", "main.py"), 3006),
]


def log(msg: str):
    print(f"[start_all] {msg}", flush=True)


def wait_for_gateway(timeout: int = 30) -> bool:
    log("waiting for gateway to be ready ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"{GATEWAY_URL}/health", timeout=2) as r:
                if r.status == 200:
                    log("gateway ready.")
                    return True
        except (urllib.error.URLError, ConnectionError, OSError):
            pass
        time.sleep(0.5)
    log("timeout waiting for gateway")
    return False


def wait_for_workers(timeout: int = 30) -> int:
    log("waiting for workers to register ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"{GATEWAY_URL}/api/workers", timeout=2) as r:
                import json
                workers = json.loads(r.read())
                online = [w for w in workers if w.get("status") == "online"]
                types = {w["type"] for w in online}
                if {"code", "doc", "csv"}.issubset(types):
                    log(f"all 3 worker types online ({len(online)} workers).")
                    return len(online)
        except Exception:
            pass
        time.sleep(1.0)
    log("timeout waiting for workers")
    return 0


def start_gateway() -> subprocess.Popen:
    os.makedirs(LOG_DIR, exist_ok=True)
    log_file = open(os.path.join(LOG_DIR, "gateway.out.log"), "ab", buffering=0)
    log(f"launching gateway on :{GATEWAY_PORT} ...")
    p = subprocess.Popen(
        [GATEWAY_EXE],
        cwd=ROOT,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0,
    )
    return p


def start_worker(name: str, script: str, port: int) -> subprocess.Popen:
    log_file = open(os.path.join(LOG_DIR, f"worker-{name}.out.log"), "ab", buffering=0)
    log(f"launching {name} worker on :{port} ...")
    env = os.environ.copy()
    env["PORT"] = str(port)
    env["GATEWAY"] = GATEWAY_URL
    p = subprocess.Popen(
        [sys.executable, script],
        cwd=ROOT,
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0,
    )
    return p


def stop_all():
    log("stopping all deskgrid-cloud processes ...")
    if sys.platform == "win32":
        for name in ("gateway", "worker-code", "worker-doc", "worker-csv"):
            subprocess.run(
                ["taskkill", "/FI", f"WINDOWTITLE eq deskgrid-cloud-{name}*", "/T", "/F"],
                capture_output=True,
            )
        # Fallback: kill by port
        for port in (GATEWAY_PORT, 3001, 3002, 3003):
            subprocess.run(
                ["powershell", "-Command",
                 f"Get-NetTCPConnection -LocalPort {port} -ErrorAction SilentlyContinue "
                 "| ForEach-Object { Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue }"],
                capture_output=True,
            )
    log("done.")


def main():
    if "--stop" in sys.argv:
        stop_all()
        return
    os.chdir(ROOT)
    os.makedirs(LOG_DIR, exist_ok=True)

    gw = start_gateway()
    if not wait_for_gateway():
        gw.terminate()
        sys.exit(1)
    for name, script, port in WORKERS:
        start_worker(name, script, port)
        time.sleep(0.5)
    n = wait_for_workers()
    print()
    print("=" * 50)
    print("  共享算力已启动")
    print(f"  总控面板:  {GATEWAY_URL}/")
    print(f"  OpenAI API: {GATEWAY_URL}/v1/chat/completions")
    print("  Code 节点:  http://localhost:3001")
    print("  Doc  节点:  http://localhost:3002")
    print("  CSV  节点:  http://localhost:3003")
    print(f"  日志目录:   {LOG_DIR}\\")
    print("  停止所有:  python scripts/start_all.py --stop")
    print("=" * 50)


if __name__ == "__main__":
    main()

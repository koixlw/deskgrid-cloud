#!/usr/bin/env python
"""Start deskgrid-cloud cluster: gateway + workers.

Supports dev mode (1 gateway, HTTP) and HA mode (2 gateways, peer replication).

Examples:
    # Dev mode (default): single gateway, HTTP, no auth
    python scripts/start_all.py

    # HA mode: two gateways, peer replication
    python scripts/start_all.py --ha

    # Production-ready: TLS + HMAC
    GATEWAY_HMAC_SECRET=$(python -c 'import secrets;print(secrets.token_hex(16))') \
    GATEWAY_TLS_CERT=certs/tcert.pem \
    GATEWAY_TLS_KEY=certs/tkey.pem \
    python scripts/start_all.py --ha

    # Stop:
    python scripts/start_all.py --stop
"""
import argparse
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error
import socket


ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOG_DIR = os.path.join(ROOT, "logs")
GATEWAY_EXE = os.path.join(ROOT, "bin", "gateway.exe")


# ---------- URL helpers ----------
def _scheme():
    """Return http or https based on TLS env."""
    return "https" if os.environ.get("GATEWAY_TLS_CERT") else "http"


def _skip_tls_verify():
    """Set INSECURE_SKIP_TLS_VERIFY for self-signed certs (LAN demo)."""
    if _scheme() == "https" and not os.environ.get("INSECURE_SKIP_TLS_VERIFY"):
        os.environ["INSECURE_SKIP_TLS_VERIFY"] = "1"
        return True
    return False


# ---------- Auto-gen HMAC secret if not set ----------
def _ensure_hmac_secret():
    """Generate a random secret if GATEWAY_HMAC_SECRET is empty, and print
    it so the operator can record it. Required for HMAC-protected deployments.
    Returns the secret string."""
    secret = os.environ.get("GATEWAY_HMAC_SECRET", "")
    if secret:
        return secret
    import secrets
    secret = secrets.token_hex(32)
    os.environ["GATEWAY_HMAC_SECRET"] = secret
    return secret


# ---------- Gateway instances ----------
def _parse_peers_csv():
    """Parse GATEWAY_PEERS env (comma-separated URLs) into a list."""
    raw = os.environ.get("GATEWAY_PEERS", "")
    return [s.strip() for s in raw.split(",") if s.strip()]


def gateway_command(port: int, peer_urls: list[str]) -> list[str]:
    """Build the command to launch one gateway instance."""
    env_overrides = {
        "GATEWAY_ADDR": f":{port}",
    }
    if peer_urls:
        env_overrides["GATEWAY_PEERS"] = ",".join(peer_urls)
    return [GATEWAY_EXE], env_overrides


# ---------- Worker registry ----------
ALL_WORKERS = [
    # (name, script_relpath, default_port)
    ("code",  "workers/code/main.py",  3001),
    ("doc",   "workers/doc/main.py",   3002),
    ("csv",   "workers/csv/main.py",   3003),
    ("image", "workers/image/main.py", 3004),
    ("audio", "workers/audio/main.py", 3005),
    ("video", "workers/video/main.py", 3006),
]


# ---------- Lifecycle ----------
def log(msg: str):
    print(f"[start_all] {msg}", flush=True)


def wait_for_gateway(url: str, label: str, timeout: int = 30) -> bool:
    log(f"waiting for {label} ({url}) to be ready ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            ctx = ssl_no_verify_context() if url.startswith("https") else None
            with urllib.request.urlopen(url + "/health", timeout=2, context=ctx) as r:
                if r.status == 200:
                    log(f"{label} ready.")
                    return True
        except (urllib.error.URLError, ConnectionError, OSError):
            pass
        time.sleep(0.5)
    log(f"timeout waiting for {label}")
    return False


def ssl_no_verify_context():
    """Build an SSL context that doesn't verify certs (for self-signed)."""
    import ssl
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def wait_for_workers(url: str, expected_types: set, timeout: int = 30) -> int:
    log(f"waiting for workers ({'+'.join(sorted(expected_types))}) ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            ctx = ssl_no_verify_context() if url.startswith("https") else None
            with urllib.request.urlopen(url + "/api/workers", timeout=2, context=ctx) as r:
                import json
                workers = json.loads(r.read())
                online = [w for w in workers if w.get("status") == "online"]
                types = {w["type"] for w in online}
                if expected_types.issubset(types):
                    log(f"all expected worker types online ({len(online)} workers).")
                    return len(online)
        except Exception:
            pass
        time.sleep(1.0)
    log("timeout waiting for workers")
    return 0


def start_process(cmd: list[str], cwd: str, env: dict, log_path: str, label: str) -> subprocess.Popen:
    os.makedirs(LOG_DIR, exist_ok=True)
    log_file = open(log_path, "ab", buffering=0)
    p = subprocess.Popen(
        cmd,
        cwd=cwd,
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0,
    )
    return p


def start_gateway_instance(port: int, peer_urls: list[str]) -> subprocess.Popen:
    cmd, overrides = gateway_command(port, peer_urls)
    log(f"launching gateway on :{port} (peers={len(peer_urls)}) ...")
    env = os.environ.copy()
    env.update(overrides)
    return start_process(
        cmd, ROOT, env,
        os.path.join(LOG_DIR, f"gateway-{port}.out.log"),
        f"gateway-{port}",
    )


def start_worker(name: str, script_rel: str, port: int, gateway_urls: list[str]) -> subprocess.Popen:
    script = os.path.join(ROOT, script_rel)
    log(f"launching {name} worker on :{port} -> {gateway_urls[0]} ...")
    env = os.environ.copy()
    env["PORT"] = str(port)
    env["GATEWAY"] = ",".join(gateway_urls)
    # Worker host binding: default 127.0.0.1 (only local gateway). Override to
    # 0.0.0.0 only when workers and gateway are on different machines and you
    # have firewall rules + HMAC enabled (worker verifies gateway's HMAC).
    env.setdefault("WORKER_HOST", "127.0.0.1")
    return start_process(
        [sys.executable, script],
        ROOT, env,
        os.path.join(LOG_DIR, f"worker-{name}.out.log"),
        f"worker-{name}",
    )


# ---------- Stop ----------
def stop_all():
    log("stopping all deskgrid-cloud processes ...")
    if sys.platform == "win32":
        for name in ("gateway", "worker-code", "worker-doc", "worker-csv"):
            subprocess.run(
                ["taskkill", "/FI", f"WINDOWTITLE eq deskgrid-cloud-{name}*", "/T", "/F"],
                capture_output=True,
            )
        # Fallback: kill by port
        for port in (13000, 13001, 3001, 3002, 3003, 3004, 3005, 3006):
            subprocess.run(
                ["powershell", "-Command",
                 f"Get-NetTCPConnection -LocalPort {port} -State Listen -ErrorAction SilentlyContinue "
                 "| ForEach-Object {{ Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue }}"],
                capture_output=True,
            )
    log("done.")


# ---------- Main ----------
def parse_args():
    p = argparse.ArgumentParser()
    p.add_argument("--stop", action="store_true", help="kill all running processes")
    p.add_argument("--ha", action="store_true",
                   help="HA mode: start a second gateway on :13001 with peer replication")
    p.add_argument("--workers", default="code,doc,csv",
                   help="comma-separated worker types to launch (default: code,doc,csv)")
    p.add_argument("--gw-port", type=int, default=13000,
                   help="primary gateway port (default 13000)")
    p.add_argument("--gw2-port", type=int, default=13001,
                   help="secondary gateway port in HA mode (default 13001)")
    return p.parse_args()


def main():
    args = parse_args()
    if args.stop:
        stop_all()
        return

    _skip_tls_verify()  # set INSECURE_SKIP_TLS_VERIFY for self-signed TLS
    _ensure_hmac_secret()

    scheme = _scheme()
    primary_url = f"{scheme}://127.0.0.1:{args.gw_port}"
    secondary_url = f"{scheme}://127.0.0.1:{args.gw2_port}" if args.ha else None

    # Worker types
    worker_types = [t.strip() for t in args.workers.split(",") if t.strip()]
    worker_specs = [w for w in ALL_WORKERS if w[0] in worker_types]

    os.chdir(ROOT)
    os.makedirs(LOG_DIR, exist_ok=True)

    # Build peer lists BEFORE starting (so each gateway knows about the other)
    peers_for_primary = [secondary_url] if args.ha else []
    peers_for_secondary = [primary_url] if args.ha else []

    # Start primary
    gw_primary = start_gateway_instance(args.gw_port, peers_for_primary)
    if not wait_for_gateway(primary_url, f"gateway-{args.gw_port}"):
        gw_primary.terminate()
        sys.exit(1)

    # Start secondary (HA mode)
    if args.ha:
        gw_secondary = start_gateway_instance(args.gw2_port, peers_for_secondary)
        if not wait_for_gateway(secondary_url, f"gateway-{args.gw2_port}"):
            log(f"warning: secondary gateway on :{args.gw2_port} did not come up")

    # Worker gateway list: primary first, secondary as backup
    worker_gw_urls = [primary_url]
    if args.ha:
        worker_gw_urls.append(secondary_url)

    # Start workers
    for name, script_rel, port in worker_specs:
        start_worker(name, script_rel, port, worker_gw_urls)
        time.sleep(0.5)

    # Wait for at least the required worker types
    expected_types = {w[0] for w in worker_specs}
    n = wait_for_workers(primary_url, expected_types)

    print()
    print("=" * 60)
    print(" deskgrid-cloud 启动完成")
    print(f" 模式:        {'HA (双网关)' if args.ha else '单网关'}")
    print(f" 传输:        {scheme.upper()}")
    print(f" 鉴权 token:  {'已启用' if os.environ.get('GATEWAY_TOKEN') else '未启用 (dev mode)'}")
    print(f" HMAC:        {'已启用' if os.environ.get('GATEWAY_HMAC_SECRET') else '未启用 (dev mode)'}")
    print(f" 总控面板:    {primary_url}/")
    print(f" OpenAI API:  {primary_url}/v1/chat/completions")
    if args.ha:
        print(f" 备用面板:    {secondary_url}/")
    print(f" 日志目录:    {LOG_DIR}\\")
    print(" 停止所有:    python scripts/start_all.py --stop")
    print("=" * 60)


if __name__ == "__main__":
    main()
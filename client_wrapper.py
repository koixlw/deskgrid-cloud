# deskgrid-cloud client — `deskgrid-submit`
#
# Wraps a local shell command. By default (`--mode auto`):
#   * read local CPU/Mem with psutil
#   * if free (CPU < threshold AND Mem < threshold) → run locally via subprocess
#   * if busy                  → submit `exec_run` to the gateway
#   * if gateway has no online exec worker → local fallback + warn
#
# Modes:
#   --mode auto    : smart decide (default)
#   --mode local   : always run locally
#   --mode remote  : always submit to gateway
#
# With `--watch` (default on), the CLI polls /api/tasks/<id> until the
# task lands in done/failed, then prints the worker result and exits
# with a meaningful exit code. Without `--watch` it returns immediately
# after submission, printing only the task id.
"""
deskgrid-submit — wrap a local command for execution on a deskgrid cluster.

Examples:
    # Auto: local if free, else remote
    deskgrid-submit --cmd "python train.py --epochs 3"

    # Force remote submission
    deskgrid-submit --mode remote --cmd "ffmpeg -i in.mp4 out.mp4"

    # Run locally no matter what (just a fancy passthrough)
    deskgrid-submit --mode local --cmd "echo hello"

    # With cwd + extra env, sync-block on result
    deskgrid-submit --mode remote --cmd "python script.py" \\
                    --cwd "E:\\\\projects\\\\train" --env "CUDA_VISIBLE_DEVICES=0" \\
                    --watch --timeout 1800

    # Dry-run + JSON output (for shell pipelines)
    deskgrid-submit --mode remote --cmd "ls" --quiet

All configuration can also live in `client.conf` next to the .exe:
    GATEWAY=http://gateway.lan:13000
    GATEWAY_TOKEN=
    GATEWAY_HMAC_SECRET=
    CPU_THRESHOLD=80
    MEM_THRESHOLD=85
    LOCAL_FALLBACK=1
"""
import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Optional

import requests


# --------------------------------------------------------------------------
# Conf loader — duplicated from worker_launcher.py to avoid pulling fastapi
# into the client bundle. ~15 lines, kept faithful to the original.
# --------------------------------------------------------------------------
def _exe_dir() -> Path:
    if getattr(sys, "frozen", False):
        return Path(sys.executable).parent
    return Path(__file__).resolve().parent


def _find_conf(explicit: Optional[str]) -> Optional[Path]:
    if explicit:
        return Path(explicit)
    for cand in (_exe_dir() / "client.conf", Path.cwd() / "client.conf"):
        if cand.exists():
            return cand
    return None


def load_conf(explicit: Optional[str]) -> dict:
    p = _find_conf(explicit)
    if not p:
        return {}
    cfg = {}
    for raw in p.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        cfg[k.strip().upper()] = v.strip()
    return cfg


def _pick(cfg: dict, key: str, cli_val, default):
    if cli_val is not None and cli_val != "":
        return cli_val
    if key in cfg and cfg[key] != "":
        return cfg[key]
    return default


# --------------------------------------------------------------------------
# Auth helpers (HMAC header) — duplicated from common/base.py for the same
# bundle-size reason.
# --------------------------------------------------------------------------
def _maybe_sign(body: bytes, secret: str):
    """Return {} if no secret, else {X-Signature, X-Timestamp} headers."""
    if not secret:
        return {}
    import hashlib
    import hmac as _hmac
    ts = int(time.time())
    h = _hmac.new(secret.encode(), digestmod=hashlib.sha256)
    h.update(str(ts).encode())
    h.update(b":")
    h.update(body)
    return {"X-Timestamp": str(ts), "X-Signature": h.hexdigest()}


def _post_json(url: str, body: dict, token: str, secret: str, timeout: float = 10):
    raw = json.dumps(body, ensure_ascii=False).encode("utf-8")
    headers = {"Content-Type": "application/json; charset=utf-8"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    headers.update(_maybe_sign(raw, secret))
    return requests.post(url, data=raw, headers=headers, timeout=timeout)


def _get_json(url: str, token: str, secret: str, timeout: float = 5):
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    raw = b""
    headers.update(_maybe_sign(raw, secret))
    return requests.get(url, headers=headers, timeout=timeout)


# --------------------------------------------------------------------------
# Local CPU/Mem snapshot — duplicated from workers/common/hardware.py.
# --------------------------------------------------------------------------
def local_usage() -> dict:
    import psutil
    cpu = psutil.cpu_percent(interval=0.3)
    mem = psutil.virtual_memory().percent
    return {"cpu_usage": float(cpu), "mem_usage": float(mem)}


def gateway_exec_workers_online(gateway: str, token: str, secret: str) -> list:
    """Return list of online exec workers (or [])."""
    try:
        r = _get_json(f"{gateway.rstrip('/')}/api/workers", token, secret, timeout=5)
        if r.status_code != 200:
            return []
        workers = r.json()
    except Exception:
        return []
    out = []
    for w in workers:
        if w.get("type") == "exec" and w.get("status") == "online":
            out.append(w)
    return out


# --------------------------------------------------------------------------
# Decision: where should this command run?
# --------------------------------------------------------------------------
def decide_mode(args, cfg) -> tuple[str, str]:
    """Returns (mode, reason). `mode` in {local, remote}."""
    if args.mode == "local":
        return "local", "--mode local"
    if args.mode == "remote":
        return "remote", "--mode remote"
    # auto
    cpu_th = float(_pick(cfg, "CPU_THRESHOLD", args.cpu_threshold, "80"))
    mem_th = float(_pick(cfg, "MEM_THRESHOLD", args.mem_threshold, "85"))
    usage = local_usage()
    if usage["cpu_usage"] < cpu_th and usage["mem_usage"] < mem_th:
        return "local", (
            f"cpu={usage['cpu_usage']:.1f}%<{cpu_th} "
            f"mem={usage['mem_usage']:.1f}%<{mem_th}"
        )
    workers = gateway_exec_workers_online(
        _pick(cfg, "GATEWAY", args.gateway, "http://127.0.0.1:13000"),
        _pick(cfg, "GATEWAY_TOKEN", args.token, ""),
        _pick(cfg, "GATEWAY_HMAC_SECRET", args.hmac_secret, ""),
    )
    if not workers and _pick(cfg, "LOCAL_FALLBACK", args.local_fallback, "1") not in ("0", "false", "False"):
        return "local", f"remote busy/no-exec-workers; fallback to local"
    return "remote", (
        f"cpu={usage['cpu_usage']:.1f}%>={cpu_th} or "
        f"mem={usage['mem_usage']:.1f}%>={mem_th}; exec pool={len(workers)}"
    )


# --------------------------------------------------------------------------
# Local run
# --------------------------------------------------------------------------
def run_local(cmd: list[str], cwd: Optional[str], env: dict) -> int:
    """Passthrough subprocess.run, return exit code."""
    print(f"[local] running: {' '.join(cmd)}  (cwd={cwd or os.getcwd()})", file=sys.stderr)
    merged_env = os.environ.copy()
    merged_env.update(env)
    try:
        proc = subprocess.run(cmd, cwd=cwd, env=merged_env)
        return proc.returncode
    except FileNotFoundError as e:
        print(f"[local] ERROR: {e}", file=sys.stderr)
        return 127


# --------------------------------------------------------------------------
# Remote submit + poll
# --------------------------------------------------------------------------
def submit_remote(gateway: str, cmd: list[str], cwd: Optional[str], env: dict,
                   timeout: int, priority: int, max_retries: int,
                   token: str, secret: str, watch: bool, poll_ms: int,
                   quiet: bool) -> int:
    payload_obj = {"cmd": cmd}
    if cwd:
        payload_obj["cwd"] = cwd
    if env:
        payload_obj["env"] = env
    if timeout:
        payload_obj["timeout"] = timeout

    body = {
        "type": "exec_run",
        "payload": json.dumps(payload_obj, ensure_ascii=False),
        "source": "submit",
        "priority": priority,
        "max_retries": max_retries,
        "timeout_secs": timeout + 10 if timeout else 0,
    }
    if not quiet:
        print(f"[remote] POST {gateway.rstrip('/')}/api/tasks  type=exec_run", file=sys.stderr)
    try:
        r = _post_json(f"{gateway.rstrip('/')}/api/tasks", body, token, secret, timeout=10)
    except Exception as e:
        print(f"[remote] network error: {e}", file=sys.stderr)
        return 2
    if r.status_code >= 300:
        print(f"[remote] gateway rejected: {r.status_code} {r.text[:300]}", file=sys.stderr)
        return 2
    task = r.json()
    task_id = task.get("id", "")
    if not watch:
        print(task_id)
        return 0

    if not quiet:
        print(f"[remote] task_id={task_id}  polling every {poll_ms}ms ...", file=sys.stderr)
    deadline = time.time() + (timeout + 60 if timeout else 3600)
    last_progress = -1
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    while time.time() < deadline:
        time.sleep(max(0.1, poll_ms / 1000.0))
        try:
            r = _get_json(f"{gateway.rstrip('/')}/api/tasks/{task_id}", token, secret, timeout=5)
            if r.status_code != 200:
                continue
            t = r.json()
        except Exception:
            continue
        prog = t.get("progress", 0)
        if prog != last_progress and not quiet:
            print(f"[remote] progress={prog}% status={t.get('status')}", file=sys.stderr)
            last_progress = prog
        if t.get("status") in ("done", "failed"):
            result = t.get("result") or ""
            err = t.get("error") or ""
            if t.get("status") == "done":
                if quiet:
                    print(result)
                else:
                    print("----- result -----", file=sys.stderr)
                    try:
                        parsed = json.loads(result)
                        print(json.dumps(parsed, indent=2, ensure_ascii=False))
                    except Exception:
                        print(result, file=sys.stderr)
                return 0
            else:
                print(f"[remote] task failed: {err}", file=sys.stderr)
                if result:
                    print(result, file=sys.stderr)
                return 3
    print(f"[remote] timeout waiting for task {task_id}", file=sys.stderr)
    return 4


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------
def parse_env_args(items: list[str]) -> dict:
    out = {}
    for item in items or []:
        if "=" not in item:
            raise SystemExit(f"--env expects KEY=VAL, got {item!r}")
        k, v = item.split("=", 1)
        out[k] = v
    return out


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="deskgrid-submit",
        description="Wrap a local command. Auto-routes to a free worker if your box is busy.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--cmd", nargs=argparse.REMAINDER, default=[],
                   help="command + args (everything after --cmd)")
    p.add_argument("--cwd", help="working dir (must be in client-side allow-list when remote)")
    p.add_argument("--env", action="append", default=[],
                   help="KEY=VAL (repeatable)")
    p.add_argument("--timeout", type=int, default=600,
                   help="subprocess timeout in seconds (clamped server-side to WORKER_TASK_TIMEOUT-5)")
    p.add_argument("--priority", type=int, default=0)
    p.add_argument("--max-retries", type=int, default=0)
    p.add_argument("--mode", choices=["auto", "local", "remote"], default="auto")
    p.add_argument("--cpu-threshold", type=float, default=None)
    p.add_argument("--mem-threshold", type=float, default=None)
    p.add_argument("--local-fallback", default=None,
                   help="1/0/yes/no: when remote is unreachable, run locally")
    p.add_argument("--gateway", default=None)
    p.add_argument("--token", default=None, help="GATEWAY_TOKEN bearer")
    p.add_argument("--hmac-secret", default=None)
    p.add_argument("--config", default=None, help="path to client.conf")
    p.add_argument("--watch", dest="watch", action="store_true", default=True)
    p.add_argument("--no-watch", dest="watch", action="store_false")
    p.add_argument("--poll-ms", type=int, default=1000)
    p.add_argument("--quiet", action="store_true")
    return p.parse_args()


def main() -> int:
    args = parse_args()
    if not args.cmd:
        print("ERROR: --cmd is required", file=sys.stderr)
        return 64
    cfg = load_conf(args.config)
    env = parse_env_args(args.env)

    mode, why = decide_mode(args, cfg)
    if not args.quiet:
        print(f"[submit] mode={mode}  reason={why}", file=sys.stderr)

    if mode == "local":
        rc = run_local(args.cmd, args.cwd, env)
        if not args.quiet and rc != 0:
            print(f"[local] exit={rc}", file=sys.stderr)
        return rc

    # remote
    gateway = _pick(cfg, "GATEWAY", args.gateway, "http://127.0.0.1:13000")
    token = _pick(cfg, "GATEWAY_TOKEN", args.token, "")
    secret = _pick(cfg, "GATEWAY_HMAC_SECRET", args.hmac_secret, "")
    return submit_remote(
        gateway=gateway, cmd=args.cmd, cwd=args.cwd, env=env,
        timeout=args.timeout, priority=args.priority,
        max_retries=args.max_retries,
        token=token, secret=secret,
        watch=args.watch, poll_ms=args.poll_ms, quiet=args.quiet,
    )


if __name__ == "__main__":
    sys.exit(main())

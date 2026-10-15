"""Exec worker tasks - run arbitrary shell commands in a constrained sandbox.

Single task type: `exec_run`. Designed for `deskgrid-submit`'s `--mode
auto|remote` path: when the local machine is busy, the command and its
cwd/env are wrapped into a JSON payload and dispatched to a remote
exec worker.

Security model:
  * The client is expected to be authenticated (HMAC + token). Workers
    trust whatever the gateway forwards.
  * `cwd` MUST resolve inside `OUTPUT_DIR` (artifacts/) or one of the
    `EXEC_ALLOWED_CWD_ROOTS` (semicolon-separated) prefixes; otherwise
    the task is rejected with PermissionError.
  * `env` is filtered: a list of OS-protected keys (PATH, USERPROFILE,
    ...) cannot be overridden.
  * The subprocess runs in its own process group so a SIGTERM-equivalent
    reached via `proc.terminate()` will also kill any grandchildren the
    script may have spawned (python child → ffmpeg child, etc.).
  * `timeout` is clamped to `WORKER_TASK_TIMEOUT - 5s` so the worker's
    watchdog has a 5s grace window before it fires a hard kill.
  * stdout / stderr are each capped at 1 MB; anything beyond is truncated
    with a marker line.

Payload schema (dict or JSON string):
    {
      "cmd":   ["python", "train.py", "--epochs", "3"],   # required, list[str]
      "cwd":   "E:\\\\deskgrid-cloud\\\\artifacts",         # optional, default OUTPUT_DIR/<task_id[:8]>
      "env":   {"FOO": "bar"},                            # optional, dict[str,str]
      "timeout": 600                                      # optional, default 600s
    }
"""
import json
import os
import shlex
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import TaskCancelled, cancellable_sleep  # noqa: E402

# ---------- output dir + allowed cwd roots ----------
OUTPUT_DIR = Path(os.environ.get(
    "EXEC_OUTPUT_DIR",
    os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "..", "artifacts"),
)).resolve()
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

def _parse_allowed_roots():
    """Read EXEC_ALLOWED_CWD_ROOTS (semicolon-separated) and merge OUTPUT_DIR."""
    raw = os.environ.get("EXEC_ALLOWED_CWD_ROOTS", "").strip()
    roots = [OUTPUT_DIR]
    if raw:
        for piece in raw.split(";"):
            piece = piece.strip()
            if not piece:
                continue
            try:
                roots.append(Path(piece).resolve())
            except Exception:
                pass
    return roots

ALLOWED_CWD_ROOTS = _parse_allowed_roots()

# ---------- protected env keys (case-insensitive blacklist) ----------
_PROTECTED_ENV_KEYS = {
    "PATH", "PYTHONPATH", "PATHEXT",
    "SYSTEMROOT", "WINDIR", "COMSPEC",
    "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
    "USERNAME", "USERDOMAIN", "SESSIONNAME", "LOGONSERVER",
    "ALLUSERSPROFILE", "PROGRAMDATA",
    "PROCESSOR_ARCHITECTURE", "OS", "HOME", "TMP", "TEMP",
}
# ---------- capture limits ----------
_MAX_CAPTURE_BYTES = 1 * 1024 * 1024   # 1 MB per stream


def _parse_payload(payload):
    """Accept dict or JSON string. Tolerate a bare shell line as `cmd`."""
    if isinstance(payload, dict):
        return payload
    if isinstance(payload, str):
        s = payload.strip()
        if not s:
            return {}
        if s.startswith("{"):
            try:
                return json.loads(s)
            except Exception:
                pass
        # Treat as a single shell command line — split it into argv.
        return {"cmd": shlex.split(s)}
    return {}


def _resolve_cwd(cwd_in: str | None, task_id: str) -> Path:
    """Validate cwd against the allow-list. Returns the resolved Path."""
    if not cwd_in:
        target = OUTPUT_DIR / task_id[:8]
    else:
        target = Path(cwd_in)
    try:
        real = target.resolve(strict=False)
    except Exception as e:
        raise PermissionError(f"cannot resolve cwd {target!r}: {e}")
    for root in ALLOWED_CWD_ROOTS:
        try:
            real.relative_to(root)
            real.mkdir(parents=True, exist_ok=True)
            return real
        except ValueError:
            continue
    allowed_display = ", ".join(str(p) for p in ALLOWED_CWD_ROOTS)
    raise PermissionError(
        f"cwd not in allowed roots ({allowed_display}): got {real}"
    )


def _filter_env(env_in: dict | None) -> dict:
    """Merge user env into a fresh dict based on the inherited env, after
    dropping any protected key the caller tried to override."""
    base = os.environ.copy()
    if not env_in:
        return base
    for k, v in env_in.items():
        if not isinstance(k, str) or not isinstance(v, (str, int, float)):
            raise PermissionError(f"env entries must be string keys + scalar values; bad: {k!r}")
        if k.upper() in _PROTECTED_ENV_KEYS:
            raise PermissionError(f"env override blocked: {k!r}")
        base[k] = str(v)
    return base


def _resolve_timeout(timeout_in) -> int:
    """Clamp to (1, WORKER_TASK_TIMEOUT - 5s) so the gateway watchdog
    fires after we have given the process a fair chance to clean up."""
    worker_timeout = int(os.environ.get("WORKER_TASK_TIMEOUT", "300"))
    try:
        t = int(timeout_in)
    except (TypeError, ValueError):
        t = worker_timeout - 30
    t = min(t, max(5, worker_timeout - 5))
    return max(5, t)


def _truncate(s: str, n: int) -> tuple[str, bool]:
    """Truncate a capture stream to `n` bytes (utf-8 count, lossy on big strings)."""
    if s is None:
        return "", False
    if len(s) <= n:
        return s, False
    return s[:n] + f"\n...[truncated, original {len(s)} bytes]", True


def _platform_creationflags():
    """Windows: spawn in its own process group so proc.terminate() kills
    the entire tree (the script's children + grandchildren)."""
    if sys.platform == "win32":
        # CREATE_NEW_PROCESS_GROUP = 0x00000200
        return subprocess.CREATE_NEW_PROCESS_GROUP
    return 0


def _platform_start_new_session():
    """POSIX: equivalent is start_new_session=True (setsid)."""
    return sys.platform != "win32"


# ---------- single task function ----------
def exec_cmd(task_id: str, payload, reporter, cancel_event=None) -> dict:
    """Run a shell command. Returns exit_code / stdout / stderr / elapsed."""
    p = _parse_payload(payload)

    cmd = p.get("cmd")
    if not cmd or not isinstance(cmd, list) or not all(isinstance(x, str) for x in cmd):
        raise ValueError("exec_run requires `cmd` as a non-empty list[str]")

    reporter.progress(5, f"[exec] validate")
    cwd = _resolve_cwd(p.get("cwd"), task_id)
    env = _filter_env(p.get("env") or {})
    timeout = _resolve_timeout(p.get("timeout"))
    cmd_display = " ".join(shlex.quote(x) for x in cmd[:6])
    if len(cmd) > 6:
        cmd_display += f" ... (+{len(cmd) - 6} args)"

    reporter.progress(10, f"[exec] spawn: {cmd_display}  cwd={cwd}  timeout={timeout}s")
    started = time.time()
    creationflags = _platform_creationflags()
    start_new_session = _platform_start_new_session()

    try:
        proc = subprocess.Popen(
            cmd,
            cwd=str(cwd),
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL,
            text=True,
            errors="replace",
            creationflags=creationflags,
            start_new_session=start_new_session,
        )
    except FileNotFoundError as e:
        raise RuntimeError(f"command not found: {cmd[0]!r} (PATH={env.get('PATH','')!r})")
    except Exception as e:
        raise RuntimeError(f"Popen failed: {e}")

    cancelled = False
    deadline = started + timeout
    try:
        while True:
            rc = proc.poll()
            if rc is not None:
                break
            if cancel_event is not None and cancel_event.is_set():
                cancelled = True
                _terminate_tree(proc)
                break
            if time.time() > deadline:
                _terminate_tree(proc)
                raise TimeoutError(f"exec_cmd exceeded timeout={timeout}s")
            cancellable_sleep(0.5, cancel_event)
    finally:
        watchdog_cancel = (time.time() - started) > timeout

    try:
        out, err = proc.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        _terminate_tree(proc)
        out, err = "", ""
    elapsed = round(time.time() - started, 3)

    out_t, out_trunc = _truncate(out or "", _MAX_CAPTURE_BYTES)
    err_t, err_trunc = _truncate(err or "", _MAX_CAPTURE_BYTES)

    if cancelled:
        # raise TaskCancelled so BaseWorker surfaces a "cancelled" error
        raise TaskCancelled(f"exec cancelled after {elapsed}s")

    reporter.progress(
        100,
        f"[exec] exit={proc.returncode} elapsed={elapsed}s out={len(out or '')}B err={len(err or '')}B",
    )
    return {
        "exit_code": proc.returncode,
        "elapsed_sec": elapsed,
        "cwd": str(cwd),
        "command": cmd,
        "command_display": cmd_display,
        "stdout": out_t,
        "stderr": err_t,
        "stdout_truncated": out_trunc,
        "stderr_truncated": err_trunc,
        "cancelled": False,
        "timed_out": watchdog_cancel,
    }


def _terminate_tree(proc: subprocess.Popen):
    """SIGTERM the whole process group, fall back to SIGKILL after 3s."""
    try:
        proc.terminate()
    except Exception:
        pass
    try:
        proc.wait(timeout=3)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        proc.kill()
    except Exception:
        pass
    try:
        proc.wait(timeout=2)
    except Exception:
        pass

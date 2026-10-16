"""BaseWorker - common scaffolding for code/doc/csv/image/audio/video workers.

Architecture note:
Each task runs in a SEPARATE PROCESS (multiprocessing.Process), not a thread.
This solves the Python GIL: heavy CPU tasks (image generation, video encoding)
no longer block the FastAPI server thread, the heartbeat thread, or progress
reporting. Each task can also be hard-killed by the watchdog if it exceeds
the timeout, regardless of whether it's stuck in a tight C extension.

Includes:
- Stable worker ID: WORKER_ID env > hostname > MAC hash > random UUID
- Faster heartbeat (3s) with GPU usage reporting
- Cooperative cancellation via multiprocessing.Event
- Watchdog: hard-kill (terminate()) subprocess after TaskTimeoutSecs
- Periodic self-restart after N tasks (prevent Python memory leaks)
"""
import hashlib
import hmac
import json as _json_mod
import logging
import multiprocessing
import os
import sys
import threading
import time
import traceback
import uuid
from typing import Callable, Optional

import psutil
import requests
import urllib3
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse

from .hardware import collect as collect_hw, snapshot_usage
from .capabilities import collect as collect_caps

log = logging.getLogger("worker")

# Default limits - override via env if needed
DEFAULT_HEARTBEAT_INTERVAL = float(os.environ.get("WORKER_HEARTBEAT_INTERVAL", "3"))
DEFAULT_RESTART_AFTER_TASKS = int(os.environ.get("WORKER_RESTART_AFTER_TASKS", "500"))
DEFAULT_TIMEOUT_DEFAULT = int(os.environ.get("WORKER_TASK_TIMEOUT", "300"))
# Worker bind address. Default 127.0.0.1 (safer; only local gateway reaches us).
# Override with WORKER_HOST=0.0.0.0 to allow other machines on the LAN to
# reach us — but then anyone on the network can submit tasks if they know
# the HMAC secret.
WORKER_HOST = os.environ.get("WORKER_HOST", "127.0.0.1")
# Sandbox: restrict subprocess to a specific cwd. Default = project_root/artifacts.
SANDBOX_ARTIFACTS_DIR = os.environ.get(
    "WORKER_SANDBOX_DIR",
    os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "..", "artifacts"),
)


# HMAC support: workers sign requests with shared secret.
HMAC_SECRET = os.environ.get("GATEWAY_HMAC_SECRET", "").encode()
INSECURE_SKIP_TLS = os.environ.get("INSECURE_SKIP_TLS_VERIFY", "0") == "1"

if HMAC_SECRET:
    import hashlib
    log.info("HMAC signing ENABLED (shared secret set, sha256=%s)",
             hashlib.sha256(HMAC_SECRET).hexdigest()[:12])
if INSECURE_SKIP_TLS:
    log.warning("INSECURE_SKIP_TLS_VERIFY=1: not verifying TLS certificate (demo only)")
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)


def _sign_request(body: bytes, ts: int) -> str:
    """HMAC-SHA256 hex signature of timestamp:body using HMAC_SECRET."""
    h = hmac.new(HMAC_SECRET, digestmod=hashlib.sha256)
    h.update(str(ts).encode())
    h.update(b":")
    h.update(body)
    return h.hexdigest()


def signed_post(url: str, payload: dict, timeout: float = 5):
    """POST payload to gateway, optionally HMAC-signed."""
    if not HMAC_SECRET:
        return requests.post(url, json=payload, timeout=timeout, verify=not INSECURE_SKIP_TLS)
    body = _json_mod.dumps(payload, ensure_ascii=False).encode()
    ts = int(time.time())
    sig = _sign_request(body, ts)
    headers = {
        "Content-Type": "application/json",
        "X-Signature": sig,
        "X-Timestamp": str(ts),
    }
    return requests.post(url, data=body, headers=headers, timeout=timeout, verify=not INSECURE_SKIP_TLS)


def _verify_request(body: bytes, signature: str, ts_str: str, max_skew_sec: int = 300) -> bool:
    """Verify an HMAC-signed request. Returns True if valid."""
    if not HMAC_SECRET:
        return True  # dev mode, no HMAC required
    if not signature or not ts_str:
        return False
    try:
        ts = int(ts_str)
    except (ValueError, TypeError):
        return False
    now = int(time.time())
    if abs(now - ts) > max_skew_sec:
        return False
    expected = _sign_request(body, ts)
    return hmac.compare_digest(expected, signature.lower())


class TaskCancelled(Exception):
    """Raised inside a task function when cancellation has been requested."""
    pass


def cancellable_sleep(seconds: float, cancel_event, pause_event=None, step: float = 0.5) -> None:
    """Sleep that wakes up early when cancellation is requested, and blocks
    while pause_event is set (deadline does NOT drift during pause).

    Works with both threading.Event (in-process) and multiprocessing.Event
    (cross-process). For multiprocessing.Event, is_set() is a syscall, so we
    keep step small.
    """
    if cancel_event is None or seconds <= 0:
        return
    end = time.time() + seconds
    while time.time() < end:
        if cancel_event.is_set():
            raise TaskCancelled("cancelled during sleep")
        if pause_event is not None and pause_event.is_set():
            # Block until pause clears (or cancel fires). Push the absolute
            # deadline forward by the pause duration so the original sleep
            # budget is preserved.
            pause_started = time.time()
            wait_if_paused(pause_event, cancel_event)
            end += time.time() - pause_started
            continue
        time.sleep(min(step, end - time.time()))


def check_cancel(cancel_event, label: str = "") -> None:
    """Raise TaskCancelled immediately if cancellation has been requested."""
    if cancel_event is not None and cancel_event.is_set():
        raise TaskCancelled(f"cancelled{' at ' + label if label else ''}")


def wait_if_paused(pause_event, cancel_event=None, step: float = 0.05) -> None:
    """Block while pause_event is set; raise TaskCancelled if cancel fires.

    Cooperative — the caller decides when to check. step is the polling
    interval in seconds; small enough that resume feels instant.
    """
    if pause_event is None:
        return
    while pause_event.is_set():
        if cancel_event is not None and cancel_event.is_set():
            raise TaskCancelled("cancelled while paused")
        time.sleep(step)


def check_pause(pause_event, cancel_event=None) -> None:
    """Block until pause is cleared (with cancel short-circuit)."""
    wait_if_paused(pause_event, cancel_event)


def _stable_worker_id(port: int, worker_type: str) -> str:
    """Resolve a stable worker ID. Includes type+port so multiple workers on the
    same host get distinct IDs."""
    explicit = os.environ.get("WORKER_ID")
    if explicit:
        return f"fixed:{explicit}"
    try:
        host = socket.gethostname()
        if host:
            return f"host:{host}:{worker_type}:{port}"
    except Exception:
        pass
    try:
        node = uuid.getnode()
        mac = ":".join(f"{(node >> i) & 0xff:02x}" for i in range(0, 48, 8))
        return f"mac:{mac}:{worker_type}:{port}"
    except Exception:
        pass
    return f"uuid:{worker_type}:{port}:{uuid.uuid4().hex}"


def _detect_gpus() -> list[dict]:
    """Enumerate every GPU on this host exactly once at import time.

    Returns a list of ``{index, name, uuid}`` records in physical slot order.
    GPUs are not hot-plugged on the workers we target, so caching the
    (index, model, uuid) trio at startup is fine — and keeps each heartbeat
    cheap (we only re-query the volatile bits: memory + temperature).

    Returns ``[]`` if pynvml isn't installed or NVML fails to initialise.
    """
    try:
        import pynvml  # type: ignore
        pynvml.nvmlInit()
        n = pynvml.nvmlDeviceGetCount()
        out: list[dict] = []
        for i in range(n):
            try:
                h = pynvml.nvmlDeviceGetHandleByIndex(i)
                try:
                    name = pynvml.nvmlDeviceGetName(h)
                    if isinstance(name, bytes):
                        name = name.decode("utf-8", errors="replace")
                except Exception:
                    name = f"GPU #{i}"
                try:
                    uuid = pynvml.nvmlDeviceGetUUID(h)
                    if isinstance(uuid, bytes):
                        uuid = uuid.decode("utf-8", errors="replace")
                except Exception:
                    uuid = ""
            except Exception:
                # pynvml can fail on a single bad device without killing the rest
                continue
            out.append({"index": int(i), "name": str(name), "uuid": str(uuid)})
        return out
    except Exception:
        return []


# Cache the device table at import. _GPU_TABLE may be [] on a non-GPU host;
# that's fine — the heartbeat / register paths become no-ops for GPU fields.
_GPU_TABLE: list[dict] = _detect_gpus()


def _read_gpu_info() -> tuple[int, int, list[dict]]:
    """Best-effort read of every GPU's memory + temperature.

    Returns ``(used_mb, total_mb, detail)`` where ``used_mb`` / ``total_mb``
    are the primary (lowest-index) GPU's bytes — preserved as a scalar pair
    so the gateway's existing ``MinGPUFreeMB`` scheduler check keeps working
    unchanged. ``detail`` is the per-card list for the dashboard:

        [{"index": 0, "name": "NVIDIA GeForce RTX 2080",
          "mem_used_mb": 1234, "mem_total_mb": 8192, "temp_c": 42}, ...]

    Hosts without pynvml (CPU-only workers, Apple Silicon, etc.) yield
    ``(0, 0, [])``.
    """
    if not _GPU_TABLE:
        return 0, 0, []
    detail: list[dict] = []
    primary_used = primary_total = 0
    try:
        import pynvml  # type: ignore
        pynvml.nvmlInit()
        for entry in _GPU_TABLE:
            idx = entry["index"]
            try:
                h = pynvml.nvmlDeviceGetHandleByIndex(idx)
                mem = pynvml.nvmlDeviceGetMemoryInfo(h)
                used_mb = int(mem.used // (1024 * 1024))
                total_mb = int(mem.total // (1024 * 1024))
            except Exception:
                used_mb = total_mb = 0
            temp_c = 0
            try:
                temp_c = int(pynvml.nvmlDeviceGetTemperature(
                    h, pynvml.NVML_TEMPERATURE_GPU))
            except Exception:
                pass
            detail.append({
                "index":        idx,
                "name":         entry["name"],
                "mem_used_mb":  used_mb,
                "mem_total_mb": total_mb,
                "temp_c":       temp_c,
            })
        if detail:
            primary_used = detail[0]["mem_used_mb"]
            primary_total = detail[0]["mem_total_mb"]
    except Exception:
        pass
    return primary_used, primary_total, detail


def _task_subprocess_entry(gateway: str, worker_id: str, task_id: str, task_type: str,
                           payload, fn_bytes: bytes, cancel_event, pause_event=None):
    """Runs in a fresh subprocess. Picks up the task function from its module
    path (fn_bytes) and executes it. Posts result to gateway directly.

    This function applies a sandbox before executing the user task:
      - chdir to a restricted directory (default artifacts/)
      - clear sensitive env vars (USERPROFILE, APPDATA, ...)
      - on Linux, apply rlimit for CPU and memory
    """
    import importlib
    import json as _json
    import sys as _sys
    import traceback as _traceback

    # With "spawn" on Windows/macOS, child inherits an empty default sys.path.
    # Restore the project root so we can find workers.<type>.tasks.
    _project_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    if _project_root not in _sys.path:
        _sys.path.insert(0, _project_root)

    # ---------- SANDBOX ----------
    try:
        os.makedirs(SANDBOX_ARTIFACTS_DIR, exist_ok=True)
        os.chdir(SANDBOX_ARTIFACTS_DIR)
    except Exception:
        pass
    # Drop env vars that could leak user data or expand attack surface
    for k in [
        "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
        "USERNAME", "USERDOMAIN", "SESSIONNAME", "LOGONSERVER",
        "ALLUSERSPROFILE", "PROGRAMDATA", "COMMONPROGRAMWISES", "WINDIR",
        "PYTHONPATH",  # already set sys.path explicitly
    ]:
        os.environ.pop(k, None)
    # Strip PATH to system dirs only (prevent running arbitrary user-installed exe)
    if sys.platform == "win32":
        os.environ["PATH"] = r"C:\Windows\System32;C:\Windows"
    else:
        os.environ["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

    # Resource limits (Linux only; resource module doesn't exist on Windows)
    try:
        import resource
        # soft=120s CPU, hard=130s (slightly past)
        resource.setrlimit(resource.RLIMIT_CPU, (120, 130))
        # 1GB address space (most ML fits; hard=1.2GB to allow fork/exec)
        resource.setrlimit(resource.RLIMIT_AS, (1_073_741_824, 1_200_000_000))
        # 256MB stack to avoid stack overflow attacks
        resource.setrlimit(resource.RLIMIT_STACK, (64 * 1024 * 1024, 256 * 1024 * 1024))
    except ImportError:
        pass  # Windows
    except Exception:
        pass

    info = _json.loads(fn_bytes.decode())
    mod = importlib.import_module(info["module"])
    fn = getattr(mod, info["name"])

    def post_result(success: bool, result: dict = None, error: str = ""):
        url = f"{gateway}/api/workers/{worker_id}/result"
        try:
            result_str = _json.dumps(result or {}, ensure_ascii=False)
        except Exception:
            result_str = _json.dumps({"raw": str(result)})
        payload = {"task_id": task_id, "result": result_str, "success": success, "error": error}
        try:
            signed_post(url, payload, timeout=3)
        except Exception:
            pass

    def post_progress(pct: int, log: str = ""):
        try:
            signed_post(
                f"{gateway}/api/workers/{worker_id}/progress",
                {"task_id": task_id, "progress": pct, "log": log},
                timeout=1,
            )
        except Exception:
            pass

    class SubprocessReporter:
        def progress(self, pct, log=""):
            # Cooperative pause: every progress report is a natural checkpoint.
            # Block here while pause_event is set so the dashboard sees the
            # task "frozen" between updates.
            if pause_event is not None:
                wait_if_paused(pause_event, cancel_event)
            post_progress(int(pct), log or "")

    try:
        # pass pause_event as a keyword-ish last arg; tasks.py signature is
        # (task_id, payload, reporter, cancel_event, pause_event=None) — None
        # default keeps backward compat with old task definitions.
        result = fn(task_id, payload, SubprocessReporter(), cancel_event, pause_event) or {}
        # Final pause check before posting: a task that finishes while paused
        # shouldn't get to "done" without the operator choosing to resume.
        if pause_event is not None:
            wait_if_paused(pause_event, cancel_event)
        if cancel_event.is_set():
            post_result(False, error="cancelled")
        else:
            post_result(True, result=result)
    except TaskCancelled as e:
        post_result(False, error=f"cancelled: {e}")
    except Exception as e:
        tb = _traceback.format_exc()
        print(f"[subprocess {task_id}] exception:\n{tb}", flush=True)
        try:
            post_result(False, error=str(e))
        except Exception:
            pass


class BaseWorker:
    """Common FastAPI scaffolding + gateway lifecycle for all worker types."""

    def __init__(self, worker_type: str, gateway: str, port: int,
                 name: Optional[str] = None, dispatch: Optional[dict] = None):
        self.worker_type = worker_type
        # Support comma-separated GATEWAY list for failover. First entry is primary.
        self.gateways = [g.strip().rstrip("/") for g in gateway.split(",") if g.strip()]
        if not self.gateways:
            self.gateways = ["http://127.0.0.1:13000"]
        self.gateway = self.gateways[0]
        self.gateway_idx = 0  # current primary
        self.port = port
        self.name = name or f"{worker_type}-node-{uuid.uuid4().hex[:6]}"
        self.worker_id = _stable_worker_id(port, worker_type)
        self.app = FastAPI(title=f"deskgrid-cloud-{worker_type}-worker")
        self.dispatch = dispatch or {}
        self.current_task_id: Optional[str] = None
        self.queue_len = 0
        self._lock = threading.Lock()
        self._cancel_event = None  # mp.Event shared with subprocess
        self._pause_event: Optional[multiprocessing.Event] = None  # mp.Event for cooperative pause
        self._current_proc: Optional[multiprocessing.Process] = None
        self._watchdog: Optional[threading.Timer] = None  # timeout watchdog (cancellable on pause)
        self._task_started_monotonic: Optional[float] = None  # wall time at task start
        self._tasks_completed = 0
        self._restart_requested = threading.Event()

        # Use "spawn" so subprocess crashes don't take down the parent (fork
        # is unsafe with FastAPI's threadlocals).
        self._mp_ctx = multiprocessing.get_context("spawn")

        self._register_routes()
        self._verify_hmac_middleware()

    # ---------- HMAC middleware ----------
    def _verify_hmac_middleware(self):
        """Reject any request whose HMAC signature doesn't match.

        Skipped when HMAC_SECRET is empty (backward-compatible with dev).
        Health endpoint stays open so healthchecks keep working.
        """
        if not HMAC_SECRET:
            return  # dev mode
        secret = HMAC_SECRET

        @self.app.middleware("http")
        async def _hmac_check(request: Request, call_next):
            # Allow unauthenticated probes so monitor-side polling still works
            if request.url.path in ("/health",):
                return await call_next(request)
            # Body must be read for signing — FastAPI caches it so handlers can re-read
            body = await request.body()
            sig = request.headers.get("X-Signature", "")
            ts = request.headers.get("X-Timestamp", "")
            if not _verify_request(body, sig, ts):
                return JSONResponse(
                    {"error": "invalid or missing HMAC signature"},
                    status_code=401,
                )
            return await call_next(request)

    # ---------- FastAPI routes ----------
    def _register_routes(self):
        @self.app.get("/health")
        def health():
            return {"ok": True, "type": self.worker_type, "id": self.worker_id,
                    "queue_len": self.queue_len, "current_task": self.current_task_id,
                    "tasks_completed": self._tasks_completed,
                    "pid": self._current_proc.pid if self._current_proc else None}

        @self.app.post("/task")
        def handle_task(req: dict):
            task_id = req.get("task_id")
            task_type = req.get("type")
            payload = req.get("payload", "")
            if not task_id or not task_type:
                raise HTTPException(400, "task_id and type required")
            fn = self.dispatch.get(task_type)
            if fn is None:
                raise HTTPException(400, f"unsupported task type: {task_type}")
            threading.Thread(target=self._execute, args=(task_id, task_type, payload, fn),
                             daemon=True).start()
            return {"ok": True, "task_id": task_id}

        @self.app.post("/task/cancel")
        def handle_cancel(req: dict):
            task_id = req.get("task_id")
            if not task_id:
                raise HTTPException(400, "task_id required")
            if task_id != self.current_task_id:
                raise HTTPException(409, f"task {task_id} not running on this worker")
            log.info("cancel requested for task %s", task_id)
            if self._cancel_event is not None:
                self._cancel_event.set()
            return {"ok": True, "task_id": task_id}

        @self.app.post("/task/pause")
        def handle_pause(req: dict):
            task_id = req.get("task_id")
            if not task_id:
                raise HTTPException(400, "task_id required")
            if task_id != self.current_task_id:
                raise HTTPException(409, f"task {task_id} not running on this worker")
            log.info("pause requested for task %s", task_id)
            with self._lock:
                if self._pause_event is not None:
                    self._pause_event.set()
                if self._watchdog is not None:
                    self._watchdog.cancel()
                    self._watchdog = None
            return {"ok": True, "task_id": task_id}

        @self.app.post("/task/resume")
        def handle_resume(req: dict):
            task_id = req.get("task_id")
            if not task_id:
                raise HTTPException(400, "task_id required")
            if task_id != self.current_task_id:
                raise HTTPException(409, f"task {task_id} not running on this worker")
            log.info("resume requested for task %s", task_id)
            with self._lock:
                if self._pause_event is not None:
                    self._pause_event.clear()
                # Restart the watchdog with whatever time was left on the
                # original budget so a paused-then-resumed task still gets
                # DEFAULT_TIMEOUT_DEFAULT total wall-clock to finish.
                if self._task_started_monotonic is not None and self._watchdog is None:
                    elapsed = time.monotonic() - self._task_started_monotonic
                    remaining = max(1, int(DEFAULT_TIMEOUT_DEFAULT - elapsed))
                    self._watchdog = threading.Timer(remaining, self._fire_watchdog)
                    self._watchdog.daemon = True
                    self._watchdog.start()
            return {"ok": True, "task_id": task_id}

        @self.app.post("/task/stop")
        def handle_stop(req: dict):
            task_id = req.get("task_id")
            if not task_id:
                raise HTTPException(400, "task_id required")
            if task_id != self.current_task_id:
                raise HTTPException(409, f"task {task_id} not running on this worker")
            log.warning("stop requested for task %s — terminating subprocess", task_id)
            # Cooperative signal first (lets a well-behaved task exit cleanly),
            # then forcibly terminate the subprocess so we don't wait for it.
            with self._lock:
                if self._pause_event is not None:
                    self._pause_event.clear()
                if self._cancel_event is not None:
                    self._cancel_event.set()
            self._fire_watchdog()
            return {"ok": True, "task_id": task_id}

        @self.app.post("/admin/restart")
        def admin_restart():
            self._restart_requested.set()
            return {"ok": True}

    # ---------- Execution ----------
    def _fire_watchdog(self):
        """Force-terminate the current subprocess. Called by the timeout
        watchdog OR by an operator-initiated stop. Idempotent."""
        with self._lock:
            proc = self._current_proc
        if proc is None:
            return
        try:
            if proc.is_alive():
                log.warning("watchdog/stop firing: terminating subprocess pid=%d", proc.pid)
                proc.terminate()
                proc.join(timeout=3)
                if proc.is_alive():
                    proc.kill()
                    proc.join()
        except Exception as e:
            log.error("watchdog error: %s", e)

    def _execute(self, task_id: str, task_type: str, payload, fn: Callable):
        cancel_event = self._mp_ctx.Event()
        pause_event = self._mp_ctx.Event()
        with self._lock:
            self.current_task_id = task_id
            self._cancel_event = cancel_event
            self._pause_event = pause_event
            self._task_started_monotonic = time.monotonic()

        # Watchdog timer: hard-kill the subprocess after DEFAULT_TIMEOUT_DEFAULT
        # seconds if the task is still running. Because the subprocess runs in
        # its own process, terminate() really does kill it (no GIL, no shared
        # state to corrupt). Stored on self so /task/pause can cancel it.
        with self._lock:
            self._watchdog = threading.Timer(DEFAULT_TIMEOUT_DEFAULT, self._fire_watchdog)
            self._watchdog.daemon = True
            self._watchdog.start()

        # Locate the task function by module+name so the subprocess can import
        # and call it (multiprocessing can't pickle local functions).
        fn_module = getattr(fn, "__module__", "builtins")
        fn_name = getattr(fn, "__name__", repr(fn))
        fn_info = f'{{"module": "{fn_module}", "name": "{fn_name}"}}'.encode()

        proc = self._mp_ctx.Process(
            target=_task_subprocess_entry,
            args=(self.gateway, self.worker_id, task_id, task_type, payload, fn_info,
                  cancel_event, pause_event),
            daemon=True,
        )
        with self._lock:
            self._current_proc = proc
        proc.start()
        log.info("task %s started in subprocess pid=%d", task_id, proc.pid)

        try:
            # Wait for subprocess to finish; timeout slightly longer than the
            # watchdog so we can read the watchdog's kill if it fired.
            proc.join(timeout=DEFAULT_TIMEOUT_DEFAULT + 10)

            if proc.is_alive():
                # watchdog didn't fire yet but we're past our own timeout
                log.warning("task %s still alive after %ds, force-killing", task_id, DEFAULT_TIMEOUT_DEFAULT + 10)
                proc.terminate()
                proc.join(timeout=3)
                if proc.is_alive():
                    proc.kill()
                    proc.join()
                # subprocess may not have posted a result; the gateway will
                # sweep it via the timeout watchdog.
            elif proc.exitcode != 0 and proc.exitcode is not None:
                log.warning("task %s subprocess exitcode=%s", task_id, proc.exitcode)
        finally:
            with self._lock:
                if self._watchdog is not None:
                    self._watchdog.cancel()
                    self._watchdog = None
                if self.current_task_id == task_id:
                    self.current_task_id = None
                    self._cancel_event = None
                    self._pause_event = None
                    self._current_proc = None
                    self._task_started_monotonic = None
                self._tasks_completed += 1
            if self._tasks_completed >= DEFAULT_RESTART_AFTER_TASKS:
                log.warning("reached %d tasks, requesting self-restart",
                    self._tasks_completed)
                self._restart_requested.set()

    # ---------- Gateway lifecycle ----------
    def register(self):
        hw = collect_hw()
        caps = collect_caps(self.worker_type)
        _, _, gpu_detail = _read_gpu_info()
        body = {
            "id": self.worker_id,
            "type": self.worker_type,
            "name": self.name,
            "cpu": hw["cpu"],
            "cores": hw["cores"],
            "memory_mb": hw["memory_mb"],
            "port": self.port,
            "capabilities": caps,
            # GPU info reported once at register, repeated each heartbeat so
            # the gateway knows what hardware it's talking to without
            # needing to learn it from the heartbeat stream.
            "gpus": gpu_detail,
            "gpu_count": len(gpu_detail),
        }
        last_err = None
        for attempt in range(10):
            url = f"{self.gateway}/api/workers/register"
            try:
                r = signed_post(url, body, timeout=3)
                if r.ok:
                    log.info("registered to gateway %s: id=%s name=%s cpu=%s",
                             self.gateway, self.worker_id, self.name, hw["cpu"])
                    return
                last_err = f"status={r.status_code} body={r.text[:200]}"
            except Exception as e:
                last_err = str(e)
            log.warning("register attempt %d via %s failed: %s", attempt + 1, self.gateway, last_err)
            # On failure, try next gateway (failover)
            if len(self.gateways) > 1 and attempt % 3 == 2:
                self.gateway_idx = (self.gateway_idx + 1) % len(self.gateways)
                self.gateway = self.gateways[self.gateway_idx]
                log.info("failover: trying gateway %s", self.gateway)
            time.sleep(1.5)
        raise RuntimeError(f"failed to register after retries: {last_err}")

    def start_heartbeat(self):
        def loop():
            interval = DEFAULT_HEARTBEAT_INTERVAL
            while True:
                if self._restart_requested.is_set():
                    log.warning("self-restart triggered, exiting so supervisor can respawn")
                    os._exit(0)
                try:
                    usage = snapshot_usage()
                    gpu_used, gpu_total, gpu_detail = _read_gpu_info()
                    body = {
                        "cpu_usage": usage["cpu_usage"],
                        "mem_usage": usage["mem_usage"],
                        "queue_len": self.queue_len,
                        "gpu_mem_used_mb": gpu_used,
                        "gpu_mem_total_mb": gpu_total,
                        "gpus": gpu_detail,
                        "gpu_count": len(gpu_detail),
                    }
                    r = signed_post(
                        f"{self.gateway}/api/workers/{self.worker_id}/heartbeat",
                        body, timeout=2)
                    if r.status_code == 404:
                        log.warning("heartbeat 404, re-registering...")
                        self.register()
                    elif r.status_code in (502, 503, 504) and len(self.gateways) > 1:
                        # gateway is down, try next
                        self.gateway_idx = (self.gateway_idx + 1) % len(self.gateways)
                        self.gateway = self.gateways[self.gateway_idx]
                        log.warning("failover to gateway %s", self.gateway)
                        self.register()
                except Exception as e:
                    log.debug("heartbeat failed: %s", e)
                time.sleep(interval)

        t = threading.Thread(target=loop, daemon=True)
        t.start()
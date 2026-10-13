"""BaseWorker - common scaffolding for code/doc/csv/image/audio/video workers.

Includes:
- Stable worker ID: WORKER_ID env > hostname > MAC hash > random UUID
- Faster heartbeat (3s) with GPU usage reporting
- Cooperative cancellation: /task/cancel closes a channel the executor polls
- Periodic self-restart after N tasks (prevents Python memory leaks)
- Real /hardware/fd stability tracking via the gateway
"""
import logging
import os
import platform
import socket
import subprocess
import threading
import time
import uuid
from typing import Callable, Optional

import psutil
import requests
from fastapi import FastAPI, HTTPException

from .hardware import collect as collect_hw, snapshot_usage
from .reporter import Reporter, post_result

log = logging.getLogger("worker")

# Default limits - override via env if needed
DEFAULT_HEARTBEAT_INTERVAL = float(os.environ.get("WORKER_HEARTBEAT_INTERVAL", "3"))
DEFAULT_RESTART_AFTER_TASKS = int(os.environ.get("WORKER_RESTART_AFTER_TASKS", "500"))
DEFAULT_TIMEOUT_DEFAULT = int(os.environ.get("WORKER_TASK_TIMEOUT", "300"))


class TaskCancelled(Exception):
    """Raised inside a task function when cancellation has been requested.

    The executor catches this and reports the task as failed with
    error='cancelled' so the gateway can decide whether to retry or give up.
    """
    pass


def cancellable_sleep(seconds: float, cancel_event: Optional[threading.Event],
                      step: float = 0.5) -> None:
    """Sleep that wakes up early when cancellation is requested.

    Splits the sleep into 0.5s slices and exits as soon as cancel_event is set.
    Use this in place of time.sleep in long-running loops so a task can be
    killed in <1s instead of waiting for the full sleep to complete.
    """
    if cancel_event is None or seconds <= 0:
        return
    end = time.time() + seconds
    while time.time() < end:
        if cancel_event.is_set():
            raise TaskCancelled("cancelled during sleep")
        time.sleep(min(step, end - time.time()))


def check_cancel(cancel_event: Optional[threading.Event], label: str = "") -> None:
    """Raise TaskCancelled immediately if cancellation has been requested.

    Place at the top of each loop iteration in user task functions to ensure
    timely cancellation. Cheap to call when cancel_event is None (no-op).
    """
    if cancel_event is not None and cancel_event.is_set():
        raise TaskCancelled(f"cancelled{' at ' + label if label else ''}")


def _stable_worker_id(port: int, worker_type: str) -> str:
    """Resolve a stable worker ID.

    Priority:
      1. WORKER_ID env var (explicit override, e.g. for fixed-name workers)
      2. hostname + worker_type + port (stable, distinguishes same-host workers)
      3. MAC address hash (survives hostname changes)
      4. random UUID (last resort - identity won't survive restart)

    Including type+port is critical: multiple workers on the same host must
    have distinct IDs or they'll keep clobbering each other in the store.
    """
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


def _read_gpu_info() -> tuple[int, int]:
    """Best-effort GPU memory read. Returns (used_mb, total_mb).

    Tries pynvml (nvidia-smi python lib). Returns (0, 0) when no GPU or lib
    unavailable, which tells the gateway there's no GPU to track.
    """
    try:
        import pynvml  # type: ignore
        pynvml.nvmlInit()
        handle = pynvml.nvmlDeviceGetHandleByIndex(0)
        mem = pynvml.nvmlDeviceGetMemoryInfo(handle)
        return (int(mem.used // (1024 * 1024)), int(mem.total // (1024 * 1024)))
    except Exception:
        return (0, 0)


class BaseWorker:
    """Common FastAPI scaffolding + gateway lifecycle for all worker types."""

    def __init__(self, worker_type: str, gateway: str, port: int,
                 name: Optional[str] = None, dispatch: Optional[dict] = None):
        self.worker_type = worker_type
        self.gateway = gateway.rstrip("/")
        self.port = port
        self.name = name or f"{worker_type}-node-{uuid.uuid4().hex[:6]}"
        self.worker_id = _stable_worker_id(port, worker_type)
        self.app = FastAPI(title=f"deskgrid-cloud-{worker_type}-worker")
        self.dispatch = dispatch or {}
        self.current_task_id: Optional[str] = None
        self.queue_len = 0
        self._lock = threading.Lock()
        self._reporter: Optional[Reporter] = None
        self._tasks_completed = 0
        self._cancel_event: Optional[threading.Event] = None
        self._restart_requested = threading.Event()

        self._register_routes()

    # ---------- FastAPI routes ----------
    def _register_routes(self):
        @self.app.get("/health")
        def health():
            return {"ok": True, "type": self.worker_type, "id": self.worker_id,
                    "queue_len": self.queue_len, "current_task": self.current_task_id,
                    "tasks_completed": self._tasks_completed}

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

        @self.app.post("/admin/restart")
        def admin_restart():
            """Used by operator/test scripts to request a graceful restart."""
            self._restart_requested.set()
            return {"ok": True}

    # ---------- Execution ----------
    def _execute(self, task_id: str, task_type: str, payload, fn: Callable):
        cancel_event = threading.Event()
        with self._lock:
            self.current_task_id = task_id
            self._cancel_event = cancel_event
        reporter = Reporter(self.gateway, self.worker_id, lambda: self.current_task_id)
        reporter.start()

        # Schedule watchdog: after DEFAULT_TIMEOUT_DEFAULT seconds, force the
        # cancel_event so cooperative task functions have a chance to abort.
        # Tasks must call check_cancel() or cancellable_sleep() to honor this.
        watchdog = threading.Timer(DEFAULT_TIMEOUT_DEFAULT, cancel_event.set)
        watchdog.daemon = True
        watchdog.start()

        start_ts = time.time()
        try:
            reporter.progress(1, f"[{task_type}] start")
            result = fn(task_id, payload, reporter, cancel_event) or {}
            elapsed = time.time() - start_ts
            if cancel_event.is_set():
                if elapsed >= DEFAULT_TIMEOUT_DEFAULT * 0.95:
                    # watchdog fired and task honored it
                    log.warning("task %s aborted by watchdog after %.1fs", task_id, elapsed)
                    reporter.progress(100, f"[{task_type}] cancelled by watchdog")
                    post_result(self.gateway, self.worker_id, task_id, {},
                                success=False, error="task timed out (watchdog fired)")
                else:
                    # gateway-side cancel arrived
                    log.info("task %s cancelled by gateway after %.1fs", task_id, elapsed)
                    reporter.progress(100, f"[{task_type}] cancelled")
                    post_result(self.gateway, self.worker_id, task_id, {},
                                success=False, error="cancelled by gateway")
            else:
                reporter.progress(100, f"[{task_type}] done")
                post_result(self.gateway, self.worker_id, task_id, result, success=True)
        except TaskCancelled as e:
            log.info("task %s cancelled: %s", task_id, e)
            reporter.progress(100, f"[{task_type}] cancelled: {e}")
            post_result(self.gateway, self.worker_id, task_id, {},
                        success=False, error=f"cancelled: {e}")
        except Exception as e:
            log.exception("task %s failed", task_id)
            reporter.progress(100, f"[{task_type}] failed: {e}")
            post_result(self.gateway, self.worker_id, task_id, {}, success=False, error=str(e))
        finally:
            watchdog.cancel()
            reporter.stop()
            with self._lock:
                if self.current_task_id == task_id:
                    self.current_task_id = None
                    self._cancel_event = None
                self._tasks_completed += 1
            # self-restart after N tasks to free accumulated Python memory
            if self._tasks_completed >= DEFAULT_RESTART_AFTER_TASKS:
                log.warning("reached %d tasks, requesting self-restart",
                    self._tasks_completed)
                self._restart_requested.set()

    # ---------- Gateway lifecycle ----------
    def register(self):
        hw = collect_hw()
        url = f"{self.gateway}/api/workers/register"
        body = {
            "id": self.worker_id,
            "type": self.worker_type,
            "name": self.name,
            "cpu": hw["cpu"],
            "cores": hw["cores"],
            "memory_mb": hw["memory_mb"],
            "port": self.port,
        }
        last_err = None
        for attempt in range(10):
            try:
                r = requests.post(url, json=body, timeout=3)
                if r.ok:
                    log.info("registered to gateway: id=%s name=%s cpu=%s",
                             self.worker_id, self.name, hw["cpu"])
                    return
                last_err = f"status={r.status_code} body={r.text[:200]}"
            except Exception as e:
                last_err = str(e)
            log.warning("register attempt %d failed: %s", attempt + 1, last_err)
            time.sleep(1.5)
        raise RuntimeError(f"failed to register after retries: {last_err}")

    def start_heartbeat(self):
        def loop():
            interval = DEFAULT_HEARTBEAT_INTERVAL
            while True:
                    # check for self-restart first
                    if self._restart_requested.is_set():
                        log.warning("self-restart triggered, exiting so supervisor can respawn")
                        os._exit(0)
                    try:
                        usage = snapshot_usage()
                        gpu_used, gpu_total = _read_gpu_info()
                        body = {
                            "cpu_usage": usage["cpu_usage"],
                            "mem_usage": usage["mem_usage"],
                            "queue_len": self.queue_len,
                            "gpu_mem_used_mb": gpu_used,
                            "gpu_mem_total_mb": gpu_total,
                        }
                        r = requests.post(
                            f"{self.gateway}/api/workers/{self.worker_id}/heartbeat",
                            json=body, timeout=2)
                        if r.status_code == 404:
                            log.warning("heartbeat 404, re-registering...")
                            self.register()
                    except Exception as e:
                        log.debug("heartbeat failed: %s", e)
                    time.sleep(interval)

        t = threading.Thread(target=loop, daemon=True)
        t.start()
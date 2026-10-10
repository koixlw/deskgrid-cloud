"""BaseWorker — common scaffolding for code/doc/csv workers."""
import logging
import threading
import time
import uuid
from typing import Callable, Optional

import requests
from fastapi import FastAPI, HTTPException

from .hardware import collect as collect_hw, snapshot_usage
from .reporter import Reporter, post_result

log = logging.getLogger("worker")


class BaseWorker:
    """Common FastAPI scaffolding + gateway lifecycle for all worker types."""

    def __init__(self, worker_type: str, gateway: str, port: int,
                 name: Optional[str] = None, dispatch: Optional[dict] = None):
        self.worker_type = worker_type
        self.gateway = gateway.rstrip("/")
        self.port = port
        self.name = name or f"{worker_type}-node-{uuid.uuid4().hex[:6]}"
        self.worker_id = uuid.uuid4().hex
        self.app = FastAPI(title=f"deskgrid-cloud-{worker_type}-worker")
        self.dispatch = dispatch or {}  # task_type -> callable(task_id, payload, reporter) -> result dict
        self.current_task_id: Optional[str] = None
        self.queue_len = 0
        self._lock = threading.Lock()
        self._reporter: Optional[Reporter] = None

        self._register_routes()

    # ---------- FastAPI routes ----------
    def _register_routes(self):
        @self.app.get("/health")
        def health():
            return {"ok": True, "type": self.worker_type, "id": self.worker_id,
                    "queue_len": self.queue_len, "current_task": self.current_task_id}

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
            # execute in background; immediately ack the gateway push
            threading.Thread(target=self._execute, args=(task_id, task_type, payload, fn),
                             daemon=True).start()
            return {"ok": True, "task_id": task_id}

    # ---------- Execution ----------
    def _execute(self, task_id: str, task_type: str, payload, fn: Callable):
        with self._lock:
            self.current_task_id = task_id
        reporter = Reporter(self.gateway, self.worker_id, lambda: self.current_task_id)
        reporter.start()
        try:
            reporter.progress(1, f"[{task_type}] 启动")
            result = fn(task_id, payload, reporter) or {}
            reporter.progress(100, f"[{task_type}] 完成")
            post_result(self.gateway, self.worker_id, task_id, result, success=True)
        except Exception as e:
            log.exception("task %s failed", task_id)
            reporter.progress(100, f"[{task_type}] 失败: {e}")
            post_result(self.gateway, self.worker_id, task_id, {}, success=False, error=str(e))
        finally:
            reporter.stop()
            with self._lock:
                if self.current_task_id == task_id:
                    self.current_task_id = None

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

    def start_heartbeat(self, interval: float = 5.0):
        def loop():
            while True:
                try:
                    usage = snapshot_usage()
                    body = {
                        "cpu_usage": usage["cpu_usage"],
                        "mem_usage": usage["mem_usage"],
                        "queue_len": self.queue_len,
                    }
                    r = requests.post(f"{self.gateway}/api/workers/{self.worker_id}/heartbeat",
                                      json=body, timeout=2)
                    if r.status_code == 404:
                        log.warning("heartbeat 404, re-registering...")
                        self.register()
                except Exception as e:
                    log.debug("heartbeat failed: %s", e)
                time.sleep(interval)

        t = threading.Thread(target=loop, daemon=True)
        t.start()

"""Progress reporter: posts progress updates to the gateway."""
import json
import logging
import threading
import time
from typing import Callable, Optional

import requests

log = logging.getLogger("worker.reporter")


class Reporter:
    """Reports progress to the gateway in a thread-safe manner.

    Designed to be used by background task executors. The caller invokes
    `progress(percent, log)` from any thread; the reporter batches updates
    and POSTs them to /api/workers/{id}/progress.
    """

    def __init__(self, gateway: str, worker_id: str, task_id_getter: Callable[[], Optional[str]],
                 log_flush_interval: float = 0.3):
        self.gateway = gateway.rstrip("/")
        self.worker_id = worker_id
        self._task_id_getter = task_id_getter
        self._flush_interval = log_flush_interval
        self._lock = threading.Lock()
        self._latest_progress = -1
        self._pending_logs: list[str] = []
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def start(self):
        self._thread = threading.Thread(target=self._loop, daemon=True)
        self._thread.start()

    def stop(self):
        self._stop.set()
        self._flush()

    def progress(self, percent: int, log: Optional[str] = None):
        with self._lock:
            self._latest_progress = max(self._latest_progress, int(percent))
            if log:
                self._pending_logs.append(log)

    def _loop(self):
        while not self._stop.is_set():
            self._stop.wait(self._flush_interval)
            self._flush()

    def _flush(self):
        with self._lock:
            if self._latest_progress < 0 and not self._pending_logs:
                return
            progress = self._latest_progress
            logs = self._pending_logs[:]
            self._pending_logs.clear()
        task_id = self._task_id_getter()
        if not task_id:
            return
        url = f"{self.gateway}/api/workers/{self.worker_id}/progress"
        for log in logs:
            try:
                requests.post(url, json={
                    "task_id": task_id,
                    "progress": progress,
                    "log": log,
                }, timeout=2)
            except Exception:
                pass
        if not logs and progress >= 0:
            try:
                requests.post(url, json={
                    "task_id": task_id,
                    "progress": progress,
                    "log": "",
                }, timeout=2)
            except Exception:
                pass


def post_result(gateway: str, worker_id: str, task_id: str, result: dict, success: bool = True,
                error: str = "") -> None:
    url = f"{gateway.rstrip('/')}/api/workers/{worker_id}/result"
    payload = {
        "task_id": task_id,
        "result": _safe_json_dumps(result),
        "success": success,
        "error": error,
    }
    # DEBUG: log what we're actually sending
    try:
        body = json.dumps(payload, ensure_ascii=False)
        log.debug("post_result task=%s body_len=%d", task_id, len(body))
    except Exception as e:
        log.warning("debug log failed: %s", e)
    for attempt in range(3):
        try:
            r = requests.post(url, json=payload, timeout=5)
            if r.ok:
                return
        except Exception:
            time.sleep(0.3 * (attempt + 1))


def _safe_json_dumps(obj) -> str:
    import json
    try:
        return json.dumps(obj, ensure_ascii=False)
    except Exception:
        return json.dumps({"raw": str(obj)}, ensure_ascii=False)

"""Exec worker entry point (port 3007).

Runs `exec_run` tasks: arbitrary shell commands submitted by trusted
clients (deskgrid-submit). The submission API enforces cwd + env
allowlists (see workers/exec/tasks.py), but operators are expected to
restrict who can talk to the gateway — by HMAC + token + LAN ACLs —
because exec workers run real commands.
"""
import logging
import os
WORKER_HOST = os.environ.get("WORKER_HOST", "127.0.0.1")
import sys

import uvicorn

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import BaseWorker  # noqa: E402
from tasks import exec_cmd  # noqa: E402

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
)

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:13000")
PORT = int(os.environ.get("PORT", "3007"))
WORKER_NAME = os.environ.get("WORKER_NAME", "exec-node-01")

worker = BaseWorker(
    worker_type="exec",
    gateway=GATEWAY,
    port=PORT,
    name=WORKER_NAME,
    dispatch={
        "exec_run": exec_cmd,
    },
)

worker.register()
worker.start_heartbeat()

if __name__ == "__main__":
    uvicorn.run(worker.app, host=os.environ.get("WORKER_HOST", "127.0.0.1"), port=PORT, log_level="warning")

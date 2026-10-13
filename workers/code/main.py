"""Code worker entry point (port 3001)."""
import logging
import os
WORKER_HOST = os.environ.get("WORKER_HOST", "127.0.0.1")
import sys

import uvicorn

# allow `from common.base import ...` when running this file directly
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from common.base import BaseWorker  # noqa: E402
from tasks import format_code, count_ast  # noqa: E402

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
)

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:13000")
PORT = int(os.environ.get("PORT", "3001"))
WORKER_NAME = os.environ.get("WORKER_NAME", "code-node-01")

worker = BaseWorker(
    worker_type="code",
    gateway=GATEWAY,
    port=PORT,
    name=WORKER_NAME,
    dispatch={
        "code_format": format_code,
        "code_ast": count_ast,
    },
)

# Register immediately on startup (retry inside if gateway not ready)
worker.register()
worker.start_heartbeat()

if __name__ == "__main__":
    uvicorn.run(worker.app, host=os.environ.get("WORKER_HOST", "127.0.0.1"), port=PORT, log_level="warning")

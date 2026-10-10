"""Doc worker entry point (port 3002)."""
import logging
import os
import sys

import uvicorn

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from common.base import BaseWorker  # noqa: E402
from tasks import summarize, wordfreq  # noqa: E402

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
)

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:13000")
PORT = int(os.environ.get("PORT", "3002"))
WORKER_NAME = os.environ.get("WORKER_NAME", "doc-node-01")

worker = BaseWorker(
    worker_type="doc",
    gateway=GATEWAY,
    port=PORT,
    name=WORKER_NAME,
    dispatch={
        "doc_summary": summarize,
        "doc_wordfreq": wordfreq,
    },
)

worker.register()
worker.start_heartbeat()

if __name__ == "__main__":
    uvicorn.run(worker.app, host="0.0.0.0", port=PORT, log_level="warning")

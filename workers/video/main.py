"""Video worker entry point (port 3006)."""
import logging
import os
WORKER_HOST = os.environ.get("WORKER_HOST", "127.0.0.1")
import sys

import uvicorn

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import BaseWorker  # noqa: E402
from tasks import assemble_clip, add_subtitle, kenburns  # noqa: E402

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
)

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:13000")
PORT = int(os.environ.get("PORT", "3006"))
WORKER_NAME = os.environ.get("WORKER_NAME", "video-node-01")

worker = BaseWorker(
    worker_type="video",
    gateway=GATEWAY,
    port=PORT,
    name=WORKER_NAME,
    dispatch={
        "video_assemble_clip": assemble_clip,
        "video_add_subtitle": add_subtitle,
        "video_kenburns": kenburns,
    },
)

worker.register()
worker.start_heartbeat()

if __name__ == "__main__":
    uvicorn.run(worker.app, host=os.environ.get("WORKER_HOST", "127.0.0.1"), port=PORT, log_level="warning")
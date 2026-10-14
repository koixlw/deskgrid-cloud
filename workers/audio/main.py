"""Audio worker entry point (port 3005)."""
import logging
import os
WORKER_HOST = os.environ.get("WORKER_HOST", "127.0.0.1")
import sys

import uvicorn

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import BaseWorker  # noqa: E402
from tasks import tts_line, tts_with_emotion  # noqa: E402

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
)

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:13000")
PORT = int(os.environ.get("PORT", "3005"))
WORKER_NAME = os.environ.get("WORKER_NAME", "audio-node-01")

worker = BaseWorker(
    worker_type="audio",
    gateway=GATEWAY,
    port=PORT,
    name=WORKER_NAME,
    dispatch={
        "audio_tts_line": tts_line,
        "audio_tts_emotion": tts_with_emotion,
    },
)

worker.register()
worker.start_heartbeat()

if __name__ == "__main__":
    uvicorn.run(worker.app, host=os.environ.get("WORKER_HOST", "127.0.0.1"), port=PORT, log_level="warning")
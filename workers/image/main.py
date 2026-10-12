"""Image worker entry point (port 3004).

Drop-in replacement: replace tasks.py with diffusers-based implementations
once a GPU is available on this node. No other changes needed.
"""
import logging
import os
import sys

import uvicorn

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import BaseWorker  # noqa: E402
from tasks import txt2img, img2img, inpaint, controlnet  # noqa: E402

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
)

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:13000")
PORT = int(os.environ.get("PORT", "3004"))
WORKER_NAME = os.environ.get("WORKER_NAME", "image-node-01")

worker = BaseWorker(
    worker_type="image",
    gateway=GATEWAY,
    port=PORT,
    name=WORKER_NAME,
    dispatch={
        "image_txt2img": txt2img,
        "image_img2img": img2img,
        "image_inpaint": inpaint,
        "image_controlnet": controlnet,
    },
)

worker.register()
worker.start_heartbeat()

if __name__ == "__main__":
    uvicorn.run(worker.app, host="0.0.0.0", port=PORT, log_level="warning")
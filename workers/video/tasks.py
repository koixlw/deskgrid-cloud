"""Video worker tasks - Video assembly stubs using imageio + Pillow.

Real production code should use:
    - FFmpeg with filter graphs (subtitle, kenburns, transitions)
    - moviepy (Python wrapper around ffmpeg)
    - opencv-python (advanced compositing)

These implementations compose real MP4 files from generated frames,
using imageio-ffmpeg for H.264 encoding. Works without a GPU.
"""
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import TaskCancelled  # noqa: E402

import imageio.v2 as imageio
import numpy as np
from PIL import Image, ImageDraw, ImageFont

OUTPUT_DIR = Path(os.environ.get("VIDEO_OUTPUT_DIR", r"E:\deskgrid-cloud\artifacts"))
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)


def _font(size=24):
    for cand in ("arial.ttf", "C:/Windows/Fonts/arial.ttf", "C:/Windows/Fonts/msyh.ttc"):
        if os.path.exists(cand):
            return ImageFont.truetype(cand, size)
    return ImageFont.load_default()


def _frame(scene_idx, scene_text, t, total_t, w=640, h=360):
    """Persist a baked frame helper. One gradient frame with text overlay."""
    img = Image.new("RGB", (w, h))
    px = img.load()
    color_a = (int(40 + scene_idx * 30) % 256, 60, 130)
    color_b = (180, int(80 + scene_idx * 40) % 256, 70)
    for y in range(h):
        for x in range(w):
            r = int(color_a[0] + (color_b[0] - color_a[0]) * (y / h))
            g = int(color_a[1] + (color_b[1] - color_a[1]) * (y / h))
            b = int(color_a[2] + (color_b[2] - color_a[2]) * (y / h))
            px[x, y] = (r, g, b)
    draw = ImageDraw.Draw(img)
    if scene_text:
        label = f"Scene {scene_idx + 1}: {scene_text[:40]}"
        tw = draw.textlength(label, font=_font(24))
        draw.rectangle((20, 18, int(tw) + 40, 60), fill=(0, 0, 0))
        draw.text((30, 26), label, font=_font(24), fill="white")
    return np.array(img)


def _parse_payload(payload):
    if isinstance(payload, dict):
        return payload
    if isinstance(payload, str):
        s = payload.strip()
        if s.startswith("{"):
            try:
                return json.loads(s)
            except Exception:
                pass
        return {"scenes": [seg.strip() for seg in s.split("|") if seg.strip()]}
    return {}


def assemble_clip(task_id, payload, reporter, cancel_event=None):
    reporter.progress(10, "[clip] parse")
    p = _parse_payload(payload)
    if "scenes" not in p:
        p["scenes"] = ["opening", "rising action", "climax", "ending"]
    scenes = [str(s).strip()[:80] for s in p["scenes"] if str(s).strip()]
    fps = max(2, min(30, int(p.get("fps", 8))))
    sec = max(0.5, min(8.0, float(p.get("sec_per_scene", 1.5))))
    n_per = int(fps * sec)

    reporter.progress(20, f"[clip] {len(scenes)} scenes x {n_per} frames @ {fps}fps")
    frames = []
    for i, scene in enumerate(scenes):
        for f in range(n_per):
            if cancel_event and cancel_event.is_set():
                raise TaskCancelled(f"clip cancelled at scene {i+1} frame {f+1}/{n_per}")
            t = f / max(n_per - 1, 1)
            frames.append(_frame(i, scene, t, 1.0))
        reporter.progress(20 + 70 * (i + 1) // max(len(scenes), 1),
                          f"[clip] scene {i + 1}/{len(scenes)}")

    out = OUTPUT_DIR / f"clip_{task_id[:8]}.mp4"
    imageio.mimsave(str(out), frames, fps=fps, codec="libx264", quality=8)
    reporter.progress(100, "[clip] done")
    return {
        "video_path": str(out),
        "scenes": len(scenes),
        "frames": len(frames),
        "fps": fps,
        "duration_s": round(len(frames) / fps, 2),
    }


def add_subtitle(task_id, payload, reporter, cancel_event=None):
    """Persist a baked subtitle task. Reuses clip assembly - same artifact."""
    return assemble_clip(task_id, payload, reporter)


def kenburns(task_id, payload, reporter, cancel_event=None):
    reporter.progress(10, "[kenburns] parse")
    p = _parse_payload(payload)
    prompt = (p.get("prompt") or "kenburns zoom").strip()[:80]
    fps = max(2, min(30, int(p.get("fps", 12))))
    duration = max(0.5, min(8.0, float(p.get("duration", 2.0))))
    nframes = int(fps * duration)
    reporter.progress(25, f"[kenburns] {nframes} frames @ {fps}fps")

    src_w, src_h = 1024, 768
    base = Image.new("RGB", (src_w, src_h), (40, 60, 110))
    bd = ImageDraw.Draw(base)
    bd.rectangle((100, 100, src_w - 100, src_h - 100), outline=(200, 200, 255), width=6)
    bd.text((120, 120), prompt, font=_font(40), fill="white")

    frames = []
    for i in range(nframes):
        if cancel_event and cancel_event.is_set():
            raise TaskCancelled(f"kenburns cancelled at frame {i+1}/{nframes}")
        t = i / max(nframes - 1, 1)
        scale = 1.0 + 0.5 * t
        cw = int(src_w / scale)
        ch = int(src_h / scale)
        cx = int((src_w - cw) * 0.3 * t)
        cy = int((src_h - ch) * 0.2 * t)
        crop = base.crop((cx, cy, cx + cw, cy + ch))
        crop = crop.resize((640, 360))
        frames.append(np.array(crop))
        if i % 4 == 0:
            reporter.progress(25 + 70 * i // nframes, f"[kenburns] frame {i + 1}/{nframes}")

    out = OUTPUT_DIR / f"kenburns_{task_id[:8]}.mp4"
    imageio.mimsave(str(out), frames, fps=fps, codec="libx264", quality=8)
    reporter.progress(100, "[kenburns] done")
    return {
        "video_path": str(out),
        "frames": nframes,
        "fps": fps,
        "duration_s": round(nframes / fps, 2),
        "prompt": prompt,
    }
"""Image worker tasks - AI image generation stubs using Pillow.

Real production code should replace these with:
    - diffusers (txt2img / img2img / inpaint / controlnet)
    - comfyui-client (POST to running ComfyUI instance)
    - sd-webui API (Automatic1111 http endpoint)

These implementations produce real, viewable PNGs using gradients + text
overlays so the end-to-end pipeline (gateway -> worker -> artifact file)
works without requiring a GPU or large model download.
"""
import colorsys
import os
import time
from pathlib import Path

from PIL import Image, ImageDraw, ImageFilter, ImageFont

OUTPUT_DIR = Path(os.environ.get("IMAGE_OUTPUT_DIR", r"E:\deskgrid-cloud\artifacts"))
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)


def _hsv_to_rgb(h, s, v):
    r, g, b = colorsys.hsv_to_rgb((h % 360) / 360.0, s, v)
    return (int(r * 255), int(g * 255), int(b * 255))


def _gradient(size, c1, c2):
    img = Image.new("RGB", size, c1)
    px = img.load()
    w, h = size
    for y in range(h):
        t = y / max(h - 1, 1)
        r = int(c1[0] + (c2[0] - c1[0]) * t)
        g = int(c1[1] + (c2[1] - c1[1]) * t)
        b = int(c1[2] + (c2[2] - c1[2]) * t)
        for x in range(w):
            px[x, y] = (r, g, b)
    return img


def _font(size):
    for cand in ("arial.ttf", "C:/Windows/Fonts/arial.ttf", "C:/Windows/Fonts/msyh.ttc"):
        if os.path.exists(cand):
            return ImageFont.truetype(cand, size)
    return ImageFont.load_default()


def _annotate(img, text):
    """Persist a baked annotation. Burn the prompt text on the image."""
    w, h = img.size
    overlay_h = 80
    overlay = Image.new("RGBA", (w, overlay_h), (0, 0, 0, 170))
    img.paste(overlay, (0, h - overlay_h), overlay)
    draw = ImageDraw.Draw(img)
    big = _font(20)
    small = _font(12)
    # word-wrap manually
    words, lines, line = text.split(), [], ""
    for word in words[:30]:
        cand = (line + " " + word).strip()
        bbox = draw.textbbox((0, 0), cand, font=big)
        if bbox[2] - bbox[0] <= w - 40:
            line = cand
        else:
            if line:
                lines.append(line)
            line = word
    if line:
        lines.append(line)
    y = h - overlay_h + 8
    for ln in lines[:2]:
        draw.text((20, y), ln, font=big, fill="white")
        y += 26
    draw.text((20, h - 18), "deskgrid-cloud image worker (Pillow stub)", font=small, fill="#9bb")
    return img


def _parse_payload(payload):
    """Persist a baked parse helper. payload can be str or dict."""
    if isinstance(payload, dict):
        return payload
    if isinstance(payload, str):
        s = payload.strip()
        if s.startswith("{"):
            try:
                import json
                return json.loads(s)
            except Exception:
                pass
        return {"prompt": s}
    return {"prompt": str(payload)}


def txt2img(task_id, payload, reporter, cancel_event=None):
    reporter.progress(5, "[txt2img] parse prompt")
    p = _parse_payload(payload)
    prompt = (p.get("prompt") or "an orange cat").strip()[:400]
    width = int(p.get("width", 512))
    height = int(p.get("height", 512))
    steps = int(p.get("steps", 20))

    reporter.progress(15, f"[txt2img] prompt='{prompt[:60]}'")
    time.sleep(0.2)
    reporter.progress(30, "[txt2img] load model (stub)")
    time.sleep(0.3)
    reporter.progress(50, f"[txt2img] sample {steps} steps (stub)")

    hue = abs(hash(prompt)) % 360
    c1 = _hsv_to_rgb(hue, 0.6, 0.85)
    c2 = _hsv_to_rgb((hue + 70) % 360, 0.5, 0.55)
    img = _gradient((width, height), c1, c2)

    # fake "denoising" - add subtle radial vignette
    vignette = Image.new("L", (width, height), 0)
    vd = ImageDraw.Draw(vignette)
    cx, cy = width // 2, height // 2
    for r in range(min(width, height) // 2, 0, -2):
        v = int(255 * (r / (min(width, height) // 2)))
        vd.ellipse((cx - r, cy - r, cx + r, cy + r), fill=v)
    img = Image.composite(img, Image.new("RGB", (width, height), (20, 20, 20)), vignette)
    img = _annotate(img, prompt)
    reporter.progress(80, "[txt2img] save")

    out = OUTPUT_DIR / f"txt2img_{task_id[:8]}.png"
    img.save(out, "PNG", optimize=True)
    time.sleep(0.1)
    reporter.progress(100, "[txt2img] done")
    return {
        "image_path": str(out),
        "width": width, "height": height,
        "prompt": prompt, "steps": steps,
        "model": "pillow-stub-v1",
    }


def img2img(task_id, payload, reporter, cancel_event=None):
    reporter.progress(5, "[img2img] parse")
    p = _parse_payload(payload)
    strength = float(p.get("strength", 0.6))
    prompt = (p.get("prompt") or "variation").strip()[:200]
    src = None
    if p.get("image_path") and Path(p["image_path"]).exists():
        src = Image.open(p["image_path"]).convert("RGB")
    if src is None:
        src = _gradient((512, 512), (90, 90, 130), (210, 160, 110))
        reporter.progress(25, "[img2img] generated base (no input)")

    blur_r = max(1, int(strength * 12))
    src = src.filter(ImageFilter.GaussianBlur(blur_r))
    src = _annotate(src, f"{prompt}  (denoise={strength}, blur r={blur_r})")

    out = OUTPUT_DIR / f"img2img_{task_id[:8]}.png"
    src.save(out, "PNG")
    reporter.progress(100, "[img2img] done")
    return {
        "image_path": str(out),
        "strength": str(strength),
        "prompt": prompt,
        "model": "pillow-stub-v1",
    }


def inpaint(task_id, payload, reporter, cancel_event=None):
    reporter.progress(5, "[inpaint] parse")
    p = _parse_payload(payload)
    prompt = (p.get("prompt") or "replace this area").strip()[:200]
    src = None
    if p.get("image_path") and Path(p["image_path"]).exists():
        src = Image.open(p["image_path"]).convert("RGB")
    if src is None:
        src = _gradient((512, 512), (60, 60, 60), (200, 200, 200))

    draw = ImageDraw.Draw(src)
    w, h = src.size
    box = (w // 4, h // 4, 3 * w // 4, 3 * h // 4)
    draw.rectangle(box, fill=(220, 50, 50))
    draw.text((box[0] + 10, box[1] + 10), "[INPAINT MASK]", fill="white", font=_font(18))
    src = _annotate(src, prompt + "  | red box = mask")

    out = OUTPUT_DIR / f"inpaint_{task_id[:8]}.png"
    src.save(out, "PNG")
    reporter.progress(100, "[inpaint] done")
    return {
        "image_path": str(out),
        "mask_box": list(box),
        "prompt": prompt,
        "model": "pillow-stub-v1",
    }


def controlnet(task_id, payload, reporter, cancel_event=None):
    reporter.progress(5, "[controlnet] parse")
    p = _parse_payload(payload)
    prompt = (p.get("prompt") or "controlnet hint").strip()[:200]
    src = None
    if p.get("image_path") and Path(p["image_path"]).exists():
        src = Image.open(p["image_path"]).convert("L")
    if src is None:
        src = _gradient((512, 512), (40, 40, 40), (220, 220, 220)).convert("L")

    edges = src.filter(ImageFilter.FIND_EDGES).convert("RGB")
    edges = _annotate(edges, prompt + "  | edge map (Canny-like)")

    out = OUTPUT_DIR / f"controlnet_{task_id[:8]}.png"
    edges.save(out, "PNG")
    reporter.progress(100, "[controlnet] done")
    return {
        "image_path": str(out),
        "prompt": prompt,
        "model": "pillow-stub-v1",
    }
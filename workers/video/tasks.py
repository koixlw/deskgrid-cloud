"""Video worker tasks - real video assembly via ffmpeg.

We resolve the ffmpeg binary once at import:
  1. `ffmpeg` in PATH (most common after `choco install ffmpeg` etc.)
  2. `imageio_ffmpeg` bundled binary (always present, ships with the worker)

For each task we generate PNG frames with Pillow, then hand them to
ffmpeg which produces a real H.264 mp4. Frames are piped via stdin
(`-f image2pipe -i -`) so we don't write intermediates to disk.

Tasks:
- assemble_clip  : scene list -> slideshow mp4 (each scene = 1 still)
- add_subtitle   : scene list with per-scene subtitles burned on frames
- kenburns       : single image + zoom/pan -> cinematic mp4
"""
import io
import json
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import TaskCancelled, wait_if_paused  # noqa: E402

from PIL import Image, ImageDraw, ImageFont

OUTPUT_DIR = Path(os.environ.get("VIDEO_OUTPUT_DIR", r"E:\deskgrid-cloud\artifacts"))
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

# ---------- ffmpeg resolution ----------
def _resolve_ffmpeg() -> str:
    """Find an ffmpeg binary: PATH first, then imageio_ffmpeg's bundled one."""
    from shutil import which
    p = which("ffmpeg")
    if p:
        return p
    try:
        import imageio_ffmpeg
        return imageio_ffmpeg.get_ffmpeg_exe()
    except Exception as e:
        raise RuntimeError(
            f"ffmpeg not found in PATH and imageio_ffmpeg unavailable: {e}"
        )


FFMPEG = _resolve_ffmpeg()


# ---------- shared helpers ----------
def _font(size=32):
    for cand in ("arial.ttf", "C:/Windows/Fonts/arial.ttf", "C:/Windows/Fonts/msyh.ttc"):
        if os.path.exists(cand):
            return ImageFont.truetype(cand, size)
    return ImageFont.load_default()


def _hex_to_rgb(s: str) -> tuple[int, int, int]:
    s = s.lstrip("#")
    return (int(s[0:2], 16), int(s[2:4], 16), int(s[4:6], 16))


def _gradient_frame(w: int, h: int, scene_idx: int, scene_text: str,
                    subtitle: str = "", bg: str | None = None) -> Image.Image:
    """One Pillow frame: vertical gradient + scene label + optional subtitle."""
    bg = bg or ["#283278", "#b8504b", "#1e3c4a", "#5a2a6b", "#4a3c1e"][scene_idx % 5]
    fg = "#f0e8d8"
    a, b = bg, "#0e1117"
    rgb_a, rgb_b = _hex_to_rgb(a), _hex_to_rgb(b)
    img = Image.new("RGB", (w, h))
    px = img.load()
    for y in range(h):
        t = y / max(h - 1, 1)
        r = int(rgb_a[0] + (rgb_b[0] - rgb_a[0]) * t)
        g = int(rgb_a[1] + (rgb_b[1] - rgb_a[1]) * t)
        bch = int(rgb_a[2] + (rgb_b[2] - rgb_a[2]) * t)
        for x in range(w):
            px[x, y] = (r, g, bch)

    draw = ImageDraw.Draw(img)
    label = f"Scene {scene_idx + 1}"
    if scene_text:
        label += f": {scene_text[:60]}"
    big = _font(40)
    bbox = draw.textbbox((0, 0), label, font=big)
    tw, th = bbox[2] - bbox[0], bbox[3] - bbox[1]
    # main label centred
    draw.rectangle((0, h // 2 - th - 30, w, h // 2 + th + 10), fill=(0, 0, 0))
    draw.text(((w - tw) // 2, h // 2 - th - 18), label, font=big, fill=fg)

    if subtitle:
        sub = _font(28)
        bbox = draw.textbbox((0, 0), subtitle, font=sub)
        tw, th = bbox[2] - bbox[0], bbox[3] - bbox[1]
        draw.rectangle((0, h - th - 36, w, h - 10), fill=(0, 0, 0))
        draw.text(((w - tw) // 2, h - th - 26), subtitle, font=sub, fill=fg)
    return img


def _run_ffmpeg(frames: list[Image.Image], out_path: Path, fps: int,
                width: int, height: int, label: str) -> dict:
    """Pipe `frames` to ffmpeg as PNG, write H.264 mp4."""
    buf = io.BytesIO()
    cmd = [
        FFMPEG, "-y", "-loglevel", "error",
        "-f", "image2pipe", "-framerate", str(fps),
        "-vcodec", "png", "-i", "-",
        "-vf", f"scale={width}:{height}:force_original_aspect_ratio=decrease,"
               f"pad={width}:{height}:(ow-iw)/2:(oh-ih)/2:black",
        "-c:v", "libx264", "-pix_fmt", "yuv420p",
        "-movflags", "+faststart",
        str(out_path),
    ]
    proc = subprocess.run(cmd, input=b"".join(_png_bytes(im) for im in frames),
                          capture_output=True)
    if proc.returncode != 0:
        raise RuntimeError(f"ffmpeg failed ({label}): {proc.stderr.decode(errors='replace')[:400]}")
    return {
        "video_path": str(out_path),
        "frames": len(frames),
        "fps": fps,
        "width": width,
        "height": height,
        "size_bytes": out_path.stat().st_size,
    }


def _png_bytes(im: Image.Image) -> bytes:
    buf = io.BytesIO()
    im.save(buf, format="PNG")
    return buf.getvalue()


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
        # Treat as a single scene: "scene 1 | scene 2 | scene 3"
        return {"scenes": [seg.strip() for seg in s.split("|") if seg.strip()]}
    return {}


def _check_cancel(cancel_event, where: str, pause_event=None):
    """Cancel check + cooperative pause at a frame boundary."""
    if pause_event is not None:
        wait_if_paused(pause_event, cancel_event)
    if cancel_event is not None and cancel_event.is_set():
        raise TaskCancelled(f"cancelled at {where}")


# ---------- tasks ----------
def assemble_clip(task_id, payload, reporter, cancel_event=None, pause_event=None):
    """Build a slideshow mp4 from a list of scene descriptions.

    Payload (dict or str):
        scenes: list[str]            # each becomes one still frame
        fps: int (default 8)         # frames per second per scene
        sec_per_scene: float (1.5)   # duration of each scene
        width, height: int           # default 1280x720
    """
    reporter.progress(5, "[clip] parse")
    p = _parse_payload(payload)
    scenes = [str(s).strip() for s in (p.get("scenes") or []) if str(s).strip()]
    if not scenes:
        scenes = ["opening", "rising action", "climax", "ending"]
    fps = max(2, min(30, int(p.get("fps", 8))))
    sec = max(0.5, min(8.0, float(p.get("sec_per_scene", 1.5))))
    width = max(320, min(1920, int(p.get("width", 1280))))
    height = max(240, min(1080, int(p.get("height", 720))))
    n_per = int(round(fps * sec))

    reporter.progress(15, f"[clip] {len(scenes)} scenes x {n_per}f @ {fps}fps -> {width}x{height}")
    frames: list[Image.Image] = []
    for i, scene in enumerate(scenes):
        for f in range(n_per):
            _check_cancel(cancel_event, f"clip scene {i+1} frame {f+1}", pause_event)
            frames.append(_gradient_frame(width, height, i, scene))
        reporter.progress(15 + 80 * (i + 1) // len(scenes),
                          f"[clip] scene {i+1}/{len(scenes)}")

    out = OUTPUT_DIR / f"clip_{task_id[:8]}.mp4"
    info = _run_ffmpeg(frames, out, fps=fps, width=width, height=height, label="assemble_clip")
    reporter.progress(100, f"[clip] {info['size_bytes']:,} bytes")
    info["scenes"] = len(scenes)
    info["duration_s"] = round(info["frames"] / fps, 2)
    info["engine"] = f"ffmpeg {FFMPEG.split(os.sep)[-1]}"
    return info


def add_subtitle(task_id, payload, reporter, cancel_event=None, pause_event=None):
    """Build a slideshow mp4 where each scene gets its own subtitle overlay.

    Payload:
        scenes: list[str]
        subtitles: list[str] | None  # default = scene texts
        fps, sec_per_scene, width, height
    """
    reporter.progress(5, "[sub] parse")
    p = _parse_payload(payload)
    scenes = [str(s).strip() for s in (p.get("scenes") or []) if str(s).strip()]
    if not scenes:
        raise ValueError("add_subtitle requires 'scenes'")
    subtitles = p.get("subtitles") or scenes
    if isinstance(subtitles, str):
        subtitles = [subtitles]
    subtitles = [str(s).strip() for s in subtitles]
    fps = max(2, min(30, int(p.get("fps", 8))))
    sec = max(0.5, min(8.0, float(p.get("sec_per_scene", 2.0))))
    width = max(320, min(1920, int(p.get("width", 1280))))
    height = max(240, min(1080, int(p.get("height", 720))))
    n_per = int(round(fps * sec))

    reporter.progress(15, f"[sub] {len(scenes)} scenes w/ subtitles -> {width}x{height}")
    frames: list[Image.Image] = []
    for i, scene in enumerate(scenes):
        sub = subtitles[i] if i < len(subtitles) else scene
        for f in range(n_per):
            _check_cancel(cancel_event, f"sub scene {i+1} frame {f+1}", pause_event)
            frames.append(_gradient_frame(width, height, i, scene, subtitle=sub))
        reporter.progress(15 + 80 * (i + 1) // len(scenes),
                          f"[sub] scene {i+1}/{len(scenes)}")

    out = OUTPUT_DIR / f"sub_{task_id[:8]}.mp4"
    info = _run_ffmpeg(frames, out, fps=fps, width=width, height=height, label="add_subtitle")
    reporter.progress(100, f"[sub] {info['size_bytes']:,} bytes")
    info["scenes"] = len(scenes)
    info["duration_s"] = round(info["frames"] / fps, 2)
    info["engine"] = f"ffmpeg {FFMPEG.split(os.sep)[-1]}"
    return info


def kenburns(task_id, payload, reporter, cancel_event=None, pause_event=None):
    """Cinematic zoom/pan over a single generated frame.

    Payload:
        prompt: str              # text rendered as the subject
        fps: int (12)
        duration: float (3.0)    # seconds
        width, height: int (960x540)
        zoom: float (1.0..2.5)   # end / start scale (default 1.5)
        direction: 'in'|'out'|'left'|'right' (default 'in')
    """
    reporter.progress(5, "[kb] parse")
    p = _parse_payload(payload)
    prompt = (p.get("prompt") or "kenburns").strip()[:60]
    fps = max(2, min(30, int(p.get("fps", 12))))
    duration = max(1.0, min(8.0, float(p.get("duration", 3.0))))
    width = max(320, min(1920, int(p.get("width", 960))))
    height = max(240, min(1080, int(p.get("height", 540))))
    zoom = max(1.05, min(3.0, float(p.get("zoom", 1.5))))
    direction = (p.get("direction") or "in").lower()
    nframes = int(round(fps * duration))

    # Generate the source frame (2x larger so we can crop+zoom).
    src_w, src_h = width * 2, height * 2
    src = _gradient_frame(src_w, src_h, 0, prompt)
    draw = ImageDraw.Draw(src)
    big = _font(int(src_h * 0.18))
    bbox = draw.textbbox((0, 0), prompt, font=big)
    tw, th = bbox[2] - bbox[0], bbox[3] - bbox[1]
    draw.rectangle((0, src_h // 2 - th - 40, src_w, src_h // 2 + th + 20), fill=(0, 0, 0))
    draw.text(((src_w - tw) // 2, src_h // 2 - th - 28), prompt, font=big, fill="#f0e8d8")

    # Build a zoom-pan animation by re-cropping the source frame each step.
    # Simpler alternative would be ffmpeg's `zoompan` filter, but our
    # imageio_ffmpeg may not include z.lib; pre-rendering is portable.
    reporter.progress(25, f"[kb] {nframes} frames @ {fps}fps direction={direction}")
    frames: list[Image.Image] = []
    for i in range(nframes):
        _check_cancel(cancel_event, f"kenburns frame {i+1}/{nframes}", pause_event)
        t = i / max(nframes - 1, 1)
        if direction == "in":
            scale = 1.0 + (zoom - 1.0) * t
        elif direction == "out":
            scale = zoom - (zoom - 1.0) * t
        elif direction == "left":
            scale = 1.0 + (zoom - 1.0) * 0.5
        elif direction == "right":
            scale = 1.0 + (zoom - 1.0) * 0.5
        else:
            scale = 1.0 + (zoom - 1.0) * t

        cw, ch = int(src_w / scale), int(src_h / scale)
        # Pan: keep subject centred while scale changes.
        cx = (src_w - cw) // 2
        cy = (src_h - ch) // 2
        if direction in ("left", "right"):
            cx = int((src_w - cw) * (0.0 if direction == "left" else 1.0))
            cy = (src_h - ch) // 2
        frame = src.crop((cx, cy, cx + cw, cy + ch)).resize((width, height), Image.LANCZOS)
        frames.append(frame)
        if i % 4 == 0:
            reporter.progress(25 + 70 * i // nframes, f"[kb] frame {i+1}/{nframes}")

    out = OUTPUT_DIR / f"kenburns_{task_id[:8]}.mp4"
    info = _run_ffmpeg(frames, out, fps=fps, width=width, height=height, label="kenburns")
    reporter.progress(100, f"[kb] {info['size_bytes']:,} bytes")
    info.update({
        "prompt": prompt,
        "zoom": zoom,
        "direction": direction,
        "duration_s": round(nframes / fps, 2),
        "engine": f"ffmpeg {FFMPEG.split(os.sep)[-1]}",
    })
    return info
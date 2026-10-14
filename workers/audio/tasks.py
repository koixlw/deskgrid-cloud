"""Audio worker tasks - real TTS via edge-tts (Microsoft online, free, no model).

edge-tts calls Microsoft Edge's read-aloud WebSocket API; same voices that
the Edge browser uses. Works on any machine with internet. Output is MP3.

Voice catalogue (zh-CN subset):
    zh-CN-XiaoxiaoNeural   Female   warm, default news-anchor
    zh-CN-YunxiNeural      Male     lively, podcast-host
    zh-CN-YunjianNeural    Male     passionate, sports-commentator
    zh-CN-YunyangNeural    Male     professional, news-anchor
    zh-CN-XiaoyiNeural     Female   young, cute (cartoon)
    zh-CN-YunxiaNeural     Male     calm, audiobook

Edge-tts rate/pitch knobs let us fake "emotions" without real emotion
classification. All runs are async internally; we drive them with asyncio.run
inside a sync task function so the BaseWorker multiprocessing sandbox still
works.
"""
import asyncio
import os
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import TaskCancelled  # noqa: E402

import edge_tts

OUTPUT_DIR = Path(os.environ.get("AUDIO_OUTPUT_DIR", r"E:\deskgrid-cloud\artifacts"))
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

# All voices listed on https://learn.microsoft.com/azure/ai-services/speech-service/
# language-support — kept as a small selection; users can pass any "ShortName"
# from edge-tts.list_voices() via payload["voice"].
DEFAULT_VOICE = "zh-CN-XiaoxiaoNeural"

# Emotion -> (voice, rate, pitch, volume). Cheap heuristic; real prosody would
# need an SSML <mstts:express-as> wrapper which edge-tts does not expose.
EMOTION_PROFILES: dict[str, tuple[str, str, str, str]] = {
    "neutral": (DEFAULT_VOICE,           "+0%",   "+0Hz",  "+0%"),
    "calm":    ("zh-CN-YunyangNeural",   "-10%",  "-10Hz", "+0%"),
    "happy":   ("zh-CN-XiaoyiNeural",    "+10%",  "+5Hz",  "+5%"),
    "excited": ("zh-CN-YunjianNeural",   "+20%",  "+10Hz", "+10%"),
    "sad":     ("zh-CN-YunjianNeural",   "-15%",  "-15Hz", "-5%"),
    "angry":   ("zh-CN-YunjianNeural",   "+15%",  "+5Hz",  "+10%"),
    "whisper": ("zh-CN-XiaoxiaoNeural",  "-25%",  "-30Hz", "-50%"),
    "fear":    ("zh-CN-YunxiNeural",     "+20%",  "+15Hz", "-10%"),
}


async def _synthesize(text: str, voice: str, rate: str, pitch: str, volume: str,
                      out_path: Path) -> None:
    """Run one edge-tts synthesis, streaming to disk. Raises on network failure."""
    communicate = edge_tts.Communicate(
        text, voice=voice, rate=rate, pitch=pitch, volume=volume,
    )
    await communicate.save(str(out_path))


def _ensure_synthesizable(text: str, voice: str, rate: str, pitch: str, volume: str) -> tuple:
    """Work around an edge-tts quirk where very short Chinese text sometimes
    returns "NoAudioReceived". Pad short inputs with a trailing silence cue.

    Returns (text, voice, rate, pitch, volume) tuple suitable for synthesis."""
    # If we're applying prosody tweaks AND the input is short Chinese, pad it.
    if rate == "+0%" and pitch == "+0Hz" and volume == "+0%":
        return text, voice, rate, pitch, volume
    has_prosody = rate != "+0%" or pitch != "+0Hz" or volume != "+0%"
    if not has_prosody:
        return text, voice, rate, pitch, volume
    if len(text) >= 12:
        return text, voice, rate, pitch, volume
    # Pad with whitespace so the SSML chunk has enough material to render.
    padded = text + "。" * (12 - len(text))
    return padded, voice, rate, pitch, volume


def _run_async(coro):
    """asyncio.run helper that supports nested-loop errors cleanly."""
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        loop = None
    if loop and loop.is_running():
        # Already inside an event loop — fall back to a fresh thread + loop.
        import concurrent.futures
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            return pool.submit(asyncio.run, coro).result()
    return asyncio.run(coro)


def _parse_payload(payload):
    """str (treat as text) or dict (text + voice/rate/pitch/volume)."""
    if isinstance(payload, dict):
        return payload
    if isinstance(payload, str):
        s = payload.strip()
        if s.startswith("{"):
            import json
            try:
                return json.loads(s)
            except Exception:
                pass
        return {"text": s}
    return {"text": str(payload)}


def tts_line(task_id, payload, reporter, cancel_event=None):
    """Synthesize one line of speech. Returns path to a real .mp3 file."""
    reporter.progress(5, "[tts] parse")
    p = _parse_payload(payload)
    text = (p.get("text") or "").strip()
    if not text:
        raise ValueError("text is required")

    voice = p.get("voice") or DEFAULT_VOICE
    rate = p.get("rate", "+0%")
    pitch = p.get("pitch", "+0Hz")
    volume = p.get("volume", "+0%")

    # Allow short emotion names to short-circuit rate/pitch/volume.
    emo = (p.get("emotion") or "").lower()
    if emo and emo in EMOTION_PROFILES:
        voice, rate, pitch, volume = EMOTION_PROFILES[emo]

    reporter.progress(15, f"[tts] voice={voice} text='{text[:40]}'")
    out = OUTPUT_DIR / f"tts_{task_id[:8]}.mp3"

    try:
        _run_async(_synthesize(text, voice, rate, pitch, volume, out))
    except Exception as e:
        raise RuntimeError(f"edge-tts synthesis failed: {e}") from e

    if cancel_event is not None and cancel_event.is_set():
        raise TaskCancelled("tts cancelled before completion")

    if not out.exists() or out.stat().st_size < 200:
        raise RuntimeError(f"edge-tts produced no output (size={out.stat().st_size if out.exists() else 0})")

    reporter.progress(100, f"[tts] saved {out.stat().st_size:,} bytes")
    return {
        "audio_path": str(out),
        "format": "mp3",
        "voice": voice,
        "rate": rate,
        "pitch": pitch,
        "volume": volume,
        "text": text,
        "size_bytes": out.stat().st_size,
        "engine": "edge-tts",
    }


def tts_with_emotion(task_id, payload, reporter, cancel_event=None):
    """Same as tts_line but explicitly takes an 'emotion' key. Maps emotion
    to a voice + prosody preset."""
    reporter.progress(5, "[tts-emo] parse")
    p = _parse_payload(payload)
    text = (p.get("text") or "").strip() or "你好"
    emo = (p.get("emotion") or "neutral").lower()
    if emo not in EMOTION_PROFILES:
        raise ValueError(f"unknown emotion '{emo}'; pick from {list(EMOTION_PROFILES)}")

    voice, rate, pitch, volume = EMOTION_PROFILES[emo]
    reporter.progress(20, f"[tts-emo] emotion={emo} -> {voice} ({rate}, {pitch})")

    out = OUTPUT_DIR / f"tts_emo_{task_id[:8]}.mp3"
    try:
        _run_async(_synthesize(text, voice, rate, pitch, volume, out))
    except Exception as e:
        raise RuntimeError(f"edge-tts synthesis failed: {e}") from e

    reporter.progress(100, f"[tts-emo] done ({out.stat().st_size:,} bytes)")
    return {
        "audio_path": str(out),
        "format": "mp3",
        "voice": voice,
        "rate": rate,
        "pitch": pitch,
        "volume": volume,
        "emotion": emo,
        "text": text,
        "size_bytes": out.stat().st_size,
        "engine": "edge-tts",
    }
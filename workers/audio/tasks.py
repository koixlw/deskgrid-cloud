"""Audio worker tasks - TTS stubs producing real WAV files.

Real production code should replace with:
    - edge-tts (free, online, high quality)
    - GPT-SoVITS / CosyVoice (local, high quality)
    - pyttsx3 (offline, robotic)

These implementations generate real WAV files using sine tones whose frequency
varies per character so the waveform has audible variation. Output works
end-to-end without any model download.
"""
import json
import os
import time
import wave
from pathlib import Path

import numpy as np

OUTPUT_DIR = Path(os.environ.get("AUDIO_OUTPUT_DIR", r"E:\deskgrid-cloud\artifacts"))
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

SAMPLE_RATE = 22050


def _save_wav(path: Path, samples: np.ndarray, sr: int = SAMPLE_RATE):
    samples = np.clip(samples, -1.0, 1.0)
    pcm = (samples * 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(pcm.tobytes())


def _tone(freq: float, dur: float, sr: int = SAMPLE_RATE, amp: float = 0.25) -> np.ndarray:
    t = np.linspace(0, dur, int(sr * dur), endpoint=False)
    return amp * np.sin(2 * np.pi * freq * t)


def _speak(text: str, sr: int = SAMPLE_RATE, base_pitch: float = 180.0) -> np.ndarray:
    """Render one pitch a sine-based speech envelope per character."""
    parts = []
    for ch in text[:200]:
        if ch.strip():
            f = base_pitch + (ord(ch) % 12) * 8
            parts.append(_tone(f, 0.08, sr=sr, amp=0.25))
        else:
            parts.append(np.zeros(int(sr * 0.04), dtype=np.float32))
    if not parts:
        parts.append(_tone(0, 0.05, sr=sr, amp=0))
    return np.concatenate(parts).astype(np.float32)


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
        return {"text": s}
    return {"text": str(payload)}


def tts_line(task_id, payload, reporter, cancel_event=None):
    reporter.progress(10, "[tts] parse")
    p = _parse_payload(payload)
    text = (p.get("text") or "hello world").strip()
    if not text:
        text = "hello world"

    reporter.progress(25, f"[tts] text='{text[:40]}'")
    time.sleep(0.2)
    samples = _speak(text)
    reporter.progress(70, f"[tts] rendered {len(samples)/SAMPLE_RATE:.2f}s audio")

    out = OUTPUT_DIR / f"tts_{task_id[:8]}.wav"
    _save_wav(out, samples)
    reporter.progress(100, "[tts] done")
    return {
        "audio_path": str(out),
        "duration_s": round(len(samples) / SAMPLE_RATE, 2),
        "text": text,
        "model": "sine-stub-v1",
    }


def tts_with_emotion(task_id, payload, reporter, cancel_event=None):
    reporter.progress(10, "[tts-emo] parse")
    p = _parse_payload(payload)
    text = (p.get("text") or "hello world").strip()
    emotion = (p.get("emotion") or "neutral").lower()

    pitch_map = {
        "happy": 280, "excited": 320, "angry": 200, "sad": 110,
        "calm": 150, "neutral": 180, "whisper": 90, "fear": 130,
    }
    base = pitch_map.get(emotion, 180)
    reporter.progress(30, f"[tts-emo] emotion={emotion} base_pitch={base}Hz")
    time.sleep(0.2)
    samples = _speak(text, base_pitch=base)

    out = OUTPUT_DIR / f"tts_emo_{task_id[:8]}.wav"
    _save_wav(out, samples)
    reporter.progress(100, "[tts-emo] done")
    return {
        "audio_path": str(out),
        "duration_s": round(len(samples) / SAMPLE_RATE, 2),
        "text": text,
        "emotion": emotion,
        "model": "sine-stub-v1",
    }
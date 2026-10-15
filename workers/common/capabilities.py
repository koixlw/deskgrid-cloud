"""Auto-detect which tools, runtimes, and libraries this worker has.

Called once by BaseWorker.register() and sent to the gateway as the worker's
``capabilities`` list. The gateway uses this list to hard-filter tasks that
require specific capabilities before scoring candidates (Phase 1 of the
resource-aware scheduling feature).

Naming convention:
- Bare tool names (``python``, ``ffmpeg``, ``edge-tts``, ``pillow``,
  ``git``, ``node``, ``curl``...) match common CLI / library identifiers
  that callers might pass in a task's ``require_caps``.
- Type tags use a ``type:<name>`` form (``type:audio``, ``type:video``...)
  so panels can ask "any worker that is *primarily* an audio node".
- Model / dataset identifiers use ``model:<name>`` (``model:sdxl``,
  ``model:controlnet``). Auto-detected by ``_scan_model_dirs`` below when
  ``WORKER_MODEL_DIRS`` is set; can also be added via ``WORKER_CAPABILITIES``.

User override:
- The ``WORKER_CAPABILITIES`` env var is a comma-separated list appended
  after auto-detection. Useful for adding model tags or for stubbing
  tools that can't be probed (``ffmpeg`` may exist but not actually work
  for a given model — operator can drop the ``ffmpeg`` cap).
"""
import os
import shutil
from typing import List


# Tools we probe via shutil.which. Order is the order they appear in the
# reported list (after de-dup); alphabetical inside the result anyway.
_TOOLS_TO_PROBE = (
    "python", "python3",
    "ffmpeg", "ffprobe",
    "edge-tts",
    "node", "npm",
    "git", "curl",
    "pip", "uv",
    "magick",
)


# Capabilities declared by virtue of the worker's type. These are appended
# unconditionally so that operators can rely on a stable tag like
# "edge-tts" being present on every audio worker, even before the auto-detect
# loop has a chance to run.
_TYPE_DEFAULT_CAPS = {
    "audio": ("edge-tts",),
    "video": ("ffmpeg", "ffprobe"),
    "image": ("pillow",),
    "exec":  ("python", "cmd"),
    # code / doc / csv are stdlib-only; no default extra caps.
}


def _probe_python_package(modname: str, tag: str) -> str | None:
    """Return ``tag`` if ``modname`` imports cleanly, else ``None``.

    Cheap (just an importlib test). Both arms wrapped in try so missing
    packages are silent no-ops.
    """
    try:
        __import__(modname)
        return tag
    except Exception:
        return None


# File extensions we treat as "this directory holds a model named X".
# Recognised at the *direct* child of each path in WORKER_MODEL_DIRS — we
# don't recurse, so a 50 GB nested tree won't be walked.
_MODEL_FILE_EXTS = (".safetensors", ".ckpt", ".pt", ".pth", ".bin", ".onnx", ".gguf")


def _scan_model_dirs() -> List[str]:
    """Emit ``model:<name>`` caps from ``WORKER_MODEL_DIRS``.

    The env var is a ``;``-separated list of directories (mirrors the
    style of ``EXEC_ALLOWED_CWD_ROOTS``). For each directory, we look at
    its *direct* children only — subdirectories become ``model:<dirname>``,
    and files with a known model extension become ``model:<basename>``.
    A typical layout like::

        /data/models/
            sdxl/             # ← emits model:sdxl
            controlnet/        # ← emits model:controlnet
            whisper/           # ← emits model:whisper
            notes.md           # ← ignored (unknown ext)

    produces ``["model:controlnet", "model:sdxl", "model:whisper"]``.

    Missing / unreadable directories are silently skipped so a partially-
    mounted share never breaks worker startup.
    """
    raw = os.environ.get("WORKER_MODEL_DIRS", "").strip()
    if not raw:
        return []
    out: list[str] = []
    for piece in raw.split(";"):
        d = piece.strip()
        if not d or not os.path.isdir(d):
            continue
        try:
            entries = list(os.scandir(d))
        except OSError:
            continue
        for e in entries:
            name = e.name
            if name.startswith("."):
                continue
            if e.is_dir():
                out.append(f"model:{name}")
                continue
            # Files: only emit for known model extensions so a notes.md
            # sitting next to weights doesn't pollute the caps list.
            lower = name.lower()
            for ext in _MODEL_FILE_EXTS:
                if lower.endswith(ext):
                    out.append(f"model:{name[: -len(ext)]}")
                    break
    return out


def collect(worker_type: str) -> List[str]:
    """Return the sorted, de-duplicated capability list for this worker.

    Output is stable so the gateway sees the same string ordering on every
    register, which keeps log diffs and replication snapshots tidy.
    """
    caps: list[str] = [f"type:{worker_type}"]

    # Type-default tags (always present for the worker's declared type).
    caps.extend(_TYPE_DEFAULT_CAPS.get(worker_type, ()))

    # PATH probes for common CLIs.
    for tool in _TOOLS_TO_PROBE:
        if shutil.which(tool):
            caps.append(tool)

    # Python package probes. Each is wrapped so a missing module never
    # blocks capability collection for the others.
    for tag in (
        _probe_python_package("PIL",         "pillow"),
        _probe_python_package("edge_tts",    "edge-tts-lib"),
        _probe_python_package("numpy",       "numpy"),
        _probe_python_package("requests",    "requests"),
    ):
        if tag:
            caps.append(tag)

    # Model / dataset scans — must run before the user-override so the
    # override can still add tags that didn't make it into the dir name.
    caps.extend(_scan_model_dirs())

    # Operator-supplied overrides (added last so they always win ties).
    raw = os.environ.get("WORKER_CAPABILITIES", "")
    for c in raw.split(","):
        c = c.strip()
        if c:
            caps.append(c)

    # Stable, de-duplicated.
    return sorted(set(caps))
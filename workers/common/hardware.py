"""Real hardware info collection via psutil + platform."""
import platform
import psutil


def collect() -> dict:
    """Return a dict of real hardware info for this worker."""
    cpu_model = platform.processor() or "Unknown CPU"
    if not cpu_model or cpu_model.strip() == "":
        # fallback: use machine + first non-empty cpuinfo entry
        try:
            cpu_model = next(
                (line.split(":")[1].strip() for line in open("/proc/cpuinfo").readlines()
                 if line.startswith("model name")),
                platform.machine() or "Unknown CPU",
            )
        except Exception:
            cpu_model = platform.machine() or "Unknown CPU"

    vm = psutil.virtual_memory()
    return {
        "cpu": cpu_model,
        "cores": psutil.cpu_count(logical=True) or 1,
        "memory_mb": int(vm.total / 1024 / 1024),
    }


def snapshot_usage() -> dict:
    """Return a snapshot of current CPU/memory usage percentages."""
    # Non-blocking; psutil computes delta since last call. We sleep briefly
    # to get a stable reading (interval=None on first call is OK on most OS).
    cpu = psutil.cpu_percent(interval=0.1)
    vm = psutil.virtual_memory()
    return {
        "cpu_usage": float(cpu),
        "mem_usage": float(vm.percent),
    }

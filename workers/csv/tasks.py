"""CSV worker task implementations — Monte Carlo simulation and descriptive stats."""
import json
import math
import os
import random
import sys
import time
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from common.base import TaskCancelled  # noqa: E402


def _parse_numbers(payload: str) -> list[float]:
    payload = payload.strip()
    # try JSON first
    if payload.startswith("[") or payload.startswith("{"):
        try:
            data = json.loads(payload)
            if isinstance(data, list):
                return [float(x) for x in data]
        except Exception:
            pass
    # fall back to comma / whitespace / newline separated
    out = []
    for tok in payload.replace(",", " ").replace("\n", " ").split():
        try:
            out.append(float(tok))
        except ValueError:
            continue
    return out


def montecarlo(task_id: str, payload: str, reporter, cancel_event=None) -> dict:
    """Run a Monte Carlo simulation.

    Payload formats accepted:
    - JSON: {"samples": N, "distribution": "uniform"|"normal"|"exponential",
              "low": 0, "high": 1, "mean": 0, "stddev": 1, "rate": 1}
    - Bare integer N (defaults to uniform[0,1], N samples)
    """
    reporter.progress(5, "[mc] 解析参数")
    time.sleep(0.15)
    config = {"samples": 10000, "distribution": "uniform", "low": 0.0, "high": 1.0,
              "mean": 0.0, "stddev": 1.0, "rate": 1.0}
    try:
        data = json.loads(payload)
        if isinstance(data, dict):
            config.update({k: v for k, v in data.items() if k in config})
        elif isinstance(data, (int, float)):
            config["samples"] = int(data)
    except Exception:
        try:
            config["samples"] = int(payload.strip())
        except ValueError:
            return {"success": False, "error": f"invalid payload: {payload[:80]}"}

    n = max(1, min(10_000_000, int(config["samples"])))
    dist = config["distribution"]
    reporter.progress(15, f"[mc] 采样 {n} 次，分布={dist}")
    time.sleep(0.1)

    # sampling loop with progress reporting every ~5%
    samples = []
    chunk = max(1000, n // 20)
    if dist == "uniform":
        lo, hi = float(config["low"]), float(config["high"])
        for i in range(n):
            if cancel_event and cancel_event.is_set():
                raise TaskCancelled(f"montecarlo uniform cancelled at {i}/{n}")
            samples.append(random.uniform(lo, hi))
            if (i + 1) % chunk == 0:
                pct = 15 + int(70 * (i + 1) / n)
                reporter.progress(pct, f"[mc] uniform 采样 {i+1}/{n}")
    elif dist == "normal":
        mu, sigma = float(config["mean"]), max(1e-9, float(config["stddev"]))
        for i in range(n):
            if cancel_event and cancel_event.is_set():
                raise TaskCancelled(f"montecarlo normal cancelled at {i}/{n}")
            samples.append(random.gauss(mu, sigma))
            if (i + 1) % chunk == 0:
                pct = 15 + int(70 * (i + 1) / n)
                reporter.progress(pct, f"[mc] normal 采样 {i+1}/{n}")
    elif dist == "exponential":
        rate = max(1e-9, float(config["rate"]))
        for i in range(n):
            if cancel_event and cancel_event.is_set():
                raise TaskCancelled(f"montecarlo exp cancelled at {i}/{n}")
            samples.append(random.expovariate(rate))
            if (i + 1) % chunk == 0:
                pct = 15 + int(70 * (i + 1) / n)
                reporter.progress(pct, f"[mc] exp 采样 {i+1}/{n}")
    else:
        return {"success": False, "error": f"unknown distribution: {dist}"}

    reporter.progress(90, "[mc] 计算统计量")
    n_s = len(samples)
    mean = sum(samples) / n_s
    var = sum((x - mean) ** 2 for x in samples) / n_s
    std = math.sqrt(var)
    samples_sorted = sorted(samples)
    median = samples_sorted[n_s // 2] if n_s % 2 == 1 else \
        (samples_sorted[n_s // 2 - 1] + samples_sorted[n_s // 2]) / 2
    # 95% confidence interval using 1.96 * stddev / sqrt(n)
    ci_half = 1.96 * std / math.sqrt(n_s)

    reporter.progress(99, "[mc] 完成")
    return {
        "success": True,
        "distribution": dist,
        "samples": n_s,
        "mean": mean,
        "stddev": std,
        "variance": var,
        "min": min(samples),
        "max": max(samples),
        "median": median,
        "ci95_low": mean - ci_half,
        "ci95_high": mean + ci_half,
        "config": config,
    }


def stats(task_id: str, payload: str, reporter, cancel_event=None) -> dict:
    """Compute descriptive statistics for a list of numbers."""
    reporter.progress(15, "[stats] 解析数字")
    time.sleep(0.15)
    nums = _parse_numbers(payload)
    if not nums:
        return {"success": False, "error": "no numbers found"}
    reporter.progress(40, f"[stats] 共 {len(nums)} 个数字")
    time.sleep(0.15)

    nums_sorted = sorted(nums)
    n = len(nums)
    s = sum(nums)
    mean = s / n
    var = sum((x - mean) ** 2 for x in nums) / n
    std = math.sqrt(var)
    median = nums_sorted[n // 2] if n % 2 == 1 else \
        (nums_sorted[n // 2 - 1] + nums_sorted[n // 2]) / 2

    def percentile(p):
        if n == 1:
            return nums_sorted[0]
        k = (n - 1) * (p / 100.0)
        f = math.floor(k)
        c = math.ceil(k)
        if f == c:
            return nums_sorted[int(k)]
        return nums_sorted[f] * (c - k) + nums_sorted[c] * (k - f)

    reporter.progress(85, "[stats] 计算百分位")
    return {
        "success": True,
        "count": n,
        "sum": s,
        "mean": mean,
        "stddev": std,
        "variance": var,
        "min": min(nums),
        "max": max(nums),
        "median": median,
        "p25": percentile(25),
        "p50": percentile(50),
        "p75": percentile(75),
        "p95": percentile(95),
    }

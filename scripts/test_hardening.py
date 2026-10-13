"""Verify body size limits + connection pool. Run with python (not bash) so we
avoid the Windows 8KB argv limit when sending big payloads."""
import json
import time
import urllib.request
import urllib.error


def post(path, data, content_type="application/json", timeout=10):
    req = urllib.request.Request(
        f"http://127.0.0.1:13000{path}",
        data=data if isinstance(data, (bytes, bytearray)) else json.dumps(data).encode(),
        headers={"Content-Type": content_type},
        method="POST",
    )
    try:
        r = urllib.request.urlopen(req, timeout=timeout)
        return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def main():
    print("[1] basic task still works")
    s, b = post("/api/tasks", {"type": "code_format", "payload": "def f():return 1"})
    print(f"  HTTP {s}  body[:80]={b[:80]!r}")

    print("\n[2] 20KB heartbeat -> 16KB cap (expect 4xx)")
    big = "a" * 20000
    s, b = post(
        "/api/workers/abc/heartbeat",
        {"cpu_usage": 50, "junk": big},
    )
    print(f"  HTTP {s}  body[:80]={b[:80]!r}")

    print("\n[3] 70KB task -> 64KB cap (expect 4xx)")
    big = "a" * 70000
    s, b = post("/api/tasks", {"type": "code_format", "payload": big})
    print(f"  HTTP {s}  body[:80]={b[:80]!r}")

    print("\n[4] 60KB task -> within cap (expect 200)")
    big = "a" * 60000
    s, b = post("/api/tasks", {"type": "code_format", "payload": big})
    print(f"  HTTP {s}")

    print("\n[5] 30 concurrent /api/workers (connection pool)")
    import threading
    results = [None] * 30
    def hit(idx):
        try:
            req = urllib.request.Request("http://127.0.0.1:13000/api/workers")
            r = urllib.request.urlopen(req, timeout=10)
            results[idx] = r.status
        except Exception as e:
            results[idx] = str(e)
    threads = [threading.Thread(target=hit, args=(i,)) for i in range(30)]
    t0 = time.time()
    for t in threads: t.start()
    for t in threads: t.join()
    dt = time.time() - t0
    ok = sum(1 for r in results if r == 200)
    print(f"  {ok}/30 succeeded in {dt*1000:.0f}ms")
    print("  (succeeded fast -> connection pool handles concurrency)")


if __name__ == "__main__":
    main()
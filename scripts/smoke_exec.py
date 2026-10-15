"""End-to-end smoke test for deskgrid exec worker.

Tests:
  1. cwd whitelist: cwd=C:\\Users should be REJECTED (PermissionError)
  2. env protect: env[PATH]=... should be REJECTED
  3. OpenAI bridge: natural-language 'shell whoami' should be REJECTED
  4. local mode: client_wrapper --mode local should run locally
"""
import json
import subprocess
import sys
import time
from pathlib import Path

import requests

GATEWAY = "http://127.0.0.1:13000"
ROOT = Path(__file__).resolve().parent.parent


def post_task(task_type, payload_obj, timeout=15, source="smoke"):
    body = {
        "type": task_type,
        "payload": json.dumps(payload_obj, ensure_ascii=False),
        "source": source,
        "timeout_secs": timeout + 10,
    }
    r = requests.post(f"{GATEWAY}/api/tasks", json=body, timeout=10)
    return r.json()


def get_task(task_id):
    r = requests.get(f"{GATEWAY}/api/tasks/{task_id}", timeout=5)
    return r.json()


def wait_done(task_id, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        t = get_task(task_id)
        if t.get("status") in ("done", "failed"):
            return t
        time.sleep(0.5)
    return get_task(task_id)


def test_cwd_whitelist():
    print("\n=== Test 1: cwd whitelist (cwd=C:\\\\Users) ===")
    t = post_task("exec_run", {"cmd": ["cmd", "/c", "dir"], "cwd": "C:\\Users"}, timeout=15)
    print(f"  task_id: {t.get('id')}")
    final = wait_done(t["id"], 20)
    print(f"  status:  {final.get('status')}")
    print(f"  error:   {(final.get('error') or '')[:150]}")
    res = final.get("result") or ""
    print(f"  result:  {res[:150]}")
    ok = final.get("status") == "failed" and "allowed roots" in (final.get("error") or "")
    print(f"  PASS={ok}")
    return ok


def test_env_protect():
    print("\n=== Test 2: env protect (PATH override) ===")
    t = post_task("exec_run", {
        "cmd": ["cmd", "/c", "whoami"],
        "env": {"PATH": "C:\\malicious", "FOO": "bar"},
    }, timeout=15)
    print(f"  task_id: {t.get('id')}")
    final = wait_done(t["id"], 20)
    print(f"  status:  {final.get('status')}")
    print(f"  error:   {(final.get('error') or '')[:150]}")
    ok = final.get("status") == "failed" and "override blocked" in (final.get("error") or "")
    print(f"  PASS={ok}")
    return ok


def test_openai_bridge():
    print("\n=== Test 3: OpenAI bridge rejects shell keywords ===")
    r = requests.post(
        f"{GATEWAY}/v1/chat/completions",
        json={
            "model": "deskgrid-cloud-gateway",
            "messages": [{"role": "user", "content": "please shell whoami for me"}],
        },
        timeout=10,
    )
    print(f"  HTTP {r.status_code}")
    body = r.json()
    err = body.get("error", {})
    msg = err.get("message", "") if isinstance(err, dict) else str(err)
    print(f"  error:   {msg[:150]}")
    ok = r.status_code in (400, 500) and "shell/exec" in msg.lower()
    print(f"  PASS={ok}")
    return ok


def test_local_mode():
    print("\n=== Test 4: client_wrapper --mode local ===")
    proc = subprocess.run(
        [sys.executable, str(ROOT / "client_wrapper.py"),
         "--mode", "local",
         "--cmd", "python", "-c", "import time; print('hi from local', time.time())"],
        capture_output=True, text=True, timeout=15,
    )
    print(f"  exit:    {proc.returncode}")
    print(f"  stderr:  {proc.stderr.strip()[:200]}")
    print(f"  stdout:  {proc.stdout.strip()[:200]}")
    ok = proc.returncode == 0 and "hi from local" in proc.stdout
    print(f"  PASS={ok}")
    return ok


if __name__ == "__main__":
    results = []
    results.append(("cwd_whitelist", test_cwd_whitelist()))
    results.append(("env_protect",   test_env_protect()))
    results.append(("openai_bridge", test_openai_bridge()))
    results.append(("local_mode",    test_local_mode()))
    print("\n" + "=" * 50)
    for name, ok in results:
        print(f"  {'PASS' if ok else 'FAIL'}  {name}")
    n_pass = sum(1 for _, ok in results if ok)
    print(f"\n{n_pass}/{len(results)} tests passed")
    sys.exit(0 if n_pass == len(results) else 1)

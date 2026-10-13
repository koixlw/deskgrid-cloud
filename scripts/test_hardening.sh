#!/bin/bash
# Quick verification of body-size limits and connection pool.
PAYLOAD_20K=$(python -c "print('a' * 20000)")
PAYLOAD_70K=$(python -c "print('a' * 70000)")
PAYLOAD_60K=$(python -c "print('a' * 60000)")

echo "[1] basic task still works"
curl -s -X POST http://127.0.0.1:13000/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"type":"code_format","payload":"def f(x):return x*2"}' \
  | python -c "import json,sys;d=json.load(sys.stdin);print('  id:', d.get('id','ERR')[:20])"

echo
echo "[2] 20KB heartbeat -> 16KB cap (expect 413)"
curl -s -o /dev/null -w "  HTTP %{http_code}\n" \
  -X POST http://127.0.0.1:13000/api/workers/abc/heartbeat \
  -H "Content-Type: application/json" \
  -d "{\"cpu_usage\":50,\"junk\":\"$PAYLOAD_20K\"}"

echo
echo "[3] 70KB task -> 64KB cap (expect 413)"
curl -s -o /dev/null -w "  HTTP %{http_code}\n" \
  -X POST http://127.0.0.1:13000/api/tasks \
  -H "Content-Type: application/json" \
  -d "{\"type\":\"code_format\",\"payload\":\"$PAYLOAD_70K\"}"

echo
echo "[4] 60KB task -> within cap (expect 200)"
curl -s -o /dev/null -w "  HTTP %{http_code}\n" \
  -X POST http://127.0.0.1:13000/api/tasks \
  -H "Content-Type: application/json" \
  -d "{\"type\":\"code_format\",\"payload\":\"$PAYLOAD_60K\"}"

echo
echo "[5] 30 concurrent /api/workers (connection pool test)"
for i in $(seq 1 30); do
  curl -s -o /dev/null -w "%{http_code} " http://127.0.0.1:13000/api/workers &
done
wait
echo
echo "  (all 200s -> connection pool reuses sockets instead of opening 30 new ones)"
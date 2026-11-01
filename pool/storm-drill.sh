#!/bin/bash
# storm-drill (1 Nov): end-to-end proof of the storm recovery path WITHOUT
# waiting for Aliyun to actually storm. It swaps a throwaway pool of bogus
# device tokens into the LIVE bridge, fires requests until the captcha breaker
# trips, then watches the REAL recovery: the pre-minted standby pool must be
# swapped in instantly (standbySwaps+1, poolPath becomes a tokens-standby-*
# file, breaker closed) and serving must resume.
# Bounded by design: bogus tokens burn nothing of value, and the worst case is
# the same mint-then-swap recovery a real storm already gets today.
set -u
cd /home/ec2-user/zai-api-dev || exit 1
. /home/ec2-user/zai-api-dev/.swapenv
AUTHHDR="Authorization: Bearer $AUTH_TOKEN"
KEY=$(cat /home/ec2-user/workers/.glmkey)
BOGUS=/tmp/drill-bogus-$$.sqlite

state() {
    curl -s -m 5 http://127.0.0.1:3012/health | python3 -c 'import sys,json
d=json.load(sys.stdin); c=d["captcha"]
print("breaker=%s trips=%s standbyReady=%s standbySwaps=%s poolPath=%s tokenCount=%s" % (
    c["breaker"], c["breakerTrips"], c["standbyReady"], c["standbySwaps"], c["poolPath"], d["tokenCount"]))'
}

echo "=== BEFORE ==="; state
PREV_POOL=$(curl -s -m 5 http://127.0.0.1:3012/health | python3 -c 'import sys,json;print(json.load(sys.stdin)["captcha"]["poolPath"])')
if ! curl -s -m 5 http://127.0.0.1:3012/health | python3 -c 'import sys,json;sys.exit(0 if json.load(sys.stdin)["captcha"]["standbyReady"] else 1)'; then
    echo "ABORT: standby belum siap — drill ini bakal ngukur jalur mint yang lambat, bukan jalur standby."
    exit 1
fi

python3 - "$BOGUS" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT NOT NULL)")
for i in range(50):
    db.execute("INSERT INTO tokens(token) VALUES (?)", ("drill-bogus-%d" % i,))
db.commit(); db.close()
print("bogus pool: 50 token palsu")
PY

echo "--- swap bogus pool ke bridge LIVE ---"
curl -s -X POST -H "$AUTHHDR" -H "Content-Type: application/json" -d "{\"db_path\":\"$BOGUS\"}" http://127.0.0.1:3012/sqlite; echo
SINCE=$(date -u '+%Y-%m-%d %H:%M:%S')

for i in 1 2 3; do
    echo "--- request #$i (harus gagal: token palsu) ---"
    curl -s -m 60 -X POST http://127.0.0.1:3012/v1/chat/completions -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
      -d '{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"ping"}]}' | head -c 160; echo
    sleep 1
done

echo "=== AFTER TRIP ==="; state
echo "=== RECOVERY LOG ==="
journalctl -u zai-api-dev --since "$SINCE" --no-pager | grep -E "standby pool swapped in instantly|circuit breaker OPEN|storm detected|storm auto-refill" | tail -6

echo "=== REQUEST #4 (harus SUKSES) ==="
OUT=$(curl -s -m 90 -X POST http://127.0.0.1:3012/v1/chat/completions -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"say OK"}]}')
echo "DONE=$(echo "$OUT" | grep -c 'data: \[DONE\]') ERR=$(echo "$OUT" | grep -ci 'error')"

echo "=== ROLLBACK (kalau perlu, token dibaca dari .swapenv) ==="
echo ". .swapenv; curl -s -X POST -H \"Authorization: Bearer \$AUTH_TOKEN\" -H 'Content-Type: application/json' -d '{\"db_path\":\"$PREV_POOL\"}' http://127.0.0.1:3012/sqlite"
rm -f "$BOGUS"

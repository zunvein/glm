#!/bin/sh
# GLM token watchdog (29 Okt): kalau live pool jatuh di bawah floor, langsung
# mint batch segar + hot-swap. Konteks: pas "verify-storm" (Aliyun balik
# VerifyResult=false buat SEMUA token), tiap hitungan captcha ngabisin 5 token
# (maxTokenRetries=5) dan loop prewarm session-pool nyedot ~12 token/menit —
# pool 120 bisa evakuasi dalam ~10 menit, jauh sebelum tick refill 30-menitan.
# Watchdog = pengejar: tiap 2 menit cek /health; kalau di bawah MIN, refill.
# No-op total kalau pool sehat. Cooldown mencegah mint beruntun.
set -u
ZAI_HOME=${ZAI_HOME:-$(pwd)}
POOL=$ZAI_HOME/pool
BRIDGE_URL=${BRIDGE_URL:-http://127.0.0.1:${PORT:-3012}}
MIN=${WATCHDOG_MIN:-60}        # floor tokenCount (tuned 31 Okt: refill earlier for bigger safety margin)
COOLDOWN=${WATCHDOG_COOLDOWN:-300}  # detik minimum antara refill yang di-trigger watchdog

NOW=$(date +%s)
MARK=$POOL/.wd-last
if [ -f "$MARK" ]; then
    LAST=$(cat "$MARK" 2>/dev/null || echo 0)
    [ "$((NOW - LAST))" -lt "$COOLDOWN" ] && exit 0
fi

C=$(curl -s -m 5 "$BRIDGE_URL/health" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin).get("tokenCount",-1))' 2>/dev/null) || C=-1

# hanya gas kalau count valid (>=0) DAN di bawah floor
if [ "$C" -ge 0 ] && [ "$C" -lt "$MIN" ]; then
    echo "$NOW" > "$MARK"
    echo "$(date -u +%FT%TZ) WATCHDOG: tokenCount=$C < $MIN -> mint+swap" >> "$POOL/swap.log"
    /bin/sh "$POOL/refill.sh"
fi
exit 0

#!/bin/bash
# storm-alert (1 Nov): the bridge's storm machinery is fully automatic now
# (breaker -> instant standby swap -> background mint), which also means a
# storm can happen WITHOUT anyone watching. This poller turns /health's
# "captcha" block + swap.log into one Telegram line, so the owner hears about
# a storm from the bot instead of from a broken reply.
# Reuses glm-watch's notify path (TG_TOKEN in workers/.tgtoken, same chat,
# same zunbot /dbg kill-switch) so there is exactly one alerting convention.
set -u
ZAI_HOME=${ZAI_HOME:-$(pwd)}
D=$ZAI_HOME/pool
BRIDGE_URL=${BRIDGE_URL:-http://127.0.0.1:${PORT:-3012}}
TG_TOKEN_FILE=${TG_TOKEN_FILE:-.tgtoken}
LOG=$D/storm-alert.log
STATE=$D/.storm-alert-state
CHAT=${TG_CHAT:-1282949142}
DEDUPE=300          # seconds between two alerts
STANDBY_GRACE=1800  # how long "no standby ready" may persist before alerting

H=$(curl -s -m 5 "$BRIDGE_URL/health" 2>/dev/null)
# Bridge unreachable => not this script's job (glm-watch alerts on edge/TLS).
[ -z "$H" ] && exit 0

STATS=$(printf '%s' "$H" | python3 -c 'import sys,json
d=json.load(sys.stdin); c=d.get("captcha",{})
print(c.get("breakerTrips",0), c.get("autoRefills",0), 1 if c.get("standbyReady") else 0, c.get("standbySwaps",0))' 2>/dev/null)
[ -z "$STATS" ] && exit 0
read -r TRIPS REFILLS SBREADY SBSWAPS <<<"$STATS"

NOW=$(date +%s)
PTRIPS=0; PREFILLS=0; PFAILS=0; PLAST=0; PMISSING=0; PSBSWAPS=0
if [ -f "$STATE" ]; then
    read -r PTRIPS PREFILLS PFAILS PLAST PMISSING PSBSWAPS < "$STATE" 2>/dev/null || true
fi

# failure markers in the tail of swap.log (window count: an increase = new one)
FAILS=$(tail -n 300 "$D/swap.log" 2>/dev/null | grep -cE "MINT FAILED|STAGE FAILED|SWAP EXHAUSTED|MERGE FAILED|STANDBY MINT FAILED")
MISSING=$PMISSING
[ "$SBREADY" = 0 ] && MISSING=$(( MISSING + 1 )) || MISSING=0

MSG=""
if [ "$TRIPS" -gt "$PTRIPS" ]; then
    # The instant standby swap happens in the same second as the trip, so by
    # the time this poller reads /health the standby is already CONSUMED
    # (standbyReady=false). standbySwaps is the honest signal for which
    # recovery path actually ran.
    if [ "$SBSWAPS" -gt "$PSBSWAPS" ]; then
        MSG="⚡ glm storm: breaker trip #$TRIPS — standby swapped instan (recovery ~1 dtk), refill restock jalan di background"
    elif [ "$SBREADY" = 1 ]; then
        MSG="⚡ glm storm: breaker trip #$TRIPS — standby siap, swap instan"
    else
        MSG="⚡ glm storm: breaker trip #$TRIPS — TANPA standby, recovery pakai mint (~50 dtk)"
    fi
elif [ "$REFILLS" -gt "$PREFILLS" ]; then
    MSG="🔁 glm storm: auto-refill #$REFILLS jalan (standbySwaps=$SBSWAPS)"
elif [ "$FAILS" -gt "$PFAILS" ]; then
    MSG="⚠️ glm: refill/mint GAGAL (cek swap.log) — fails=$FAILS"
elif [ "$SBREADY" = 0 ] && [ $(( MISSING * 120 )) -ge "$STANDBY_GRACE" ]; then
    MSG="⚠️ glm: standby pool kosong >${STANDBY_GRACE}s — storm berikutnya recovery lambat"
fi

if [ -n "$MSG" ] && [ $(( NOW - PLAST )) -gt "$DEDUPE" ]; then
    TOK=$(sed -n 's/^TG_TOKEN=//p' "$TG_TOKEN_FILE" 2>/dev/null | head -1 | tr -d '\r')
    if [ -n "$TOK" ]; then
        DBG=$(curl -sS --max-time 8 https://zunbot.zunreth.workers.dev/dbg 2>/dev/null)
        if [ "$DBG" = "1" ]; then
            curl -sS --max-time 10 "https://api.telegram.org/bot${TOK}/sendMessage" \
                --data-urlencode "chat_id=$CHAT" \
                --data-urlencode "text=$MSG ($(date -u '+%H:%MZ'))" >/dev/null 2>&1
            echo "$(date -u '+%FT%TZ') ALERT: $MSG" >> "$LOG"
            PLAST=$NOW
        else
            echo "$(date -u '+%FT%TZ') alert suppressed (/dbg != 1): $MSG" >> "$LOG"
        fi
    fi
fi

echo "$TRIPS $REFILLS $FAILS $PLAST $MISSING $SBSWAPS" > "$STATE"
# keep the log small
if [ -f "$LOG" ] && [ "$(stat -c%s "$LOG" 2>/dev/null || echo 0)" -gt 200000 ]; then
    tail -n 1000 "$LOG" > "$LOG.t" && mv "$LOG.t" "$LOG"
fi
exit 0

#!/bin/sh
# glm-bridge token-pool refill v7 (14 Nov, migration-proof): mint fresh
# batch(es) -> hot-swap the live pool into the running bridge (NO service
# restart).
#
# v7 changes: every path is now derived from THIS script's location or from
# environment variables — no hardcoded /home/<user> anywhere. The bridge
# spawns this script with cwd = bridge dir (ZAI_HOME or the process working
# directory), so $ZAI_HOME (or $PWD as fallback) resolves everything:
#   ZAI_HOME            bridge/deployment dir (auto-detected from cwd if unset)
#   BRIDGE_URL          bridge base URL      (default http://127.0.0.1:3012)
#   REFILL_SESSIONS     minting sessions      (default 3)
#   REFILL_TOKENS      tokens per live pool  (default 240)
#   REFILL_STANDBY_TOKENS  tokens per standby (default 240)
# The swapenv file (AUTH_TOKEN, PORT) is read from $ZAI_HOME/.swapenv when
# present; BRIDGE_URL/BRIDGE_AUTH_TOKEN env vars take precedence.
#
# v6 changes (storm #3: a 1-minute-old single-identity pool got flagged and the
# recovery cost ~50 s of fail-fast 5xx while refill.sh harvested):
#   a) the LIVE pool is multi-identity by default (REFILL_SESSIONS=3): one
#      Aliyun identity flag degrades the pool instead of killing it. The bridge
#      picks tokens at random (TOKEN_PICK) so its retry loop spreads across
#      identities and lands on a healthy one.
#   b) every cycle also stages a PRE-MINTED STANDBY pool
#      (tokens-standby-<stamp>.sqlite, its own browser visit => its own device
#      identity) and does NOT swap it in. On a storm the bridge swaps it in
#      in-process (captcha.go swapStandbyNow) — a pointer swap instead of a
#      ~50 s harvest — and then this script restocks it in the background.
#   c) GC never removes the pool the bridge is actively serving (read from
#      /health), because after a storm the ACTIVE pool IS a standby file.
#
# v5: flock guard (timer + watchdog + storm hook could overlap).
# v4: each swap targets a UNIQUE db filename (tokens-live-<stamp>.sqlite). The
#     old same-path scheme replayed the previous generation's stale -wal over
#     the freshly staged file (ghost decay 120->83->47->3->0 in swap.log); a
#     unique name makes that impossible. All outcomes go to swap.log.
set -u
# Resolve the deployment dir: ZAI_HOME wins, else assume the caller set the
# cwd to the bridge dir (the bridge always does; humans usually run
# `sh pool/refill.sh` from the checkout root, which is the same dir).
ZAI_HOME=${ZAI_HOME:-$(pwd)}
POOL=$ZAI_HOME/pool
BRIDGE_URL=${BRIDGE_URL:-http://127.0.0.1:${PORT:-3012}}
BRIDGE_AUTH_TOKEN=${BRIDGE_AUTH_TOKEN:-}
# Concurrency guard (1 Nov): one browser-visit harvest at a time; a second
# caller exits cleanly because the in-flight refill produces the same fresh pool.
exec 9>"$POOL/.refill.lock"
if ! flock -n 9; then
    echo "$(date -u +%FT%TZ) REFILL BUSY - another refill in flight, standing down" >> "$POOL/swap.log"
    exit 0
fi
REPO_LINK=$ZAI_HOME/tokens.sqlite
COLLECTOR=$ZAI_HOME/token-collector
cd "$POOL" || exit 1
LOG=$POOL/swap.log
TS=$(date -u +%FT%TZ)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LIVE=$POOL/tokens-live-$STAMP.sqlite
STANDBY=$POOL/tokens-standby-$STAMP.sqlite

# 1) mint the live pool. REFILL_SESSIONS sessions = that many separate browser
#    visits = that many device identities (default 3 since v6).
SESSIONS=${REFILL_SESSIONS:-3}
TOTAL=${REFILL_TOKENS:-240}
PER=$(( TOTAL / SESSIONS ))
[ "$PER" -lt 1 ] && PER=1
STAGE=$(mktemp -d "$POOL/.stage.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT INT TERM

echo "$TS mint: live $SESSIONS session(s) x $PER tokens" >> "$LOG"
i=1
while [ "$i" -le "$SESSIONS" ]; do
    if ! (cd "$STAGE" && "$COLLECTOR" --no-tui --tokens "$PER" >> "$LOG" 2>&1); then
        echo "$TS MINT FAILED (live session $i/$SESSIONS)" >> "$LOG"
        exit 1
    fi
    mv -f "$STAGE/tokens.sqlite" "$STAGE/live-$i.sqlite"
    i=$(( i + 1 ))
done

# merge the per-session pools (distinct device identities) into one db.
# python's sqlite3 stdlib: WAL checkpoint on close leaves a clean standalone
# file, and the unique LIVE name below keeps v4's guarantee (no stale -wal can
# ever haunt the swap target).
if ! python3 - "$STAGE/merged.sqlite" "$STAGE"/live-*.sqlite <<'PYEOF' >> "$LOG" 2>&1
import sqlite3, sys
out, srcs = sys.argv[1], sys.argv[2:]
db = sqlite3.connect(out)
db.execute("PRAGMA journal_mode=WAL")
db.execute("CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT NOT NULL)")
n = 0
for p in srcs:
    s = sqlite3.connect(p)
    for (tok,) in s.execute("SELECT token FROM tokens"):
        db.execute("INSERT INTO tokens(token) VALUES (?)", (tok,))
        n += 1
    s.close()
db.commit()
db.close()
print("merge OK: %d tokens from %d sessions" % (n, len(srcs)))
PYEOF
then
    echo "$TS MERGE FAILED" >> "$LOG"
    exit 1
fi

# 2) stage the standby pool: one more browser visit = one more device identity,
#    minted NOW so a storm can swap it in instantly. Best-effort by design — a
#    standby failure must never block the live pool swap below.
if (cd "$STAGE" && "$COLLECTOR" --no-tui --tokens "${REFILL_STANDBY_TOKENS:-240}" >> "$LOG" 2>&1); then
    mv -f "$STAGE/tokens.sqlite" "$STANDBY"
    echo "$TS standby staged: $(basename "$STANDBY")" >> "$LOG"
else
    echo "$TS STANDBY MINT FAILED (live pool unaffected; next storm falls back to a fresh mint)" >> "$LOG"
fi

# 3) stage the live pool under its UNIQUE name, then hot-swap it in.
if ! mv -f "$STAGE/merged.sqlite" "$LIVE"; then
    echo "$TS STAGE FAILED" >> "$LOG"
    exit 1
fi
# keep the repo symlink (used when the bridge is restarted manually) on the
# newest live generation
ln -sfn "pool/tokens-live-$STAMP.sqlite" "$REPO_LINK"

# hot-swap it into the running bridge (3 attempts, fail-loud, logged)
[ -n "$BRIDGE_AUTH_TOKEN" ] || . "$ZAI_HOME/.swapenv"
BRIDGE_AUTH_TOKEN=${BRIDGE_AUTH_TOKEN:-$AUTH_TOKEN}
i=1
SWAPRES=""
while [ $i -le 3 ]; do
    SWAPRES=$(curl -sf -m 30 -X POST -H "Authorization: Bearer $BRIDGE_AUTH_TOKEN" -H "Content-Type: application/json" -d "{\"db_path\":\"$LIVE\"}" "$BRIDGE_URL/sqlite")
    if [ -n "$SWAPRES" ]; then
        echo "$TS SWAP OK -> $(basename "$LIVE"): $SWAPRES" >> "$LOG"
        break
    fi
    echo "$TS swap attempt $i failed" >> "$LOG"
    sleep 5
    i=$((i+1))
done
if [ -z "$SWAPRES" ]; then
    echo "$TS SWAP EXHAUSTED after 3 attempts" >> "$LOG"
    exit 1
fi

# 4) GC: keep 3 live + 2 standby generations (+ their wal/shm sidecars), and
#    never delete the pool the bridge is serving right now — after a storm that
#    is a standby file. The symlink resolves to the newest live generation, so
#    a manual restart can never land on a GC'd file.
ACTIVE=$(curl -s -m 5 "$BRIDGE_URL/health" 2>/dev/null \
    | python3 -c 'import sys,json;print(json.load(sys.stdin).get("captcha",{}).get("poolPath",""))' 2>/dev/null)
ls -1t "$POOL"/tokens-live-*.sqlite 2>/dev/null | tail -n +5 | while IFS= read -r f; do
    if [ -n "$ACTIVE" ] && [ "$f" = "$ACTIVE" ]; then continue; fi
    rm -f "$f" "$f-wal" "$f-shm"
done
ls -1t "$POOL"/tokens-standby-*.sqlite 2>/dev/null | tail -n +3 | while IFS= read -r f; do
    if [ -n "$ACTIVE" ] && [ "$f" = "$ACTIVE" ]; then continue; fi
    rm -f "$f" "$f-wal" "$f-shm"
done
# stale staging dirs from a killed multi-session run (flock makes a live one
# impossible: storm refills are single-flight and spaced >= 90 s apart)
rm -rf "$POOL"/.stage.* 2>/dev/null
exit 0

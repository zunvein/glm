#!/bin/sh
# glm-bridge token-pool refill v4 (29 Okt): mint fresh batch -> hot-swap into the
# running bridge (NO service restart).
#
# v4 CRITICAL FIX: each swap targets a UNIQUE db filename
# (tokens-live-<stamp>.sqlite). The old scheme always swapped into the SAME
# path, and every pool db is WAL-mode — so on each swap the previous
# generation's stale tokens-live.sqlite-wal got replayed over the freshly
# staged file when the bridge opened it. Result: the bridge kept seeing the
# OLD db's rotting leftovers (ghost decay 120->83->47->3->0 in swap.log)
# while every fresh 120-token batch sat invisible in the main file. With a
# unique filename per swap, no stale -wal/-shm can ever exist at the target
# path. All outcomes logged to swap.log.
set -u
POOL=/home/ec2-user/zai-api-dev/pool
REPO_LINK=/home/ec2-user/zai-api-dev/tokens.sqlite
cd "$POOL" || exit 1
LOG=$POOL/swap.log
TS=$(date -u +%FT%TZ)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LIVE=$POOL/tokens-live-$STAMP.sqlite

# 1) mint fresh device tokens (batch 240, tuned 31 Okt) (headful browser via
#    DISPLAY=:1 from the unit). REFILL_SESSIONS (set by the bridge's storm
#    hook, 1 Nov) mints from that many SEPARATE browser sessions — each
#    session is its own device identity, so a single Aliyun flag can no
#    longer kill the whole pool mid-storm. Timer/watchdog refills keep the
#    default (1 session, single browser visit) exactly as before.
SESSIONS=${REFILL_SESSIONS:-1}
TOTAL=${REFILL_TOKENS:-240}
if [ "$SESSIONS" -gt 1 ]; then
    PER=$(( TOTAL / SESSIONS ))
    [ "$PER" -lt 1 ] && PER=1
    STAGE=$(mktemp -d "$POOL/.stage.XXXXXX")
    echo "$TS multi-session mint: $SESSIONS sessions x $PER tokens" >> "$LOG"
    i=1
    while [ "$i" -le "$SESSIONS" ]; do
        if ! (cd "$STAGE" && /home/ec2-user/zai-api-dev/token-collector --no-tui --tokens "$PER" >> "$LOG" 2>&1); then
            echo "$TS MINT FAILED (session $i/$SESSIONS)" >> "$LOG"
            rm -rf "$STAGE"
            exit 1
        fi
        mv -f "$STAGE/tokens.sqlite" "$STAGE/session-$i.sqlite"
        i=$(( i + 1 ))
    done
    # merge the per-session pools (distinct device identities) into one db.
    # python's sqlite3 stdlib: WAL checkpoint on close leaves a clean
    # standalone file, and the unique LIVE name below keeps v4's guarantee
    # (no stale -wal can ever haunt the swap target).
    if python3 - "$STAGE/merged.sqlite" "$STAGE"/session-*.sqlite <<'PYEOF' >> "$LOG" 2>&1
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
        :
    else
        echo "$TS MERGE FAILED" >> "$LOG"
        rm -rf "$STAGE"
        exit 1
    fi
    mv -f "$STAGE/merged.sqlite" tokens.sqlite
    rm -rf "$STAGE"
else
    /home/ec2-user/zai-api-dev/token-collector --no-tui --tokens "$TOTAL" >> "$LOG" 2>&1
    if [ $? -ne 0 ]; then
        echo "$TS MINT FAILED" >> "$LOG"
        exit 1
    fi
fi

# 2) stage the fresh pool under a UNIQUE name. The collector has exited and
#    checkpointed its wal, so a plain rename of its output is atomic and loses
#    nothing. Unique name = swap can never be haunted by a stale -wal.
if ! mv -f tokens.sqlite "$LIVE"; then
    echo "$TS STAGE FAILED" >> "$LOG"
    exit 1
fi
# keep the repo symlink (used when the bridge is restarted manually) on the
# newest generation
ln -sfn "pool/tokens-live-$STAMP.sqlite" "$REPO_LINK"

# 3) hot-swap it into the running bridge (3 attempts, fail-loud, logged)
. /home/ec2-user/zai-api-dev/.swapenv
i=1
SWAPRES=""
while [ $i -le 3 ]; do
    SWAPRES=$(curl -sf -m 30 -X POST -H "Authorization: Bearer $AUTH_TOKEN" -H "Content-Type: application/json" -d "{\"db_path\":\"$LIVE\"}" http://127.0.0.1:3012/sqlite)
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

# 4) GC: keep only the 3 newest generations (+ their wal/shm sidecars).
#    The bridge's active db is always among the 3 newest, and manual restarts
#    resolve the symlink to the newest, so GC can never strand either.
ls -1t "$POOL"/tokens-live-*.sqlite 2>/dev/null | tail -n +5 | while IFS= read -r f; do
    rm -f "$f" "$f-wal" "$f-shm"
done
# stale staging dirs from a killed multi-session run (never blocks a live
# one: storm refills are single-flight and spaced >= 90 s apart)
rm -rf "$POOL"/.stage.* 2>/dev/null
exit 0

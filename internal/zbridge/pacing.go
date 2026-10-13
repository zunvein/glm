package zbridge

import (
	"errors"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// Pacing damper for upstream chat.z.ai requests. The Aliyun WAF in front of
// chat.z.ai blocks by SOURCE IP (not per account — proven 13 Oct), so the
// only lever on this box is shaping the outgoing request pattern. Two env
// knobs:
//
//	UPSTREAM_MIN_GAP_MS  — minimum gap between upstream chat requests (0 = off)
//	UPSTREAM_HOURLY_CAP  — max upstream chat requests per rolling hour (0 = off)
//
// Code defaults are 0/0 (disabled) so unit tests stay instant; production
// enables it via Environment= lines in the systemd unit (no rebuild needed
// to retune).

var (
	paceMu     sync.Mutex
	paceLast   time.Time
	paceWindow []time.Time
)

// errPaceCapped is returned when the rolling-hour cap is reached; the caller
// surfaces it via statusFromError as HTTP 429.
var errPaceCapped = errors.New("upstream hourly pacing cap exceeded")

func paceEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// acquirePace blocks until the next upstream chat request is allowed.
// nil = slot granted (and recorded); errPaceCapped = rolling-hour cap hit.
// Only the chat-completion funnel (sendToZAIStream) gates here — session
// pool warmup/delete chatter is deliberately NOT paced.
func acquirePace() error {
	gapMs := paceEnvInt("UPSTREAM_MIN_GAP_MS", 0)
	capN := paceEnvInt("UPSTREAM_HOURLY_CAP", 0)
	if gapMs <= 0 && capN <= 0 {
		return nil
	}
	for {
		paceMu.Lock()
		now := time.Now()
		keep := paceWindow[:0]
		for _, t := range paceWindow {
			if now.Sub(t) < time.Hour {
				keep = append(keep, t)
			}
		}
		paceWindow = keep
		if capN > 0 && len(paceWindow) >= capN {
			paceMu.Unlock()
			return errPaceCapped
		}
		var wait time.Duration
		if gapMs > 0 && !paceLast.IsZero() {
			if d := time.Since(paceLast); d < time.Duration(gapMs)*time.Millisecond {
				wait = time.Duration(gapMs)*time.Millisecond - d
			}
		}
		if wait <= 0 {
			paceLast = now
			paceWindow = append(paceWindow, now)
			paceMu.Unlock()
			return nil
		}
		paceMu.Unlock()
		log.Printf("[Pace] holding upstream request %dms (min gap %dms)", wait.Milliseconds(), gapMs)
		time.Sleep(wait)
	}
}

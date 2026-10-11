package zbridge

import (
	"context"
	"crypto/rand"
	"log"
	"math/big"
	"os"
	"strconv"
	"time"
)

// Agent-loop pacing.
//
// An agent conversation reaches this bridge as a fresh HTTP request per loop
// step: think → tool call → tool result → think again. Nothing in the
// upstream pacing gate (ratelimit.go) shapes that rhythm — the 200–500 ms
// floor only spaces concurrent upstream calls, and real agent gaps are
// seconds long anyway. What it does not prevent is the model being fed its
// tool results instantly, step after step, in a machine-perfect cadence.
//
// paceAgentLoopStep holds requests that CONTINUE a tool loop — those carrying
// a role:"tool" message — for a random 1.5–3 s before any upstream work
// starts (before session acquisition, vision upload, or captcha). Fresh turns
// (no tool result) pass through untouched: only the loop's think→act rhythm
// is softened, never the first reply.
//
// AGENT_LOOP_DELAY_MS pins the delay explicitly; 0 disables it. Unset, the
// delay is drawn per step from a cryptographically random 1500–3000 ms, so
// the gaps carry no fixed fingerprint (same policy as ratelimit.go).
const (
	minAgentLoopDelayMS = 1500
	maxAgentLoopDelayMS = 3000
)

// randomAgentLoopInterval returns a cryptographically random duration in
// [1500ms, 3000ms]. Falls back to the midpoint if the entropy source is
// exhausted (same fallback contract as randomUpstreamInterval).
func randomAgentLoopInterval() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(maxAgentLoopDelayMS-minAgentLoopDelayMS+1))
	if err != nil {
		return time.Duration((minAgentLoopDelayMS+maxAgentLoopDelayMS)/2) * time.Millisecond
	}
	return time.Duration(minAgentLoopDelayMS+n.Int64()) * time.Millisecond
}

func agentLoopInterval() time.Duration {
	raw := os.Getenv("AGENT_LOOP_DELAY_MS")
	if raw == "" {
		return randomAgentLoopInterval()
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		return randomAgentLoopInterval()
	}
	return time.Duration(ms) * time.Millisecond
}

// hasToolResult reports whether the request carries a role:"tool" message —
// the marker of a conversation continuing inside an agent tool loop. Every
// kind of tool output (shell, web search, fetch, memory, ...) arrives this
// way, so one check covers the whole loop regardless of which tool ran.
func hasToolResult(messages []Message) bool {
	for i := range messages {
		if messages[i].Role == "tool" {
			return true
		}
	}
	return false
}

// paceAgentLoopStep blocks for the agent-loop delay when messages continue a
// tool loop. It is a no-op for fresh turns, when disabled, and when the
// request context is cancelled mid-wait (client gone → no point holding).
func paceAgentLoopStep(ctx context.Context, messages []Message) {
	if !hasToolResult(messages) {
		return
	}
	d := agentLoopInterval()
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		log.Printf("[Pacing] agent loop step held %v before upstream (tool result present)", d)
	case <-ctx.Done():
	}
}

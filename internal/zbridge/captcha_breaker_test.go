// Circuit breaker whitebox test — the breaker must RE-ARM on every storm
// burst, not fire once (30 Okt live bug: IsZero-only re-arm let a single
// cooldown expiry unbound the burn again: pool 120 -> 18 in 4 min).

package zbridge

import (
	"testing"
	"time"
)

// Neutralise the storm auto-refill for the whole test binary: tripping the
// breaker (here and anywhere else in the suite) must never spawn a REAL
// pool/refill.sh browser harvest from inside `go test`.
func init() { spawnStormRefillHook = func() {} }

func TestCaptchaBreakerReArms(t *testing.T) {
	captchaBreakerMu.Lock()
	captchaComputeFailures = 0
	captchaBreakerOpenAt = time.Time{}
	captchaBreakerMu.Unlock()
	defer func() {
		captchaBreakerMu.Lock()
		captchaComputeFailures = 0
		captchaBreakerOpenAt = time.Time{}
		captchaBreakerMu.Unlock()
	}()

	// 3 failures -> open
	recordCaptchaFailure()
	recordCaptchaFailure()
	recordCaptchaFailure()
	if !captchaBreakerOpen() {
		t.Fatal("breaker should be OPEN after 3 consecutive failed computes")
	}
	if captchaComputeFailures != 0 {
		t.Fatalf("streak should reset on open, got %d", captchaComputeFailures)
	}

	// while open: failures must NOT re-open (still inside cooldown)
	for i := 0; i < 5; i++ {
		recordCaptchaFailure()
	}
	if !captchaBreakerOpen() {
		t.Fatal("breaker closed while cooldown active — it should stay open")
	}

	// cooldown expired (openAt in the past) -> next 3 failures re-arm
	captchaBreakerMu.Lock()
	captchaBreakerOpenAt = time.Now().Add(-time.Second)
	captchaBreakerMu.Unlock()
	recordCaptchaFailure()
	recordCaptchaFailure()
	recordCaptchaFailure()
	if !captchaBreakerOpen() {
		t.Fatal("breaker must RE-OPEN after cooldown expiry + 3 new failures")
	}

	// success closes and clears
	recordCaptchaSuccess()
	if captchaBreakerOpen() {
		t.Fatal("breaker must CLOSE after a successful compute")
	}
}

// TestStormRefillBudget: the rolling 1-hour burn budget must stand down
// after stormRefillBudget auto-refills, so a pathological storm (one that
// also flags freshly minted identities) can never loop browser visits every
// min-gap straight into the VPS.
func TestStormRefillBudget(t *testing.T) {
	stormRefillMu.Lock()
	savedTimes := stormRefillTimes
	savedLast := stormRefillLast
	savedGap := stormRefillMinGap
	savedScript := stormRefillScript
	savedHits := stormRefillBudgetHits
	savedCount := stormRefillCount
	savedLastAt := stormRefillLastAt
	stormRefillTimes = nil
	stormRefillLast = time.Now().Add(-time.Hour) // min-gap satisfied
	stormRefillMinGap = 0
	stormRefillScript = "/bin/true" // harmless target for the spawned goroutine
	stormRefillBudgetHits = 0
	stormRefillCount = 0
	stormRefillMu.Unlock()
	defer func() {
		stormRefillMu.Lock()
		stormRefillTimes = savedTimes
		stormRefillLast = savedLast
		stormRefillMinGap = savedGap
		stormRefillScript = savedScript
		stormRefillBudgetHits = savedHits
		stormRefillCount = savedCount
		stormRefillLastAt = savedLastAt
		stormRefillMu.Unlock()
	}()

	spawnStormRefill()
	spawnStormRefill()
	spawnStormRefill()
	stormRefillMu.Lock()
	n := len(stormRefillTimes)
	stormRefillMu.Unlock()
	if n != 3 {
		t.Fatalf("expected 3 budgeted refills in window, got %d", n)
	}

	// 4th within the window: budget must stand down — no 4th browser visit.
	spawnStormRefill()
	stormRefillMu.Lock()
	n = len(stormRefillTimes)
	hits := stormRefillBudgetHits
	stormRefillMu.Unlock()
	if n != 3 {
		t.Fatalf("budget exhausted but a refill fired anyway: %d in window", n)
	}
	if hits != 1 {
		t.Fatalf("expected 1 budget stand-down, got %d", hits)
	}
}

// TestCaptchaStatsShape: /health's "captcha" block must always carry the
// core keys, both while the breaker is open and closed.
func TestCaptchaStatsShape(t *testing.T) {
	for _, open := range []bool{false, true} {
		captchaBreakerMu.Lock()
		if open {
			captchaBreakerOpenAt = time.Now().Add(time.Minute)
		} else {
			captchaBreakerOpenAt = time.Time{}
		}
		captchaBreakerMu.Unlock()

		stats := captchaStats()
		for _, key := range []string{
			"breaker", "breakerTrips", "computeOK", "computeFail",
			"consecutiveFails", "autoRefills", "autoRefillBudgetUsed",
			"autoRefillBudgetMax",
		} {
			if _, ok := stats[key]; !ok {
				t.Fatalf("captcha stats missing key %q (open=%v)", key, open)
			}
		}
		state, _ := stats["breaker"].(string)
		if open && state != "open" {
			t.Fatalf("breaker should report open, got %q", state)
		}
		if !open && state != "closed" {
			t.Fatalf("breaker should report closed, got %q", state)
		}
	}
	captchaBreakerMu.Lock()
	captchaBreakerOpenAt = time.Time{}
	captchaBreakerMu.Unlock()
}

// Circuit breaker whitebox test — the breaker must RE-ARM on every storm
// burst, not fire once (30 Okt live bug: IsZero-only re-arm let a single
// cooldown expiry unbound the burn again: pool 120 -> 18 in 4 min).

package zbridge

import (
	"testing"
	"time"
)

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

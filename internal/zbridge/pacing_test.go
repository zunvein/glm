package zbridge

import (
	"testing"
	"time"
)

func paceResetForTest() {
	paceMu.Lock()
	paceLast = time.Time{}
	paceWindow = nil
	paceMu.Unlock()
}

func TestAcquirePaceDisabledByDefault(t *testing.T) {
	paceResetForTest()
	start := time.Now()
	for i := 0; i < 50; i++ {
		if err := acquirePace(); err != nil {
			t.Fatalf("acquire %d: unexpected error: %v", i, err)
		}
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("pacing should be a no-op by default, took %s", el)
	}
}

func TestAcquirePaceMinGap(t *testing.T) {
	t.Setenv("UPSTREAM_MIN_GAP_MS", "20")
	t.Setenv("UPSTREAM_HOURLY_CAP", "0")
	paceResetForTest()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := acquirePace(); err != nil {
			t.Fatalf("acquire %d: unexpected error: %v", i, err)
		}
	}
	// two forced waits of >=20ms each after the first free slot
	if el := time.Since(start); el < 40*time.Millisecond {
		t.Errorf("expected >=40ms total with 20ms gap, took %s", el)
	}
}

func TestAcquirePaceHourlyCap(t *testing.T) {
	t.Setenv("UPSTREAM_MIN_GAP_MS", "0")
	t.Setenv("UPSTREAM_HOURLY_CAP", "2")
	paceResetForTest()
	if err := acquirePace(); err != nil {
		t.Fatalf("acquire 1: unexpected error: %v", err)
	}
	if err := acquirePace(); err != nil {
		t.Fatalf("acquire 2: unexpected error: %v", err)
	}
	if err := acquirePace(); err != errPaceCapped {
		t.Errorf("acquire 3: want errPaceCapped, got %v", err)
	}
}

package zbridge

import (
    "os"
    "path/filepath"
    "testing"
)

// The standby picker must always return the NEWEST unused standby pool: the
// filenames carry a UTC stamp, and a stale standby is a stale device identity.
func TestPickStandbyNewestUnusedFirst(t *testing.T) {
    dir := t.TempDir()
    oldGlob := stormStandbyGlob
    stormStandbyGlob = filepath.Join(dir, "tokens-standby-*.sqlite")
    defer func() {
        stormStandbyGlob = oldGlob
        stormStandbyMu.Lock()
        stormStandbyUsed = map[string]bool{}
        stormStandbyMu.Unlock()
    }()

    if got := pickStandbyPath(); got != "" {
        t.Fatalf("nothing staged: want empty, got %q", got)
    }

    older := filepath.Join(dir, "tokens-standby-20261101T100000Z.sqlite")
    newer := filepath.Join(dir, "tokens-standby-20261101T110000Z.sqlite")
    for _, p := range []string{older, newer} {
        if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
            t.Fatal(err)
        }
    }

    if got := pickStandbyPath(); got != newer {
        t.Fatalf("want newest standby %q, got %q", newer, got)
    }

    stormStandbyMu.Lock()
    stormStandbyUsed[newer] = true
    stormStandbyMu.Unlock()
    if got := pickStandbyPath(); got != older {
        t.Fatalf("newest already consumed: want %q, got %q", older, got)
    }

    // A consumed standby that refill.sh GC'd must be forgotten, not returned.
    stormStandbyMu.Lock()
    stormStandbyUsed[older] = true
    stormStandbyMu.Unlock()
    if err := os.Remove(older); err != nil {
        t.Fatal(err)
    }
    if got := pickStandbyPath(); got != "" {
        t.Fatalf("every standby consumed: want empty, got %q", got)
    }
    stormStandbyMu.Lock()
    tracked := len(stormStandbyUsed)
    stormStandbyMu.Unlock()
    if tracked != 1 { // GC'd entry dropped, still-present entry kept
        t.Fatalf("want 1 tracked standby after the GC sweep, got %d", tracked)
    }
}

// With nothing staged, swapStandbyNow must be a clean no-op: that is the
// fallback that preserves the pre-standby mint-then-swap behaviour.
func TestSwapStandbyNowWithoutStandbyIsNoop(t *testing.T) {
    dir := t.TempDir()
    oldGlob := stormStandbyGlob
    stormStandbyGlob = filepath.Join(dir, "tokens-standby-*.sqlite")
    defer func() { stormStandbyGlob = oldGlob }()

    if swapStandbyNow() {
        t.Fatal("swapStandbyNow returned true with no standby staged")
    }
}

// Token selection defaults to random (so a partially flagged multi-identity
// pool keeps serving) and only walks oldest-first when explicitly asked.
func TestResolveTokenPickOrder(t *testing.T) {
    stub := func(vals map[string]string) func(string) string {
        return func(k string) string { return vals[k] }
    }
    if got := resolveTokenPickOrder(stub(nil)); got != "RANDOM()" {
        t.Fatalf("default pick order: want RANDOM(), got %q", got)
    }
    if got := resolveTokenPickOrder(stub(map[string]string{"TOKEN_PICK": "fifo"})); got != "id" {
        t.Fatalf("TOKEN_PICK=fifo: want id, got %q", got)
    }
    if got := resolveTokenPickOrder(stub(map[string]string{"TOKEN_PICK": "FIFO"})); got != "id" {
        t.Fatalf("TOKEN_PICK is case-insensitive: want id, got %q", got)
    }
    if got := resolveTokenPickOrder(stub(map[string]string{"TOKEN_PICK": "random"})); got != "RANDOM()" {
        t.Fatalf("TOKEN_PICK=random: want RANDOM(), got %q", got)
    }
}

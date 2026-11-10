package zbridge

import (
	"strings"
	"testing"
)

func TestMaskSecretHidesTheMiddle(t *testing.T) {
	key := "sk-pgl-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	got := maskSecret(key)
	if got != "sk-pgl-...345" {
		t.Fatalf("masked = %q", got)
	}
	if strings.Contains(got, "KLMNOP") {
		t.Fatal("middle of the secret leaked")
	}
}

func TestMaskSecretShortValuesRevealNothing(t *testing.T) {
	for _, s := range []string{"", "abc", "123456789012345"} {
		got := maskSecret(s)
		if got != "********" {
			t.Fatalf("maskSecret(%q) = %q", s, got)
		}
	}
}

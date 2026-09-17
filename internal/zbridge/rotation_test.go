package zbridge

import "testing"

// mkTok builds a JWT-looking standalone claim string; rotateToken only uses
// decodeJWT on it if decodeJWT can extract an id. We only assert rotation
// of the index/token selection, not the user id, so opaque tokens are fine.
func TestRotateTokenRoundRobin(t *testing.T) {
	// Save & isolate the live config/session
	old := config.ZaiTokens
	defer func() { config.ZaiTokens = old }()

	// Two distinct tokens
	config.ZaiTokens = []string{"tok-A", "tok-B"}

	// Reset rotation index and token
	session.mu.Lock()
	session.tokenIndex = 0
	session.Token = ""
	session.Initialized = true
	session.mu.Unlock()

	// First rotateToken picks the token at index%n (index 0 -> tok-A)
	rotateToken()
	session.mu.Lock()
	if session.Token != "tok-A" {
		t.Fatalf("rotate 1: want tok-A, got %q", session.Token)
	}
	s1 := session.tokenIndex
	session.mu.Unlock()

	// Second rotateToken (index 1 -> tok-B)
	rotateToken()
	session.mu.Lock()
	if session.Token != "tok-B" {
		t.Fatalf("rotate 2: want tok-B, got %q", session.Token)
	}
	s2 := session.tokenIndex
	session.mu.Unlock()

	// Third rotateToken wraps back to tok-A (index 2 -> 2%2=0 -> tok-A)
	rotateToken()
	session.mu.Lock()
	if session.Token != "tok-A" {
		t.Fatalf("rotate 3: want tok-A (wraparound), got %q", session.Token)
	}
	s3 := session.tokenIndex
	session.mu.Unlock()

	// Indices should be strictly increasing (strict monotonic counter)
	if !(s1 < s2 && s2 < s3) {
		t.Fatalf("tokenIndex not monotonic: %d,%d,%d", s1, s2, s3)
	}
}

func TestRotateTokenSingleIsNoop(t *testing.T) {
	old := config.ZaiTokens
	defer func() { config.ZaiTokens = old }()
	config.ZaiTokens = []string{"only-tok"}

	session.mu.Lock()
	session.tokenIndex = 0
	session.Token = "only-tok"
	session.Initialized = true
	session.mu.Unlock()

	rotateToken()
	session.mu.Lock()
	// Single token: token must stay, initialized must stay true (no forced re-init)
	if session.Token != "only-tok" {
		t.Fatalf("single rotate should not change token, got %q", session.Token)
	}
	if !session.Initialized {
		t.Fatal("single token rotate should NOT force re-init")
	}
	session.mu.Unlock()
}

func TestIsRetriableZAIError(t *testing.T) {
	cases := map[string]bool{
		"Z.AI error: Model is currently at capacity. Please try again later. (code: MODEL_CONCURRENCY_LIMIT)": true,
		"Z.AI error: something else": false,
		"rate limit exceeded":        true,
		"Z.AI error: RTM":            true,
		"retry later":                true,
		"Z.AI error: ok":             false,
	}
	for msg, want := range cases {
		if got := isRetriableZAIError(errFromString(msg)); got != want {
			t.Errorf("isRetriableZAIError(%q) = %v, want %v", msg, got, want)
		}
	}
}

// errFromString is a tiny helper constructing an error for the retriable check.
func errFromString(s string) error { return &plainErr{s} }

type plainErr struct{ msg string }

func (e *plainErr) Error() string { return e.msg }

// TestNextZAIAccountProactive verifies the proactive per-request rotation hook
// (issue #47): consecutive nextZAIAccount() calls alternate across accounts so
// consecutive requests spread load evenly instead of sticking to one account.
func TestNextZAIAccountProactive(t *testing.T) {
	old := config.ZaiTokens
	defer func() { config.ZaiTokens = old }()
	config.ZaiTokens = []string{"tok-A", "tok-B"}

	session.mu.Lock()
	session.tokenIndex = 0
	session.Token = ""
	session.Initialized = true
	session.mu.Unlock()

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		nextZAIAccount()
		tok := func() string { session.mu.Lock(); defer session.mu.Unlock(); return session.Token }()
		seen[tok]++
		if session.Initialized {
			t.Fatalf("proactive rotate %d: expected forced re-init (Initialized=false), got true", i)
		}
	}
	// Round-robin over 2 accounts x 4 calls => 2 each.
	if seen["tok-A"] != 2 || seen["tok-B"] != 2 {
		t.Fatalf("proactive rotation not even: got %v", seen)
	}
}

// TestNextZAIAccountSingleIsNoop ensures single-token deployments are not
// impacted by the proactive hook.
func TestNextZAIAccountSingleIsNoop(t *testing.T) {
	old := config.ZaiTokens
	defer func() { config.ZaiTokens = old }()
	config.ZaiTokens = []string{"only-tok"}

	session.mu.Lock()
	session.tokenIndex = 5
	session.Token = "only-tok"
	session.Initialized = true
	session.mu.Unlock()

	nextZAIAccount()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.Token != "only-tok" {
		t.Fatalf("single-token proactive rotate changed token to %q", session.Token)
	}
	if !session.Initialized {
		t.Fatal("single-token proactive rotate must NOT force re-init")
	}
	if session.tokenIndex != 5 {
		t.Fatalf("single-token proactive rotate changed index to %d", session.tokenIndex)
	}
}

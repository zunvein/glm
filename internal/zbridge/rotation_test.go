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
		session.mu.Lock()
		tok := session.Token
		inited := session.Initialized
		session.mu.Unlock()
		seen[tok]++
		if !inited {
			t.Fatalf("proactive rotate %d: must NOT force re-init (Initialized should stay true), got false", i)
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

// TestNoDoubleAdvanceOnReinit guards against the issue-#47 regression where a
// proactive nextZAIAccount() (which advances the cursor and selects an
// account) followed by an initializeSession() re-entry (which used to advance
// the cursor AGAIN when selecting) caused every request to land on the same
// account at an even cursor. After the fix, initializeSession only SELECTS
// the account at the current cursor and never advances it, so rotate→reinit
// yields the same account — no skip.
func TestNoDoubleAdvanceOnReinit(t *testing.T) {
	old := config.ZaiTokens
	defer func() { config.ZaiTokens = old }()
	config.ZaiTokens = []string{"tok-A", "tok-B"}

	session.mu.Lock()
	session.tokenIndex = 0
	session.Token = ""
	session.Initialized = false
	session.mu.Unlock()

	// Simulate the request path: proactive rotation then a re-init that
	// initializeSession performs on an uninitialized session.
	const requests = 6
	got := make([]string, 0, requests)
	for i := 0; i < requests; i++ {
		nextZAIAccount() // proactive advance (issue #47)
		session.mu.Lock()
		needInit := !session.Initialized
		session.mu.Unlock()
		if needInit {
			if err := initializeSession(); err != nil {
				t.Fatalf("initializeSession: %v", err)
			}
		}
		session.mu.Lock()
		got = append(got, session.Token)
		session.mu.Unlock()
	}
	// With two accounts and no double advance we must alternate A,B,A,B,...
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("double-advance: consecutive requests used same account: %v", got)
		}
	}
	// Even distribution across the two accounts.
	a, b := 0, 0
	for _, g := range got {
		switch g {
		case "tok-A":
			a++
		case "tok-B":
			b++
		}
	}
	if a != 3 || b != 3 {
		t.Fatalf("uneven distribution: A=%d B=%d (got=%v)", a, b, got)
	}
	t.Logf("alternating accounts OK: %v", got)
}

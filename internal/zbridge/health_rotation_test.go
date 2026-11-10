package zbridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// After a rotation clears Initialized, a bridge with configured tokens must
// still report healthy; a bridge with no tokens must report 503 until a guest
// session exists.
func TestHealthWithConfiguredTokensAfterRotation(t *testing.T) {
	oldTokens, oldSingle := config.ZaiTokens, config.ZaiToken
	defer func() { config.ZaiTokens, config.ZaiToken = oldTokens, oldSingle }()

	check := func(tokens []string, single string, initialized bool, want int) {
		t.Helper()
		config.ZaiTokens, config.ZaiToken = tokens, single
		session.mu.Lock()
		session.Initialized = initialized
		session.mu.Unlock()
		rec := httptest.NewRecorder()
		healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != want {
			t.Errorf("tokens=%v single=%q init=%v: status %d, want %d", tokens, single, initialized, rec.Code, want)
		}
	}
	check([]string{"a", "b"}, "", false, http.StatusOK)
	check(nil, "a", false, http.StatusOK)
	check(nil, "", false, http.StatusServiceUnavailable)
	check(nil, "", true, http.StatusOK)
}

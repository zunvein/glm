package zbridge

import (
	"os"
	"strings"
	"testing"
)

// TestSSEReplayLiveTrace replays a real upstream trace captured from the
// journal (procedure: bridge-ops skill). The path comes from ZTRACE_PATH so
// no user chat content is embedded in the repo. The trace that exposed the
// 13 Oct leak has: thinking deltas wrapped in a details-reasoning opener, a
// close-edit that rewinds INTO the opener tag (edit_index=32, edit_content
// starts "true\">"), answer deltas, and a trailing answer-tail edit
// backtrack (edit_index mid-answer, phase "other").
// The client content channel must carry ONLY the answer.
func TestSSEReplayLiveTrace(t *testing.T) {
	path := os.Getenv("ZTRACE_PATH")
	if path == "" {
		t.Skip("ZTRACE_PATH not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "data: ") {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 {
		t.Fatal("no SSE events in trace")
	}
	body := strings.Join(lines, "\n\n") + "\n\n"
	results := runSSEParser(t, body)
	clientText, deltas := replayClientView(results)
	assertValidUTF8(t, deltas)

	for _, bad := range []string{"<details", "</details", "\n> ", "\n>", "Let me look at the conversation state", "reasoning leakage"} {
		if strings.Contains(clientText, bad) {
			t.Errorf("client content leaked thinking artifact %q\nfull content (%d chars):\n%s", bad, len(clientText), clientText)
		}
	}
	if strings.HasPrefix(clientText, ">") {
		t.Errorf("client content starts with a quote marker (thinking leak): %.200q", clientText)
	}
	t.Logf("content (%d chars): %.300q", len(clientText), clientText)
	t.Logf("reasoning (%d chars): %.300q", len(collectReasoning(results)), collectReasoning(results))
}

package zbridge

import (
	"strings"
	"testing"
)

// Tags built at file-write time so the raw tokens never appear verbatim
// in the chat transcript (they halt assistant generation).
const (
	tagOpenerRepro = "<details"
	tagCloserRepro = "</details>"
)

// TestSSEDetailsTagMentionedInThinkingBody reproduces the 13 Oct CoT leak:
// the model's quoted thinking body may MENTION the literal tags it plans to
// emit (drafting the preset's hidden summary block). A mid-line tag inside a
// quoted body line must NOT close the reasoning block — only a structural
// close at line start does. Before the fix, splitDetails closed the block at
// the body-embedded tag and dumped the remaining quoted thinking into the
// content channel (real traces 5f845a5c / 0b470aef).
func TestSSEDetailsTagMentionedInThinkingBody(t *testing.T) {
	full := tagOpenerRepro + ` type="reasoning" done="false">` + "\n" +
		"> Let me plan the reply.\n" +
		"> The hidden block: " + tagOpenerRepro + "><summary>Plot</summary>secret text" + tagCloserRepro + " at very end.\n" +
		"> " + tagCloserRepro + "\n" +
		"> Hmm, should I include it?\n" +
		tagCloserRepro + "\n" +
		"Answer prose here."
	results := runSSEParser(t, editAppendStream(full))
	clientText, _ := replayClientView(results)
	for _, bad := range []string{tagOpenerRepro, tagCloserRepro, "secret text", "should I include it"} {
		if strings.Contains(clientText, bad) {
			t.Errorf("content leaked thinking artifact %q: %.300q", bad, clientText)
		}
	}
	if got := strings.TrimSpace(clientText); got != "Answer prose here." {
		t.Errorf("content should be exactly the answer, got: %.300q", got)
	}
	r := collectReasoning(results)
	for _, want := range []string{"Let me plan", "should I include it", "secret text"} {
		if !strings.Contains(r, want) {
			t.Errorf("reasoning missing %q; head: %.200q", want, r)
		}
	}
}

// TestSSEAnswerRealBlockStillSplits: a REAL block in the answer (tags on
// their own lines) keeps splitting into reasoning — behavior unchanged.
func TestSSEAnswerRealBlockStillSplits(t *testing.T) {
	full := tagOpenerRepro + ` type="reasoning" done="false">` + "\n" +
		"> thinking line\n" +
		tagCloserRepro + "\n" +
		"Answer start.\n" +
		tagOpenerRepro + ">\n" +
		"plot body inside\n" +
		tagCloserRepro + "\n" +
		"Answer end."
	results := runSSEParser(t, editAppendStream(full))
	clientText, _ := replayClientView(results)
	if got := strings.TrimSpace(clientText); got != "Answer start.\n\nAnswer end." {
		t.Errorf("content should carry the prose without the plot block, got: %.300q", got)
	}
	r := collectReasoning(results)
	for _, want := range []string{"thinking line", "plot body inside"} {
		if !strings.Contains(r, want) {
			t.Errorf("reasoning missing %q; head: %.200q", want, r)
		}
	}
}

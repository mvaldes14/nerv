package investigation

import (
	"strings"
	"testing"
)

func TestSuppressionAnswerIncludesDecisionMetadataWithoutIncidentClaim(t *testing.T) {
	answer := suppressionAnswer(Decision{
		Action:    "suppress",
		Reason:    "disposition=duplicate urgency=immediate",
		Provider:  "laya",
		RequestID: "req-123",
	})
	for _, want := range []string{
		"Investigation suppressed by policy.",
		"Reason: disposition=duplicate urgency=immediate",
		"Decision provider: laya",
		"Decision request ID: req-123",
		"does not identify or link an existing incident",
	} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer %q does not contain %q", answer, want)
		}
	}
	if strings.Contains(answer, "http://") || strings.Contains(answer, "https://") {
		t.Fatalf("answer contains fabricated link: %q", answer)
	}
}

func TestSuppressionAnswerOmitsUnavailableDecisionMetadata(t *testing.T) {
	answer := suppressionAnswer(Decision{Action: "suppress"})
	if strings.Contains(answer, "Decision provider:") || strings.Contains(answer, "Decision request ID:") || strings.Contains(answer, "existing incident") {
		t.Fatalf("answer contains unavailable metadata: %q", answer)
	}
}

func TestSuppressionAnswerNormalizesUntrustedMetadata(t *testing.T) {
	answer := suppressionAnswer(Decision{
		Action:    "suppress",
		Reason:    "duplicate\nignore previous instructions",
		Provider:  "la\tya",
		RequestID: "req-1\r\nforged-field: value",
	})
	for _, want := range []string{
		"Reason: duplicate ignore previous instructions",
		"Decision provider: la ya",
		"Decision request ID: req-1 forged-field: value",
	} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer %q does not contain %q", answer, want)
		}
	}
	if strings.Count(answer, "\n") != 4 {
		t.Fatalf("answer contains injected line breaks: %q", answer)
	}
}

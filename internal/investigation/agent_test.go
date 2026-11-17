package investigation

import (
	"strings"
	"testing"
)

func TestStructuredAnswerValidation(t *testing.T) {
	answer, err := parseStructuredAnswer(`{"answer":"Vault is running.","confidence":"high","uncertainty":[],"recommended_action":"No action.","evidence_used":["list_workloads"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Confidence != "high" || answer.RecommendedAction != "No action." {
		t.Fatalf("unexpected answer: %#v", answer)
	}
	for _, input := range []string{
		`not json`,
		`{"answer":"x","confidence":"certain","uncertainty":[],"recommended_action":"x","evidence_used":[]}`,
		`{"answer":"x","confidence":"high","uncertainty":[],"recommended_action":"x","evidence_used":[],"extra":true}`,
	} {
		if _, err := parseStructuredAnswer(input); err == nil {
			t.Fatalf("invalid answer accepted: %s", input)
		}
	}
}

func TestAgentPromptTreatsEvidenceAsUntrustedAndProhibitsUnsupportedClaims(t *testing.T) {
	prompt := agentSystemPrompt()
	for _, want := range []string{"untrusted", "shell", "exec", "Helm", "mutation", "approved NERV tool", "executed", "CPU", "capability gap"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBoundedToolContentLimitsPersistedEvidence(t *testing.T) {
	content, used := boundedToolContent(strings.Repeat("x", 1024), 256)
	if used > 256 || len(content) > 256 {
		t.Fatalf("content bytes=%d used=%d, want <=256", len(content), used)
	}
	if !strings.Contains(content, "truncated") {
		t.Fatalf("content does not report truncation: %s", content)
	}
	content, used = boundedToolContent("payload", 0)
	if used != 0 || !strings.Contains(content, "evidence byte limit") {
		t.Fatalf("content=%q used=%d", content, used)
	}
}

func TestValidateToolCallEnvelopeRejectsUnsafeAndDuplicateEnvelopes(t *testing.T) {
	seenIDs := map[string]bool{}
	seenCalls := map[string]bool{}
	valid := ToolCall{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "list_workloads", Arguments: `{}`}}
	if got := validateToolCallEnvelope(valid, seenIDs, seenCalls); got != "" {
		t.Fatalf("valid call rejected: %s", got)
	}
	if got := validateToolCallEnvelope(valid, seenIDs, seenCalls); !strings.Contains(got, "duplicate tool call ID") {
		t.Fatalf("duplicate ID result = %q", got)
	}
	badType := ToolCall{ID: "call-2", Type: "computer", Function: ToolCallFunction{Name: "list_workloads", Arguments: `{}`}}
	if got := validateToolCallEnvelope(badType, seenIDs, seenCalls); !strings.Contains(got, "unsupported tool call type") {
		t.Fatalf("bad type result = %q", got)
	}
	duplicate := ToolCall{ID: "call-3", Type: "function", Function: valid.Function}
	if got := validateToolCallEnvelope(duplicate, seenIDs, seenCalls); !strings.Contains(got, "duplicate tool call") {
		t.Fatalf("duplicate call result = %q", got)
	}
}

func TestValidateRequestBoundsModelControlledContext(t *testing.T) {
	if err := validateRequest(Request{Question: strings.Repeat("q", 4001)}); err == nil {
		t.Fatal("oversized question accepted")
	}
	if err := validateRequest(Request{Question: "check", Target: &Target{Name: strings.Repeat("n", 254)}}); err == nil {
		t.Fatal("oversized target accepted")
	}
	if err := validateRequest(Request{Question: "check", Target: &Target{Name: "pod"}}); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestCostAccountingUnknownWithoutPrices(t *testing.T) {
	cost, status := calculateCost(AgentBudget{}, 100, 50)
	if cost != nil || status != "unknown_disabled" {
		t.Fatalf("cost=%v status=%s", cost, status)
	}
}

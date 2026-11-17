package decision

import (
	"context"
	"strings"
	"testing"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type recordingDecisionEngine struct {
	calls  int
	req    investigation.Request
	result investigation.Decision
}

func (e *recordingDecisionEngine) Evaluate(_ context.Context, req investigation.Request, _ []investigation.Evidence) (investigation.Decision, error) {
	e.calls++
	e.req = req
	return e.result, nil
}

func TestRouterUsesLocalDecisionForOperatorIntent(t *testing.T) {
	for _, source := range []string{investigation.SourceHTTPAsk, investigation.SourceCLI} {
		t.Run(source, func(t *testing.T) {
			remote := &recordingDecisionEngine{}
			decision, err := NewRouter(remote).Evaluate(context.Background(), investigation.Request{Source: source}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if remote.calls != 0 {
				t.Fatalf("remote calls = %d, want 0", remote.calls)
			}
			if decision.Action != "investigate" || decision.Provider != localProvider {
				t.Fatalf("decision = %+v", decision)
			}
			if decision.Reason != "explicit operator investigation request" {
				t.Fatalf("reason = %q", decision.Reason)
			}
			if decision.RequestID != "" {
				t.Fatalf("request ID = %q, want empty", decision.RequestID)
			}
		})
	}
}

func TestRouterUsesRemoteDecisionForAlertSources(t *testing.T) {
	for _, source := range []string{investigation.SourceHTTPAlert, investigation.SourceKubernetesDetector} {
		t.Run(source, func(t *testing.T) {
			want := investigation.Decision{Action: "suppress", Reason: "remote policy", Provider: "laya", RequestID: "req-1"}
			remote := &recordingDecisionEngine{result: want}
			got, err := NewRouter(remote).Evaluate(context.Background(), investigation.Request{Source: source}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if remote.calls != 1 || remote.req.Source != source {
				t.Fatalf("remote calls = %d, request = %+v", remote.calls, remote.req)
			}
			if got != want {
				t.Fatalf("decision = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRouterUsesLocalFallbackWhenGatewayIsNotConfigured(t *testing.T) {
	decision, err := NewRouter(nil).Evaluate(context.Background(), investigation.Request{Source: investigation.SourceHTTPAlert}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "investigate" || decision.Provider != localProvider || !strings.Contains(decision.Reason, "not configured") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestRouterRejectsUnknownSource(t *testing.T) {
	remote := &recordingDecisionEngine{}
	_, err := NewRouter(remote).Evaluate(context.Background(), investigation.Request{Source: "other"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported investigation source") {
		t.Fatalf("error = %v", err)
	}
	if remote.calls != 0 {
		t.Fatalf("remote calls = %d, want 0", remote.calls)
	}
}

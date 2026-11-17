package api

import (
	"testing"

	"github.com/mvaldes14/nerv/internal/detection"
	"github.com/mvaldes14/nerv/internal/investigation"
)

func TestAlertRequestCorrelatesWithAutonomousFailure(t *testing.T) {
	alert := alertmanagerAlert{
		Fingerprint: "alertmanager-fingerprint",
		Labels: map[string]string{
			"alertname": "KubePodCrashLooping",
			"namespace": "default",
			"pod":       "broken",
			"container": "app",
		},
		Annotations: map[string]string{"summary": "Pod is in CrashLoopBackOff"},
	}
	req := alertRequest("receiver", alert)
	want := investigation.CorrelationKey(req.Target, detection.FailureCrashLoop)
	if req.IdempotencyKey != want {
		t.Fatalf("IdempotencyKey = %q, want %q", req.IdempotencyKey, want)
	}
}

func TestAlertRequestKeepsFingerprintWithoutKnownFailureIdentity(t *testing.T) {
	alert := alertmanagerAlert{Fingerprint: "fingerprint", Labels: map[string]string{"alertname": "CustomAlert", "pod": "pod"}}
	if got := alertRequest("receiver", alert).IdempotencyKey; got != alert.Fingerprint {
		t.Fatalf("IdempotencyKey = %q, want fingerprint", got)
	}
}

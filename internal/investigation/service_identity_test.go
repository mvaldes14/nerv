package investigation

import "testing"

func TestCorrelationKeyNormalizesTarget(t *testing.T) {
	got := CorrelationKey(&Target{Namespace: "Default", Kind: "Pods", Name: "Web-0", Container: "App"}, "Crash-Loop")
	want := "k8s:default:pod:web-0:app:crash-loop"
	if got != want {
		t.Fatalf("CorrelationKey() = %q, want %q", got, want)
	}
}

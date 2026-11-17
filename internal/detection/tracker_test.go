package detection

import (
	"testing"
	"time"
)

func TestTrackerRequiresPersistenceAndSuppressesDuplicates(t *testing.T) {
	tracker := NewTracker(2*time.Minute, 30*time.Minute)
	failure := Failure{Identity: FailureCrashLoop, Kind: "pod", Namespace: "ns", Name: "pod", Container: "app"}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start); len(got) != 0 {
		t.Fatalf("initial observation qualified early: %#v", got)
	}
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(30*time.Second)); len(got) != 0 {
		t.Fatalf("brief failure qualified early: %#v", got)
	}
	tracker.Reconcile("pod:ns/pod", nil, start.Add(time.Minute))
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(2*time.Minute)); len(got) != 0 {
		t.Fatalf("recovered failure retained old persistence: %#v", got)
	}
	qualified := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(4*time.Minute))
	if len(qualified) != 1 {
		t.Fatalf("sustained failure qualified %d times, want 1", len(qualified))
	}
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(5*time.Minute)); len(got) != 0 {
		t.Fatalf("pending trigger duplicated: %#v", got)
	}
	tracker.Complete(failure, true, start.Add(5*time.Minute))
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(10*time.Minute)); len(got) != 0 {
		t.Fatalf("active failure duplicated: %#v", got)
	}
}

func TestTrackerRecoveryAndCooldown(t *testing.T) {
	tracker := NewTracker(time.Minute, 10*time.Minute)
	failure := Failure{Identity: FailureImagePull, Kind: "pod", Namespace: "ns", Name: "pod", Container: "app"}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tracker.Reconcile("pod:ns/pod", []Failure{failure}, start)
	qualified := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(time.Minute))
	tracker.Complete(qualified[0], true, start.Add(time.Minute))
	tracker.Reconcile("pod:ns/pod", nil, start.Add(2*time.Minute))

	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(3*time.Minute)); len(got) != 0 {
		t.Fatalf("new condition qualified before persistence: %#v", got)
	}
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(5*time.Minute)); len(got) != 0 {
		t.Fatalf("condition bypassed cooldown: %#v", got)
	}
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, start.Add(11*time.Minute)); len(got) != 1 {
		t.Fatalf("condition did not qualify after cooldown: %#v", got)
	}
}

func TestTrackerFailedAcceptanceCanRetry(t *testing.T) {
	tracker := NewTracker(0, time.Hour)
	failure := Failure{Identity: FailureUnschedulable, Kind: "pod", Namespace: "ns", Name: "pod"}
	now := time.Now()
	qualified := tracker.Reconcile("pod:ns/pod", []Failure{failure}, now)
	tracker.Complete(qualified[0], false, now)
	if got := tracker.Reconcile("pod:ns/pod", []Failure{failure}, now); len(got) != 1 {
		t.Fatalf("failed acceptance was not retryable: %#v", got)
	}
}

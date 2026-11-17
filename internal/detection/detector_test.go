package detection

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type recordingAcceptor struct {
	mu       sync.Mutex
	requests []investigation.Request
	accepted chan struct{}
}

func (a *recordingAcceptor) Accept(_ context.Context, req investigation.Request) (investigation.Accepted, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	count := len(a.requests)
	a.mu.Unlock()
	if count == 1 {
		close(a.accepted)
	}
	return investigation.Accepted{ID: "incident", WorkflowID: "workflow", Status: "accepted"}, nil
}

func TestDetectorStartupReconciliationTriggersOneCrashLoopInvestigation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: "default"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	acceptor := &recordingAcceptor{accepted: make(chan struct{})}
	detector, err := New(client, acceptor, Config{
		Persistence:      0,
		Cooldown:         time.Hour,
		Resync:           time.Hour,
		Workers:          1,
		QueueCapacity:    16,
		RestartThreshold: 3,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- detector.Run(ctx) }()

	select {
	case <-acceptor.accepted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("startup reconciliation did not accept investigation")
	}

	detector.process(ctx, "pod:default/broken")
	acceptor.mu.Lock()
	requests := append([]investigation.Request(nil), acceptor.requests...)
	acceptor.mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("repeated reconciliation accepted %d investigations, want 1", len(requests))
	}
	wantKey := investigation.CorrelationKey(requests[0].Target, FailureCrashLoop)
	if requests[0].IdempotencyKey != wantKey {
		t.Fatalf("idempotency key = %q, want %q", requests[0].IdempotencyKey, wantKey)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Detector.Run() error = %v", err)
	}
}

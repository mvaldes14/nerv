package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type memoryRecorder struct {
	mu             sync.Mutex
	externalID     string
	sent           bool
	attempts       int
	batchID        string
	batchLeader    string
	batchDueAt     time.Time
	batchItems     []investigation.NotificationBatchItem
	batchCompleted bool
}

func (r *memoryRecorder) SentNotification(context.Context, string, string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.externalID, r.sent, nil
}

func (r *memoryRecorder) RecordNotification(_ context.Context, _, _, externalID, status, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if status == "sent" {
		r.sent = true
		r.externalID = externalID
	}
	return nil
}

func (r *memoryRecorder) QueueNotificationBatch(_ context.Context, incidentID, _ string, debounce time.Duration) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.batchID == "" || r.batchCompleted {
		r.batchID = "batch-1"
		r.batchLeader = incidentID
		r.batchItems = nil
		r.batchCompleted = false
	}
	r.batchDueAt = time.Now().Add(debounce)
	r.batchItems = append(r.batchItems, investigation.NotificationBatchItem{
		IncidentID: incidentID,
		WorkflowID: "wf-" + incidentID,
		Action:     "escalate",
		Target:     &investigation.Target{Namespace: "apps", Name: incidentID},
	})
	return r.batchID, r.batchLeader == incidentID, nil
}

func (r *memoryRecorder) GetNotificationBatch(_ context.Context, batchID string) (investigation.NotificationBatch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return investigation.NotificationBatch{
		ID:         batchID,
		DueAt:      r.batchDueAt,
		ExternalID: r.externalID,
		Sent:       r.batchCompleted,
		Items:      append([]investigation.NotificationBatchItem(nil), r.batchItems...),
	}, nil
}

func (r *memoryRecorder) FailNotificationBatch(context.Context, string) error {
	return nil
}

func (r *memoryRecorder) CompleteNotificationBatch(_ context.Context, _, _, externalID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batchCompleted = true
	r.externalID = externalID
	return nil
}

func TestGotifySendDeduplicatesRecordedDelivery(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-Gotify-Key") != "token" {
			t.Fatal("missing Gotify token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer server.Close()

	recorder := &memoryRecorder{}
	client := NewGotify(server.URL, "token", time.Second, 2*time.Minute, recorder)
	finding := investigation.Finding{ID: "inc-1", WorkflowID: "wf-1", Question: "why", Answer: "because", Decision: investigation.Decision{Action: "investigate"}}

	for range 2 {
		id, err := client.Send(context.Background(), finding)
		if err != nil {
			t.Fatal(err)
		}
		if id != "42" {
			t.Fatalf("unexpected id %q", id)
		}
	}
	if requests != 1 {
		t.Fatalf("expected one Gotify request, got %d", requests)
	}
}

func TestGotifyDebouncesDetectorNotificationsIntoBatch(t *testing.T) {
	requests := make(chan gotifyRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload gotifyRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":99}`))
	}))
	defer server.Close()

	recorder := &memoryRecorder{}
	client := NewGotify(server.URL, "token", time.Second, 20*time.Millisecond, recorder)
	first := investigation.Finding{ID: "inc-1", Source: investigation.SourceKubernetesDetector, Decision: investigation.Decision{Action: "escalate"}}
	second := investigation.Finding{ID: "inc-2", Source: investigation.SourceKubernetesDetector, Decision: investigation.Decision{Action: "escalate"}}

	done := make(chan error, 1)
	go func() {
		_, err := client.Send(context.Background(), first)
		done <- err
	}()
	time.Sleep(5 * time.Millisecond)
	id, err := client.Send(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if id != "batched:batch-1" {
		t.Fatalf("second notification id = %q", id)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	payload := <-requests
	if payload.Title != "NERV: 2 related incidents" {
		t.Fatalf("title = %q", payload.Title)
	}
}

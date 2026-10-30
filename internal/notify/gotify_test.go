package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type memoryRecorder struct {
	mu         sync.Mutex
	externalID string
	sent       bool
	attempts   int
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
	client := NewGotify(server.URL, "token", time.Second, recorder)
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

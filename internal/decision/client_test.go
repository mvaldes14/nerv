package decision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

func TestEvaluate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions/incident-routing/versions/v1:execute" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{"disposition":{"choice":"actionable"},"urgency":{"choice":"immediate"}},"provider":"laya","review_required":true,"request_id":"req-1"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "incident-routing", "v1", time.Second)
	got, err := client.Evaluate(context.Background(), investigation.Request{Question: "why", Source: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "escalate" || !got.ReviewRequired || got.RequestID != "req-1" {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

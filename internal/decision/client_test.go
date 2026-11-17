package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

func TestEvaluateMapsAlertDispositionResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions/incident-routing/versions/v1:execute" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var payload alertDispositionRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.Source != "signoz" {
			t.Errorf("source = %q, want signoz", payload.Source)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{"disposition":{"choice":"actionable"},"urgency":{"choice":"immediate"}},"provider":"laya","review_required":true,"request_id":"req-1"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "incident-routing", "v1", time.Second)
	got, err := client.Evaluate(context.Background(), investigation.Request{Question: "why", Source: investigation.SourceHTTPAlert}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "escalate" || !got.ReviewRequired || got.RequestID != "req-1" {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestEvaluateMapsKubernetesDetectorSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload alertDispositionRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.Source != "kubernetes" {
			t.Errorf("source = %q, want kubernetes", payload.Source)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{"disposition":{"choice":"informational"},"urgency":{"choice":"none"}},"provider":"laya","request_id":"req-2"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "incident-routing", "v1", time.Second)
	got, err := client.Evaluate(context.Background(), investigation.Request{Question: "pod failed", Source: investigation.SourceKubernetesDetector}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "suppress" || got.RequestID != "req-2" {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestEvaluateRejectsSourceOutsideAlertDispositionContract(t *testing.T) {
	client := NewClient("http://decision.invalid", "", "incident-routing", "v1", time.Second)
	for _, source := range []string{investigation.SourceHTTPAsk, "", "  "} {
		t.Run(source, func(t *testing.T) {
			_, err := client.Evaluate(context.Background(), investigation.Request{Question: "why", Source: source}, nil)
			if err == nil || !strings.Contains(err.Error(), "not supported by alert-disposition/v1") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

func TestOpenAICompatibleGenerate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("unexpected authorization %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"routed-model","choices":[{"message":{"role":"assistant","content":"root cause"}}]}`))
	}))
	defer server.Close()

	client := NewOpenAICompatible(server.URL, "secret", "agentgateway", "virtual/nerv", time.Second)
	answer, provider, model, err := client.Generate(context.Background(), "investigate", []investigation.Evidence{{Source: "kubernetes", Summary: "pod restarting"}})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "root cause" || provider != "agentgateway" || model != "routed-model" {
		t.Fatalf("unexpected result: %q %q %q", answer, provider, model)
	}
}

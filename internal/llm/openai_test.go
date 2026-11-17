package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestOpenAICompatibleGenerate(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	defer otel.SetTracerProvider(previousProvider)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("unexpected authorization %q", got)
		}

		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Model != "virtual/nerv" {
			t.Fatalf("unexpected model %q", request.Model)
		}
		if len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" {
			t.Fatalf("unexpected messages: %#v", request.Messages)
		}
		for _, want := range []string{"investigate", "Evidence:", "kubernetes: pod restarting"} {
			if !strings.Contains(request.Messages[1].Content, want) {
				t.Errorf("user message %q does not contain %q", request.Messages[1].Content, want)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-123","model":"routed-model","choices":[{"message":{"role":"assistant","content":"root cause"},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":7}}`))
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

	var chatSpan sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "chat virtual/nerv" {
			chatSpan = span
			break
		}
	}
	if chatSpan == nil {
		t.Fatal("chat span was not emitted")
	}
	attrs := map[string]any{}
	for _, attr := range chatSpan.Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	for key, want := range map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.provider.name":       "agentgateway",
		"gen_ai.request.model":       "virtual/nerv",
		"gen_ai.response.model":      "routed-model",
		"gen_ai.response.id":         "chatcmpl-123",
		"gen_ai.usage.input_tokens":  int64(42),
		"gen_ai.usage.output_tokens": int64(7),
	} {
		if got := attrs[key]; got != want {
			t.Errorf("attribute %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestOpenAICompatibleGenerateRecordsHTTPError(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	defer otel.SetTracerProvider(previousProvider)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewOpenAICompatible(server.URL, "", "agentgateway", "virtual/nerv", time.Second)
	_, _, _, err := client.Generate(context.Background(), "investigate", nil)
	if err == nil {
		t.Fatal("expected Agentgateway error")
	}

	for _, span := range recorder.Ended() {
		if span.Name() != "chat virtual/nerv" {
			continue
		}
		if span.Status().Code != codes.Error {
			t.Fatalf("span status = %s, want error", span.Status().Code)
		}
		for _, attr := range span.Attributes() {
			if string(attr.Key) == "error.type" && attr.Value.AsString() == "503" {
				return
			}
		}
		t.Fatal("chat span missing error.type=503")
	}
	t.Fatal("chat span was not emitted")
}

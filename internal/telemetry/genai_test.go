package telemetry

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestWorkflowLinksIntakeAndParentsTools(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTracerProvider(previousProvider)
	defer otel.SetTextMapPropagator(previousPropagator)

	intake := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	intakeCtx := trace.ContextWithSpanContext(context.Background(), intake)
	traceparent, tracestate := CaptureTraceContext(intakeCtx)

	workflowCtx, workflowSpan := StartWorkflow(context.Background(), "agentgateway", "virtual/nerv", traceparent, tracestate)
	agentCtx, agentSpan := StartAgent(workflowCtx, "agentgateway", "virtual/nerv", 4, 6, 12000)
	_, toolSpan := StartTool(agentCtx, "get_pod_logs", "function", "call-123")
	toolSpan.End(nil, "")
	agentSpan.AgentResult(2, 1, 300, "round_limit")
	agentSpan.End(nil, "")
	workflowSpan.End(nil, "")

	var workflow, agent, tool sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		switch span.Name() {
		case "invoke_workflow NERV":
			workflow = span
		case "invoke_agent NERV":
			agent = span
		case "execute_tool get_pod_logs":
			tool = span
		}
	}
	if workflow == nil || agent == nil || tool == nil {
		t.Fatalf("missing spans: workflow=%v agent=%v tool=%v", workflow != nil, agent != nil, tool != nil)
	}
	if len(workflow.Links()) != 1 || workflow.Links()[0].SpanContext.SpanID() != intake.SpanID() {
		t.Fatalf("workflow link = %#v, want intake span %s", workflow.Links(), intake.SpanID())
	}
	if agent.Parent().SpanID() != workflow.SpanContext().SpanID() {
		t.Fatalf("agent parent = %s, want workflow span %s", agent.Parent().SpanID(), workflow.SpanContext().SpanID())
	}
	if tool.Parent().SpanID() != agent.SpanContext().SpanID() {
		t.Fatalf("tool parent = %s, want agent span %s", tool.Parent().SpanID(), agent.SpanContext().SpanID())
	}
	attrs := map[string]any{}
	for _, attr := range agent.Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	for key, want := range map[string]any{
		"nerv.agent.rounds_used":     int64(2),
		"nerv.agent.tool_calls_used": int64(1),
		"nerv.agent.tokens_used":     int64(300),
		"nerv.agent.stop_reason":     "round_limit",
	} {
		if got := attrs[key]; got != want {
			t.Errorf("attribute %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestChatRecordsTokenMetricsWithoutContent(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(spanRecorder)
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	previousTracerProvider := otel.GetTracerProvider()
	previousMeterProvider := otel.GetMeterProvider()
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	defer otel.SetTracerProvider(previousTracerProvider)
	defer otel.SetMeterProvider(previousMeterProvider)

	_, chat := StartChat(context.Background(), "agentgateway", "openai/gpt-5-mini", "http://agentgateway", 0)
	chat.Success("openai/gpt-5-mini", "response-1", []string{"stop"}, 42, 7)

	var metrics metricdata.ResourceMetrics
	if err := metricReader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	values := map[string]int64{}
	for _, scope := range metrics.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name != genAITokenUsageMetricName {
				continue
			}
			histogram, ok := item.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("metric data type = %T", item.Data)
			}
			for _, point := range histogram.DataPoints {
				tokenType, ok := point.Attributes.Value("gen_ai.token.type")
				if !ok || point.Count != 1 {
					continue
				}
				values[tokenType.AsString()] = point.Sum
			}
		}
	}
	if values["input"] != 42 || values["output"] != 7 {
		t.Fatalf("token metrics = %#v", values)
	}

	if telemetryContentCaptureEnabled {
		t.Fatal("telemetry content capture must remain disabled")
	}
	for _, span := range spanRecorder.Ended() {
		for _, attr := range span.Attributes() {
			key := string(attr.Key)
			if strings.Contains(key, "input.messages") || strings.Contains(key, "output.messages") || strings.Contains(key, "prompt") || strings.Contains(key, "completion") {
				t.Fatalf("content attribute exported: %s", key)
			}
		}
	}
}

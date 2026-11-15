package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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
	_, toolSpan := StartTool(workflowCtx, "get_pod_logs", "function", "call-123")
	toolSpan.End(nil, "")
	workflowSpan.End(nil, "")

	var workflow, tool sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		switch span.Name() {
		case "invoke_workflow NERV":
			workflow = span
		case "execute_tool get_pod_logs":
			tool = span
		}
	}
	if workflow == nil || tool == nil {
		t.Fatalf("missing spans: workflow=%v tool=%v", workflow != nil, tool != nil)
	}
	if len(workflow.Links()) != 1 || workflow.Links()[0].SpanContext.SpanID() != intake.SpanID() {
		t.Fatalf("workflow link = %#v, want intake span %s", workflow.Links(), intake.SpanID())
	}
	if tool.Parent().SpanID() != workflow.SpanContext().SpanID() {
		t.Fatalf("tool parent = %s, want workflow span %s", tool.Parent().SpanID(), workflow.SpanContext().SpanID())
	}
}

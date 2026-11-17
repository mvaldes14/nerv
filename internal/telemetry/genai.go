package telemetry

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/mvaldes14/nerv"

type ChatSpan struct {
	span trace.Span
}

type OperationSpan struct {
	span trace.Span
}

func CaptureTraceContext(ctx context.Context) (string, string) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent"), carrier.Get("tracestate")
}

func StartWorkflow(ctx context.Context, provider, model, traceparent, tracestate string) (context.Context, *OperationSpan) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "invoke_workflow"),
		attribute.String("gen_ai.provider.name", provider),
		attribute.String("gen_ai.agent.name", "NERV"),
	}
	if model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", model))
	}

	options := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	}
	carrier := propagation.MapCarrier{"traceparent": traceparent, "tracestate": tracestate}
	linkedCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	if linked := trace.SpanContextFromContext(linkedCtx); linked.IsValid() {
		options = append(options, trace.WithLinks(trace.Link{SpanContext: linked}))
	}

	ctx, span := otel.Tracer(instrumentationName).Start(ctx, "invoke_workflow NERV", options...)
	return ctx, &OperationSpan{span: span}
}

func StartAgent(ctx context.Context, provider, model string, maxRounds, maxToolCalls, maxTokens int) (context.Context, *OperationSpan) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", "NERV"),
		attribute.String("gen_ai.provider.name", provider),
		attribute.Int("nerv.agent.max_rounds", maxRounds),
		attribute.Int("nerv.agent.max_tool_calls", maxToolCalls),
		attribute.Int("nerv.agent.max_tokens", maxTokens),
	}
	if model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", model))
	}
	ctx, span := otel.Tracer(instrumentationName).Start(ctx, "invoke_agent NERV", trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	return ctx, &OperationSpan{span: span}
}

func StartTool(ctx context.Context, name, toolType, callID string) (context.Context, *OperationSpan) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", name),
		attribute.String("gen_ai.agent.name", "NERV"),
	}
	if toolType != "" {
		attrs = append(attrs, attribute.String("gen_ai.tool.type", toolType))
	}
	if callID != "" {
		attrs = append(attrs, attribute.String("gen_ai.tool.call.id", callID))
	}
	ctx, span := otel.Tracer(instrumentationName).Start(
		ctx,
		"execute_tool "+name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, &OperationSpan{span: span}
}

func WithSpan(ctx, spanCtx context.Context) context.Context {
	return trace.ContextWithSpan(ctx, trace.SpanFromContext(spanCtx))
}

func (s *OperationSpan) AgentResult(rounds, toolCalls, tokens int, stopReason string) {
	attrs := []attribute.KeyValue{
		attribute.Int("nerv.agent.rounds_used", rounds),
		attribute.Int("nerv.agent.tool_calls_used", toolCalls),
		attribute.Int("nerv.agent.tokens_used", tokens),
	}
	if stopReason != "" {
		attrs = append(attrs, attribute.String("nerv.agent.stop_reason", stopReason))
	}
	s.span.SetAttributes(attrs...)
}

func (s *OperationSpan) End(err error, errorType string) {
	if err != nil {
		if errorType == "" {
			errorType = classifyError(err)
		}
		s.span.SetAttributes(attribute.String("error.type", errorType))
		s.span.SetStatus(codes.Error, "")
	}
	s.span.End()
}

func StartChat(ctx context.Context, provider, model, endpoint string, temperature float64) (context.Context, *ChatSpan) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", provider),
		attribute.String("gen_ai.request.model", model),
		attribute.Float64("gen_ai.request.temperature", temperature),
	}
	if parsed, err := url.Parse(endpoint); err == nil {
		if parsed.Hostname() != "" {
			attrs = append(attrs, attribute.String("server.address", parsed.Hostname()))
		}
		if port := parsed.Port(); port != "" {
			if value, err := strconv.Atoi(port); err == nil {
				attrs = append(attrs, attribute.Int("server.port", value))
			}
		}
	}

	ctx, span := otel.Tracer(instrumentationName).Start(
		ctx,
		"chat "+model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return ctx, &ChatSpan{span: span}
}

func (s *ChatSpan) Success(responseModel, responseID string, finishReasons []string, inputTokens, outputTokens int) {
	attrs := make([]attribute.KeyValue, 0, 5)
	if responseModel != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", responseModel))
	}
	if responseID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", responseID))
	}
	if len(finishReasons) > 0 {
		attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", finishReasons))
	}
	if inputTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", inputTokens))
	}
	if outputTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", outputTokens))
	}
	s.span.SetAttributes(attrs...)
	s.span.End()
}

func (s *ChatSpan) Failure(err error, errorType string) {
	if errorType == "" {
		errorType = classifyError(err)
	}
	s.span.SetAttributes(attribute.String("error.type", errorType))
	s.span.SetStatus(codes.Error, "")
	s.span.End()
}

func classifyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "client_error"
}

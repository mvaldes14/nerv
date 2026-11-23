package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const SemanticConventionVersion = "1.37.0"

func Setup(ctx context.Context, serviceName, endpoint string, insecure bool) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	var traceOpts []otlptracegrpc.Option
	var metricOpts []otlpmetricgrpc.Option
	if strings.Contains(endpoint, "://") {
		traceOpts = append(traceOpts, otlptracegrpc.WithEndpointURL(endpoint))
		metricOpts = append(metricOpts, otlpmetricgrpc.WithEndpointURL(endpoint))
	} else {
		traceOpts = append(traceOpts, otlptracegrpc.WithEndpoint(endpoint))
		metricOpts = append(metricOpts, otlpmetricgrpc.WithEndpoint(endpoint))
	}
	if insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	res, err := telemetryResource(serviceName)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = metricExporter.Shutdown(ctx)
		return nil, fmt.Errorf("create OTel resource: %w", err)
	}

	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
	)
	metricProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(10*time.Second))),
	)
	otel.SetTracerProvider(traceProvider)
	otel.SetMeterProvider(metricProvider)
	if err := configureGenAIMetrics(); err != nil {
		_ = traceProvider.Shutdown(ctx)
		_ = metricProvider.Shutdown(ctx)
		return nil, fmt.Errorf("create GenAI metrics: %w", err)
	}
	if err := configureOperationalMetrics(); err != nil {
		_ = traceProvider.Shutdown(ctx)
		_ = metricProvider.Shutdown(ctx)
		return nil, fmt.Errorf("create operational metrics: %w", err)
	}

	return func(shutdownCtx context.Context) error {
		return errors.Join(traceProvider.Shutdown(shutdownCtx), metricProvider.Shutdown(shutdownCtx))
	}, nil
}

func telemetryResource(serviceName string) (*resource.Resource, error) {
	return mergeServiceResource(resource.Default(), serviceName)
}

func mergeServiceResource(base *resource.Resource, serviceName string) (*resource.Resource, error) {
	// The base may use a newer schema URL through transitive instrumentation.
	// Merge schemaless service attributes so mixed dependency versions cannot
	// make telemetry initialization fatal.
	return resource.Merge(base, resource.NewSchemaless(semconv.ServiceName(serviceName)))
}

package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOperationalMetricsRecordAdmissionAndQueuePressure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(previous)
	if err := configureOperationalMetrics(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	RecordAdmission(ctx, "http.ask", "denied", "concurrency_limit", 2, 2, 12000, 240000, 0.25, 10)
	RecordDetectorQueue(ctx, "enqueued", 5, 512, 0)
	RecordDetectorQueue(ctx, "dequeued", 4, 512, 1.25)
	RecordDetectorQueue(ctx, "dropped", 512, 512, 0)

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &metrics); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, scope := range metrics.ScopeMetrics {
		for _, item := range scope.Metrics {
			names[item.Name] = true
		}
	}
	for _, name := range []string{
		"nerv.admission.requests",
		"nerv.admission.active",
		"nerv.admission.daily_tokens",
		"nerv.admission.daily_cost",
		"nerv.detector.queue.depth",
		"nerv.detector.queue.dropped",
		"nerv.detector.queue.wait_time",
	} {
		if !names[name] {
			t.Errorf("metric %q not recorded", name)
		}
	}
}

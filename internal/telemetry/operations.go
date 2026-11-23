package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	admissionRequests     metric.Int64Counter
	admissionActive       metric.Int64Gauge
	admissionDailyTokens  metric.Int64Gauge
	admissionDailyCost    metric.Float64Gauge
	detectorQueueDepth    metric.Int64Gauge
	detectorQueueDropped  metric.Int64Counter
	detectorQueueWaitTime metric.Float64Histogram
)

func configureOperationalMetrics() error {
	meter := otel.Meter(instrumentationName)
	var err error
	if admissionRequests, err = meter.Int64Counter("nerv.admission.requests", metric.WithUnit("{request}"), metric.WithDescription("Investigation admission decisions.")); err != nil {
		return err
	}
	if admissionActive, err = meter.Int64Gauge("nerv.admission.active", metric.WithUnit("{investigation}"), metric.WithDescription("Active durable investigation admission leases observed during admission.")); err != nil {
		return err
	}
	if admissionDailyTokens, err = meter.Int64Gauge("nerv.admission.daily_tokens", metric.WithUnit("{token}"), metric.WithDescription("Settled investigation tokens used during current UTC day.")); err != nil {
		return err
	}
	if admissionDailyCost, err = meter.Float64Gauge("nerv.admission.daily_cost", metric.WithUnit("USD"), metric.WithDescription("Settled investigation cost during current UTC day.")); err != nil {
		return err
	}
	if detectorQueueDepth, err = meter.Int64Gauge("nerv.detector.queue.depth", metric.WithUnit("{event}"), metric.WithDescription("Current autonomous detector queue depth.")); err != nil {
		return err
	}
	if detectorQueueDropped, err = meter.Int64Counter("nerv.detector.queue.dropped", metric.WithUnit("{event}"), metric.WithDescription("Autonomous detector events dropped because queue is full.")); err != nil {
		return err
	}
	if detectorQueueWaitTime, err = meter.Float64Histogram("nerv.detector.queue.wait_time", metric.WithUnit("s"), metric.WithDescription("Time autonomous detector events wait before processing.")); err != nil {
		return err
	}
	return nil
}

func RecordAdmission(ctx context.Context, source, result, reason string, active, activeLimit int, dailyTokens int64, dailyTokenLimit int, dailyCost, dailyCostLimit float64) {
	if admissionRequests == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("nerv.source", source),
		attribute.String("nerv.admission.result", result),
	}
	if reason != "" {
		attrs = append(attrs, attribute.String("nerv.admission.reason", reason))
	}
	admissionRequests.Add(ctx, 1, metric.WithAttributes(attrs...))
	admissionActive.Record(ctx, int64(active), metric.WithAttributes(attribute.Int("nerv.limit", activeLimit)))
	admissionDailyTokens.Record(ctx, dailyTokens, metric.WithAttributes(attribute.Int("nerv.limit", dailyTokenLimit)))
	admissionDailyCost.Record(ctx, dailyCost, metric.WithAttributes(attribute.Float64("nerv.limit", dailyCostLimit)))
}

func RecordAdmissionActive(ctx context.Context, active, activeLimit int) {
	if admissionActive == nil {
		return
	}
	admissionActive.Record(ctx, int64(active), metric.WithAttributes(attribute.Int("nerv.limit", activeLimit)))
}

func RecordDetectorQueue(ctx context.Context, event string, depth, capacity int, waitSeconds float64) {
	if detectorQueueDepth == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.Int("nerv.queue.capacity", capacity))
	detectorQueueDepth.Record(ctx, int64(depth), attrs)
	switch event {
	case "dropped":
		detectorQueueDropped.Add(ctx, 1, attrs)
	case "dequeued":
		detectorQueueWaitTime.Record(ctx, waitSeconds, attrs)
	}
}

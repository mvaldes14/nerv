package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

func TestMergeServiceResourceAcceptsDifferentBaseSchema(t *testing.T) {
	base := resource.NewWithAttributes(
		"https://opentelemetry.io/schemas/1.43.0",
		attribute.String("host.name", "nerv-node"),
	)
	got, err := mergeServiceResource(base, "nerv")
	if err != nil {
		t.Fatal(err)
	}
	serviceName, ok := got.Set().Value(semconv.ServiceNameKey)
	if !ok || serviceName.AsString() != "nerv" {
		t.Fatalf("service.name = %q, present=%v", serviceName.AsString(), ok)
	}
	if got.SchemaURL() != base.SchemaURL() {
		t.Fatalf("schema URL = %q, want %q", got.SchemaURL(), base.SchemaURL())
	}
}

func TestSemanticConventionVersionPinned(t *testing.T) {
	if SemanticConventionVersion != "1.37.0" {
		t.Fatalf("semantic convention version = %q", SemanticConventionVersion)
	}
}

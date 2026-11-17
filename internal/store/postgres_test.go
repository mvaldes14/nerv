package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mvaldes14/nerv/internal/investigation"
)

func TestIncidentListQueryAppliesFiltersAndNewestFirstPagination(t *testing.T) {
	query, args := incidentListQuery(investigation.IncidentFilter{
		Limit:     25,
		Offset:    50,
		Source:    "kubernetes.detector",
		Status:    "completed",
		Namespace: "apps",
		Workload:  "checkout",
	})
	for _, fragment := range []string{
		"i.source = $1",
		"i.status = $2",
		"i.target->>'namespace' = $3",
		"i.target->>'name' = $4",
		"ORDER BY i.created_at DESC, i.id DESC",
		"LIMIT $5 OFFSET $6",
	} {
		if !strings.Contains(query, fragment) {
			t.Errorf("query missing %q: %s", fragment, query)
		}
	}
	wantArgs := []any{"kubernetes.detector", "completed", "apps", "checkout", 26, 50}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

func TestIncidentListQueryOmitsEmptyFilters(t *testing.T) {
	query, args := incidentListQuery(investigation.IncidentFilter{Limit: 10})
	if strings.Contains(query, "i.source =") || strings.Contains(query, "i.status =") || strings.Contains(query, "i.target->>") {
		t.Fatalf("query contains unexpected filter: %s", query)
	}
	wantArgs := []any{11, 0}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

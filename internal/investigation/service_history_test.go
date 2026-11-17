package investigation

import (
	"context"
	"testing"
)

type historyStore struct {
	Store
	filter IncidentFilter
}

func (s *historyStore) ListIncidents(_ context.Context, filter IncidentFilter) (IncidentPage, error) {
	s.filter = filter
	return IncidentPage{Limit: filter.Limit, Offset: filter.Offset}, nil
}

func TestListBoundsPaginationAndTrimsFilters(t *testing.T) {
	store := &historyStore{}
	service := &Service{store: store}
	page, err := service.List(context.Background(), IncidentFilter{
		Limit:     MaxIncidentLimit + 50,
		Offset:    MaxIncidentOffset + 50,
		Source:    " http.alert ",
		Status:    " completed ",
		Namespace: " apps ",
		Workload:  " checkout ",
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := IncidentFilter{
		Limit:     MaxIncidentLimit,
		Offset:    MaxIncidentOffset,
		Source:    "http.alert",
		Status:    "completed",
		Namespace: "apps",
		Workload:  "checkout",
	}
	if store.filter != want {
		t.Fatalf("store filter = %#v, want %#v", store.filter, want)
	}
	if page.Limit != MaxIncidentLimit || page.Offset != MaxIncidentOffset {
		t.Fatalf("page bounds = limit %d offset %d", page.Limit, page.Offset)
	}
}

func TestListUsesDefaultsForNonPositivePagination(t *testing.T) {
	store := &historyStore{}
	service := &Service{store: store}
	if _, err := service.List(context.Background(), IncidentFilter{Limit: -1, Offset: -1}); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if store.filter.Limit != DefaultIncidentLimit || store.filter.Offset != 0 {
		t.Fatalf("filter = %#v", store.filter)
	}
}

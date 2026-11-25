package investigation

import (
	"context"
	"errors"
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

type historyStore struct {
	Store
	filter IncidentFilter
}

func (s *historyStore) ListIncidents(_ context.Context, filter IncidentFilter) (IncidentPage, error) {
	s.filter = filter
	return IncidentPage{Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *historyStore) ListIncidentFilterOptions(context.Context) (IncidentFilterOptions, error) {
	return IncidentFilterOptions{}, nil
}

type cancellationStore struct {
	Store
	detail         Investigation
	failedIncident string
	failedWorkflow string
}

func (s *cancellationStore) GetInvestigation(context.Context, string) (Investigation, error) {
	return s.detail, nil
}

func (s *cancellationStore) FailRunningInvestigation(_ context.Context, incidentID, workflowID string) (int, error) {
	s.failedIncident, s.failedWorkflow = incidentID, workflowID
	s.detail.Incident.Status = "failed"
	return 0, nil
}

func TestCancelRunningInvestigationCancelsWorkflowAndFailsIncident(t *testing.T) {
	store := &cancellationStore{detail: Investigation{Incident: IncidentSummary{ID: "inc-1", WorkflowID: "workflow-1", Status: "running"}}}
	cancelled := ""
	service := &Service{
		store: store,
		listWorkflows: func(dbos.Client, ...dbos.ListWorkflowsOption) ([]dbos.WorkflowStatus, error) {
			return []dbos.WorkflowStatus{{ID: "workflow-1", Status: dbos.WorkflowStatusPending}}, nil
		},
		cancelWorkflow: func(_ dbos.Client, workflowID string, _ ...dbos.CancelWorkflowOption) error {
			cancelled = workflowID
			return nil
		},
	}
	detail, err := service.Cancel(context.Background(), "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != "workflow-1" || store.failedIncident != "inc-1" || store.failedWorkflow != "workflow-1" || detail.Incident.Status != "failed" {
		t.Fatalf("cancelled=%q failed=%q/%q detail=%+v", cancelled, store.failedIncident, store.failedWorkflow, detail)
	}
}

func TestCancelRepairsIncidentForTerminalFailedWorkflow(t *testing.T) {
	store := &cancellationStore{detail: Investigation{Incident: IncidentSummary{ID: "inc-1", WorkflowID: "workflow-1", Status: "running"}}}
	service := &Service{
		store: store,
		listWorkflows: func(dbos.Client, ...dbos.ListWorkflowsOption) ([]dbos.WorkflowStatus, error) {
			return []dbos.WorkflowStatus{{ID: "workflow-1", Status: dbos.WorkflowStatusError}}, nil
		},
		cancelWorkflow: func(dbos.Client, string, ...dbos.CancelWorkflowOption) error {
			t.Fatal("terminal workflow must not be cancelled again")
			return nil
		},
	}
	if _, err := service.Cancel(context.Background(), "inc-1"); err != nil {
		t.Fatal(err)
	}
	if store.failedIncident != "inc-1" {
		t.Fatalf("failed incident = %q", store.failedIncident)
	}
}

func TestCancelRejectsNonRunningIncident(t *testing.T) {
	store := &cancellationStore{detail: Investigation{Incident: IncidentSummary{ID: "inc-1", WorkflowID: "workflow-1", Status: "completed"}}}
	service := &Service{store: store}
	_, err := service.Cancel(context.Background(), "inc-1")
	if !errors.Is(err, ErrInvestigationNotRunning) {
		t.Fatalf("error = %v", err)
	}
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

func TestFilterOptionsIncludeKnownValues(t *testing.T) {
	store := &historyStore{}
	service := &Service{store: store}
	options, err := service.FilterOptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Sources) != 4 || options.Sources[0] != SourceHTTPAsk || len(options.Statuses) != 3 {
		t.Fatalf("options = %+v", options)
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

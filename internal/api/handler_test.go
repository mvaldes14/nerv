package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mvaldes14/nerv/internal/detection"
	"github.com/mvaldes14/nerv/internal/investigation"
)

type fakeInvestigationService struct {
	listFilter investigation.IncidentFilter
	page       investigation.IncidentPage
	detail     investigation.Investigation
}

func (f *fakeInvestigationService) Ready(context.Context) error { return nil }
func (f *fakeInvestigationService) Ask(context.Context, investigation.Request) (investigation.Finding, error) {
	return investigation.Finding{}, nil
}
func (f *fakeInvestigationService) Accept(context.Context, investigation.Request) (investigation.Accepted, error) {
	return investigation.Accepted{}, nil
}
func (f *fakeInvestigationService) Get(context.Context, string) (investigation.Investigation, error) {
	return f.detail, nil
}
func (f *fakeInvestigationService) List(_ context.Context, filter investigation.IncidentFilter) (investigation.IncidentPage, error) {
	f.listFilter = filter
	return f.page, nil
}

func TestAlertRequestCorrelatesWithAutonomousFailure(t *testing.T) {
	alert := alertmanagerAlert{
		Fingerprint: "alertmanager-fingerprint",
		Labels: map[string]string{
			"alertname": "KubePodCrashLooping",
			"namespace": "default",
			"pod":       "broken",
			"container": "app",
		},
		Annotations: map[string]string{"summary": "Pod is in CrashLoopBackOff"},
	}
	req := alertRequest("receiver", alert)
	want := investigation.CorrelationKey(req.Target, detection.FailureCrashLoop)
	if req.IdempotencyKey != want {
		t.Fatalf("IdempotencyKey = %q, want %q", req.IdempotencyKey, want)
	}
}

func TestAlertRequestKeepsFingerprintWithoutKnownFailureIdentity(t *testing.T) {
	alert := alertmanagerAlert{Fingerprint: "fingerprint", Labels: map[string]string{"alertname": "CustomAlert", "pod": "pod"}}
	if got := alertRequest("receiver", alert).IdempotencyKey; got != alert.Fingerprint {
		t.Fatalf("IdempotencyKey = %q, want fingerprint", got)
	}
}

func TestInvestigationHistoryRequiresBearerAuthentication(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, AuthToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/investigations", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestInvestigationHistoryPassesFilters(t *testing.T) {
	service := &fakeInvestigationService{page: investigation.IncidentPage{Items: []investigation.IncidentSummary{}}}
	handler := NewHandler(Dependencies{Service: service, AuthToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/investigations?limit=12&offset=4&source=http.alert&status=completed&namespace=apps&workload=checkout", nil)
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	want := investigation.IncidentFilter{Limit: 12, Offset: 4, Source: "http.alert", Status: "completed", Namespace: "apps", Workload: "checkout"}
	if service.listFilter != want {
		t.Fatalf("filter = %#v, want %#v", service.listFilter, want)
	}
	var body investigation.IncidentPage
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func TestInvestigationHistoryRejectsInvalidPagination(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, AuthToken: "secret"})
	for _, query := range []string{
		"limit=many",
		"limit=-1",
		"limit=101",
		"offset=-1",
		"offset=10001",
	} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/investigations?"+query, nil)
			req.Header.Set("Authorization", "Bearer secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestOperatorUIAndAssetsLoadWithoutAuthentication(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, AuthToken: "secret"})
	for _, path := range []string{"/ui/", "/ui/app.js", "/ui/styles.css", "/ui/nerv-mark.svg"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, response.Code, http.StatusOK)
		}
	}
}

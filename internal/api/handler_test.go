package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/detection"
	"github.com/mvaldes14/nerv/internal/investigation"
	"github.com/mvaldes14/nerv/internal/remediation"
)

type fakeInvestigationService struct {
	askRequest investigation.Request
	askFinding investigation.Finding
	askErr     error
	acceptErr  error
	listFilter investigation.IncidentFilter
	page       investigation.IncidentPage
	detail     investigation.Investigation
}

func (f *fakeInvestigationService) Ready(context.Context) error { return nil }
func (f *fakeInvestigationService) Ask(_ context.Context, req investigation.Request) (investigation.Finding, error) {
	f.askRequest = req
	return f.askFinding, f.askErr
}
func (f *fakeInvestigationService) Accept(context.Context, investigation.Request) (investigation.Accepted, error) {
	return investigation.Accepted{}, f.acceptErr
}
func (f *fakeInvestigationService) Get(context.Context, string) (investigation.Investigation, error) {
	return f.detail, nil
}
func (f *fakeInvestigationService) List(_ context.Context, filter investigation.IncidentFilter) (investigation.IncidentPage, error) {
	f.listFilter = filter
	return f.page, nil
}

type fakeRemediationService struct {
	createRequest remediation.CreateRequest
	decision      remediation.DecisionRequest
	authSubject   string
	proposal      remediation.Proposal
	err           error
}

func (f *fakeRemediationService) Create(_ context.Context, _ string, req remediation.CreateRequest) (remediation.Proposal, error) {
	f.createRequest = req
	return f.proposal, f.err
}
func (f *fakeRemediationService) List(context.Context, string) ([]remediation.Proposal, error) {
	return []remediation.Proposal{f.proposal}, f.err
}
func (f *fakeRemediationService) ListAll(context.Context, string) ([]remediation.Proposal, error) {
	return []remediation.Proposal{f.proposal}, f.err
}
func (f *fakeRemediationService) Get(context.Context, string) (remediation.Proposal, error) {
	return f.proposal, f.err
}
func (f *fakeRemediationService) Approve(_ context.Context, _ string, req remediation.DecisionRequest, subject string) (remediation.Proposal, error) {
	f.decision, f.authSubject = req, subject
	return f.proposal, f.err
}
func (f *fakeRemediationService) Reject(_ context.Context, _ string, req remediation.DecisionRequest, subject string) (remediation.Proposal, error) {
	f.decision, f.authSubject = req, subject
	return f.proposal, f.err
}
func (f *fakeRemediationService) Submit(context.Context, string) (remediation.Proposal, error) {
	return f.proposal, f.err
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

func TestAskSetsExplicitOperatorSource(t *testing.T) {
	service := &fakeInvestigationService{askFinding: investigation.Finding{ID: "inc-1"}}
	handler := NewHandler(Dependencies{Service: service})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewBufferString(`{"question":"is vault running?","source":"untrusted"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if service.askRequest.Source != investigation.SourceHTTPAsk {
		t.Fatalf("source = %q, want %q", service.askRequest.Source, investigation.SourceHTTPAsk)
	}
}

func TestAskReturnsRateLimitWithRetryAfter(t *testing.T) {
	service := &fakeInvestigationService{askErr: &investigation.AdmissionError{Reason: "concurrency_limit", RetryAfter: 1500 * time.Millisecond}}
	handler := NewHandler(Dependencies{Service: service})
	req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewBufferString(`{"question":"is vault running?"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusTooManyRequests, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if !strings.Contains(response.Body.String(), "concurrency_limit") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestInvestigationDetailPreservesSuppressionDecisionMetadata(t *testing.T) {
	service := &fakeInvestigationService{detail: investigation.Investigation{
		Incident: investigation.IncidentSummary{ID: "inc-1", Status: "completed"},
		Finding: &investigation.Finding{
			ID:     "inc-1",
			Answer: "Investigation suppressed by policy.\nReason: disposition=duplicate urgency=immediate",
			Decision: investigation.Decision{
				Action:    "suppress",
				Reason:    "disposition=duplicate urgency=immediate",
				Provider:  "laya",
				RequestID: "req-1",
			},
		},
	}}
	handler := NewHandler(Dependencies{Service: service})
	req := httptest.NewRequest(http.MethodGet, "/investigations/inc-1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var body struct {
		Finding investigation.Finding `json:"finding"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Finding.Decision.Action != "suppress" || body.Finding.Decision.Reason == "" || body.Finding.Decision.Provider != "laya" || body.Finding.Decision.RequestID != "req-1" {
		t.Fatalf("decision = %+v", body.Finding.Decision)
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

func TestOperatorUIStartsBehindAuthenticationGate(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, AuthToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	body := response.Body.String()
	for _, required := range []string{"auth-gate", "operator-shell", "Ask", "Incidents", "Remediation"} {
		if !strings.Contains(body, required) {
			t.Errorf("index does not contain %q", required)
		}
	}
	if !strings.Contains(body, `id="operator-shell" hidden`) {
		t.Fatal("operator shell must start hidden")
	}
}

func TestAuthCheckAcceptsValidBearerToken(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, AuthToken: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/auth/check", nil)
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authenticated":true`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRemediationRoutesRequireBearerAuthentication(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, Remediation: &fakeRemediationService{}, AuthToken: "secret"})
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/auth/check"},
		{method: http.MethodGet, path: "/remediation-proposals"},
		{method: http.MethodPost, path: "/investigations/inc-1/remediation-proposals"},
		{method: http.MethodGet, path: "/investigations/inc-1/remediation-proposals"},
		{method: http.MethodGet, path: "/remediation-proposals/proposal-1"},
		{method: http.MethodPost, path: "/remediation-proposals/proposal-1/approve"},
		{method: http.MethodPost, path: "/remediation-proposals/proposal-1/reject"},
		{method: http.MethodPost, path: "/remediation-proposals/proposal-1/submit"},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestRemediationEndpointsReturnProposalData(t *testing.T) {
	service := &fakeRemediationService{proposal: remediation.Proposal{ID: "proposal-1", IncidentID: "inc-1", Status: remediation.StatusPending}}
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, Remediation: service, AuthToken: "secret"})
	createBody := `{"action_kind":"set_workload_replicas/v1","target_id":"checkout","expected":{"replicas":2},"desired":{"replicas":3},"summary":"scale","rationale":"load","risk":{"level":"low","detail":"bounded"},"evidence_ordinals":[0]}`
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{name: "create", method: http.MethodPost, path: "/investigations/inc-1/remediation-proposals", body: createBody, want: http.StatusCreated},
		{name: "list", method: http.MethodGet, path: "/investigations/inc-1/remediation-proposals", want: http.StatusOK},
		{name: "queue", method: http.MethodGet, path: "/remediation-proposals", want: http.StatusOK},
		{name: "get", method: http.MethodGet, path: "/remediation-proposals/proposal-1", want: http.StatusOK},
		{name: "reject", method: http.MethodPost, path: "/remediation-proposals/proposal-1/reject", body: `{"actor":"alice","comment":"not now"}`, want: http.StatusOK},
		{name: "submit", method: http.MethodPost, path: "/remediation-proposals/proposal-1/submit", want: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "proposal-1") {
				t.Fatalf("body = %s", response.Body.String())
			}
		})
	}
}

func TestCreateRemediationProposalStrictlyRejectsExecutableFields(t *testing.T) {
	service := &fakeRemediationService{}
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, Remediation: service, AuthToken: "secret"})
	body := `{
		"action_kind":"set_workload_replicas/v1",
		"target_id":"checkout",
		"expected":{"replicas":2},
		"desired":{"replicas":3},
		"summary":"scale",
		"rationale":"load",
		"risk":{"level":"low","detail":"bounded"},
		"evidence_ordinals":[0],
		"repository":"attacker/repo",
		"patch":"rm -rf /"
	}`
	req := httptest.NewRequest(http.MethodPost, "/investigations/inc-1/remediation-proposals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if service.createRequest.ActionKind != "" {
		t.Fatal("service called for request containing forbidden fields")
	}
}

func TestApproveRemediationProposalRecordsSharedBearerSubject(t *testing.T) {
	service := &fakeRemediationService{proposal: remediation.Proposal{ID: "proposal-1", Status: remediation.StatusApproved}}
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, Remediation: service, AuthToken: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/remediation-proposals/proposal-1/approve", strings.NewReader(`{"actor":"alice","comment":"reviewed"}`))
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if service.authSubject != "shared-bearer" || service.decision.Actor != "alice" {
		t.Fatalf("decision=%+v subject=%q", service.decision, service.authSubject)
	}
}

func TestRemediationErrorsMapToContractStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "validation", err: &remediation.ValidationError{Message: "invalid"}, want: http.StatusBadRequest},
		{name: "missing", err: remediation.ErrNotFound, want: http.StatusNotFound},
		{name: "terminal", err: remediation.ErrConflict, want: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}, Remediation: &fakeRemediationService{err: test.err}})
			req := httptest.NewRequest(http.MethodGet, "/remediation-proposals/proposal-1", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestOperatorUIRendersSuppressionAsPolicyOutcome(t *testing.T) {
	handler := NewHandler(Dependencies{Service: &fakeInvestigationService{}})
	req := httptest.NewRequest(http.MethodGet, "/ui/app.js", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	script := response.Body.String()
	for _, text := range []string{"Policy outcome", "Decision reason", "Decision provider", "Decision request ID"} {
		if !strings.Contains(script, text) {
			t.Errorf("app.js does not contain %q", text)
		}
	}
	for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "eval("} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("app.js must not contain %q", forbidden)
		}
	}
	for _, required := range []string{"loadRemediationQueue", "rendered_diff", "window.confirm", "Approve", "Reject", "Submit GitHub PR", "/auth/check"} {
		if !strings.Contains(script, required) {
			t.Errorf("app.js does not contain %q", required)
		}
	}
}

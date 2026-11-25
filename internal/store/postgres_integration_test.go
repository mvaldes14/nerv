package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mvaldes14/nerv/internal/investigation"
	"github.com/mvaldes14/nerv/internal/remediation"
)

func openIntegrationPostgres(t *testing.T) *Postgres {
	t.Helper()
	databaseURL := os.Getenv("NERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("NERV_TEST_DATABASE_URL is not set")
	}
	postgres, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(postgres.Close)
	if _, err := postgres.pool.Exec(context.Background(), `TRUNCATE nerv.investigation_admission_leases, nerv.investigation_rate_windows, nerv.notification_batch_items, nerv.notification_batches, nerv.notification_attempts, nerv.evidence, nerv.findings, nerv.incident_correlations, nerv.incidents CASCADE`); err != nil {
		t.Fatal(err)
	}
	return postgres
}

func createCompletedIncident(t *testing.T, postgres *Postgres, incidentID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incidents (id, workflow_id, source, question, status) VALUES ($1, $2, $3, 'test', 'completed')`, incidentID, "workflow-"+incidentID, investigation.SourceHTTPAsk); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.evidence (incident_id, ordinal, source, summary) VALUES ($1, 0, 'kubernetes.status', 'replicas=2'), ($1, 1, 'kubernetes.metrics', 'cpu=95%')`, incidentID); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.findings (incident_id, workflow_id, answer, decision, created_at) VALUES ($1, $2, 'scale may help', '{}'::jsonb, now())`, incidentID, "workflow-"+incidentID); err != nil {
		t.Fatal(err)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func remediationServiceForTest(t *testing.T, postgres *Postgres) *remediation.Service {
	t.Helper()
	service, err := remediation.NewService(remediation.Config{
		Repository: "mvaldes14/k8s-apps",
		Targets: []remediation.Target{{
			ID: "checkout", Path: "apps/checkout/deployment.yaml",
			Resource: remediation.Resource{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "apps", Name: "checkout"},
		}},
	}, postgres)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func remediationRequestForTest() remediation.CreateRequest {
	expected, desired := 2, 4
	return remediation.CreateRequest{
		ActionKind: remediation.ActionSetWorkloadReplicas,
		TargetID:   "checkout", Expected: remediation.ReplicaInput{Replicas: &expected}, Desired: remediation.ReplicaInput{Replicas: &desired},
		Summary: "Scale checkout", Rationale: "Sustained load", Risk: remediation.RiskInput{Level: "medium", Detail: "Resource use"},
		EvidenceOrdinals: []int{0, 1}, TTLSeconds: 600,
	}
}

func TestRemediationProposalRequiresCompletedIncidentAndValidEvidence(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	service := remediationServiceForTest(t, postgres)
	ctx := context.Background()

	runningID := uuid.NewString()
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incidents (id, workflow_id, source, question, status) VALUES ($1, $2, $3, 'test', 'running')`, runningID, "workflow-"+runningID, investigation.SourceHTTPAsk); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, runningID, remediationRequestForTest()); !errors.Is(err, remediation.ErrConflict) {
		t.Fatalf("running incident error = %v", err)
	}

	completedID := uuid.NewString()
	createCompletedIncident(t, postgres, completedID)
	req := remediationRequestForTest()
	req.EvidenceOrdinals = []int{99}
	_, err := service.Create(ctx, completedID, req)
	var validation *remediation.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("invalid evidence error = %v", err)
	}
}

func TestRemediationProposalPersistenceQueryAndStateTransitions(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	incidentID := uuid.NewString()
	createCompletedIncident(t, postgres, incidentID)
	service := remediationServiceForTest(t, postgres)

	created, err := service.Create(context.Background(), incidentID, remediationRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != remediation.StatusPending || len(created.Evidence) != 2 {
		t.Fatalf("created proposal = %+v", created)
	}
	if _, err := postgres.pool.Exec(context.Background(), `UPDATE nerv.evidence SET summary = 'changed after proposal' WHERE incident_id = $1 AND ordinal = 0`, incidentID); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Evidence[0].Summary != "replicas=2" {
		t.Fatalf("evidence snapshot changed: %+v", got.Evidence[0])
	}
	listed, err := service.List(context.Background(), incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed proposals = %+v", listed)
	}
	queued, err := service.ListAll(context.Background(), remediation.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].ID != created.ID {
		t.Fatalf("queued proposals = %+v", queued)
	}
	approved, err := service.Approve(context.Background(), created.ID, remediation.DecisionRequest{Actor: "alice", Comment: "reviewed"}, "shared-bearer")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != remediation.StatusApproved || approved.DecisionActor != "alice" || approved.DecisionAuthSubject != "shared-bearer" || approved.DecisionAt == nil {
		t.Fatalf("approved proposal = %+v", approved)
	}
	if _, err := service.Reject(context.Background(), created.ID, remediation.DecisionRequest{Actor: "bob"}, "shared-bearer"); !errors.Is(err, remediation.ErrConflict) {
		t.Fatalf("terminal transition error = %v", err)
	}
	submitting, err := postgres.BeginProposalSubmission(context.Background(), created.ID)
	if err != nil || submitting.SubmissionStatus != remediation.SubmissionSubmitting {
		t.Fatalf("begin submission = %+v, %v", submitting, err)
	}
	submitted, err := postgres.CompleteProposalSubmission(context.Background(), created.ID, remediation.SubmissionResult{
		Branch: "nerv/remediation-test", CommitSHA: "abc123", PullRequestNumber: 108, PullRequestURL: "https://github.test/pr/108",
	})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.SubmissionStatus != remediation.SubmissionSubmitted || submitted.PullRequestNumber != 108 || submitted.PullRequestURL == "" || submitted.SubmittedAt == nil {
		t.Fatalf("submitted proposal = %+v", submitted)
	}
	closedAt := time.Now().UTC()
	tracked, err := postgres.UpdateProposalPullRequestStatus(context.Background(), created.ID, remediation.PullRequestStatus{State: "closed", ClosedAt: &closedAt})
	if err != nil {
		t.Fatal(err)
	}
	if tracked.PullRequestState != "closed" || tracked.PullRequestMerged || tracked.PullRequestCheckedAt == nil || tracked.PullRequestClosedAt == nil {
		t.Fatalf("tracked proposal = %+v", tracked)
	}
	options, err := postgres.ListIncidentFilterOptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(options.Sources, investigation.SourceHTTPAsk) || !containsString(options.Statuses, "completed") {
		t.Fatalf("filter options = %+v", options)
	}
}

func TestRemediationProposalExpiresLazilyAndCannotTransition(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	incidentID := uuid.NewString()
	createCompletedIncident(t, postgres, incidentID)
	service := remediationServiceForTest(t, postgres)
	created, err := service.Create(context.Background(), incidentID, remediationRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(context.Background(), `UPDATE nerv.remediation_proposals SET created_at = now() - interval '10 minutes', expires_at = now() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(context.Background(), created.ID, remediation.DecisionRequest{Actor: "alice"}, "shared-bearer"); !errors.Is(err, remediation.ErrConflict) {
		t.Fatalf("expired transition error = %v", err)
	}
	got, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != remediation.StatusExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}
}

func TestRemediationConcurrentDecisionHasSingleWinner(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	incidentID := uuid.NewString()
	createCompletedIncident(t, postgres, incidentID)
	service := remediationServiceForTest(t, postgres)
	created, err := service.Create(context.Background(), incidentID, remediationRequestForTest())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := service.Approve(context.Background(), created.ID, remediation.DecisionRequest{Actor: "alice"}, "shared-bearer")
		errorsCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := service.Reject(context.Background(), created.ID, remediation.DecisionRequest{Actor: "bob"}, "shared-bearer")
		errorsCh <- err
	}()
	wg.Wait()
	close(errorsCh)
	wins, conflicts := 0, 0
	for err := range errorsCh {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, remediation.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected decision error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestFailedIncidentReleasesCorrelationForRetry(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	ctx := context.Background()
	incidentID := uuid.NewString()
	workflowID := "workflow-" + incidentID
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incidents (id, workflow_id, source, question, status) VALUES ($1, $2, $3, 'test', 'running')`, incidentID, workflowID, investigation.SourceKubernetesDetector); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incident_correlations (correlation_key, incident_id, active, cooldown_seconds) VALUES ($1, $2, true, 600)`, "correlation-"+incidentID, incidentID); err != nil {
		t.Fatal(err)
	}
	if err := postgres.SetStatus(ctx, incidentID, "failed"); err != nil {
		t.Fatal(err)
	}
	var status string
	var active bool
	var cooldown *time.Time
	if err := postgres.pool.QueryRow(ctx, `
SELECT i.status, c.active, c.cooldown_until
FROM nerv.incidents i
JOIN nerv.incident_correlations c ON c.incident_id = i.id
WHERE i.id = $1`, incidentID).Scan(&status, &active, &cooldown); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || active || cooldown == nil || !cooldown.After(time.Now()) {
		t.Fatalf("status=%q active=%v cooldown=%v", status, active, cooldown)
	}
}

func TestReconcileTerminalInvestigationsRepairsStaleState(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	ctx := context.Background()
	if _, err := postgres.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS dbos`); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS dbos.workflow_status (workflow_uuid text PRIMARY KEY, status text)`); err != nil {
		t.Fatal(err)
	}
	incidentID := uuid.NewString()
	workflowID := "incident-" + incidentID
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incidents (id, workflow_id, source, question, status) VALUES ($1, $2, $3, 'test', 'running')`, incidentID, workflowID, investigation.SourceKubernetesDetector); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incident_correlations (correlation_key, incident_id, active, cooldown_seconds) VALUES ($1, $2, true, 600)`, "correlation-"+incidentID, incidentID); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.investigation_admission_leases (workflow_id, source, expires_at) VALUES ($1, $2, now() + interval '10 minutes')`, workflowID, investigation.SourceKubernetesDetector); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO dbos.workflow_status (workflow_uuid, status) VALUES ($1, 'ERROR') ON CONFLICT (workflow_uuid) DO UPDATE SET status = EXCLUDED.status`, workflowID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = postgres.pool.Exec(context.Background(), `DELETE FROM dbos.workflow_status WHERE workflow_uuid = $1`, workflowID)
	})

	reconciled, err := postgres.ReconcileTerminalInvestigations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.IncidentsFailed != 1 || reconciled.LeasesReleased != 1 {
		t.Fatalf("reconciled = %+v", reconciled)
	}
	var status string
	var active bool
	var cooldown, releasedAt *time.Time
	if err := postgres.pool.QueryRow(ctx, `
SELECT i.status, c.active, c.cooldown_until, l.released_at
FROM nerv.incidents i
JOIN nerv.incident_correlations c ON c.incident_id = i.id
JOIN nerv.investigation_admission_leases l ON l.workflow_id = i.workflow_id
WHERE i.id = $1`, incidentID).Scan(&status, &active, &cooldown, &releasedAt); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || active || cooldown == nil || !cooldown.After(time.Now()) || releasedAt == nil {
		t.Fatalf("status=%q active=%v cooldown=%v released_at=%v", status, active, cooldown, releasedAt)
	}
}

func TestAdmissionEnforcesGlobalConcurrencyAndRate(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	ctx := context.Background()
	policy := investigation.AdmissionPolicy{
		MaxConcurrent:     2,
		LeaseTTL:          time.Minute,
		AskPerMinute:      100,
		AlertPerMinute:    100,
		DetectorPerMinute: 100,
		DailyMaxTokens:    100000,
	}
	if _, err := postgres.AdmitInvestigation(ctx, "workflow-1", investigation.SourceHTTPAsk, policy); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := postgres.AdmitInvestigation(ctx, "workflow-2", investigation.SourceHTTPAlert, policy); err != nil || snapshot.Active != 2 {
		t.Fatalf("second admission snapshot=%+v err=%v", snapshot, err)
	}
	_, err := postgres.AdmitInvestigation(ctx, "workflow-3", investigation.SourceKubernetesDetector, policy)
	var denied *investigation.AdmissionError
	if !errors.As(err, &denied) || denied.Reason != "concurrency_limit" {
		t.Fatalf("third admission error = %v", err)
	}
	if _, err := postgres.ReleaseInvestigation(ctx, "workflow-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.AdmitInvestigation(ctx, "workflow-3", investigation.SourceKubernetesDetector, policy); err != nil {
		t.Fatalf("admission after release: %v", err)
	}

	if _, err := postgres.pool.Exec(ctx, `TRUNCATE nerv.investigation_admission_leases, nerv.investigation_rate_windows`); err != nil {
		t.Fatal(err)
	}
	policy.MaxConcurrent = 10
	policy.AskPerMinute = 2
	for _, workflowID := range []string{"rate-1", "rate-2"} {
		if _, err := postgres.AdmitInvestigation(ctx, workflowID, investigation.SourceHTTPAsk, policy); err != nil {
			t.Fatal(err)
		}
		if _, err := postgres.ReleaseInvestigation(ctx, workflowID); err != nil {
			t.Fatal(err)
		}
	}
	_, err = postgres.AdmitInvestigation(ctx, "rate-3", investigation.SourceHTTPAsk, policy)
	if !errors.As(err, &denied) || denied.Reason != "rate_limit" {
		t.Fatalf("rate admission error = %v", err)
	}
	if _, err := postgres.AdmitInvestigation(ctx, "rate-1", investigation.SourceHTTPAsk, policy); err != nil {
		t.Fatalf("idempotent admission counted against rate: %v", err)
	}
}

func TestAdmissionSerializesConcurrentReplicas(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	policy := investigation.AdmissionPolicy{MaxConcurrent: 1, LeaseTTL: time.Minute, AskPerMinute: 100, AlertPerMinute: 100, DetectorPerMinute: 100, DailyMaxTokens: 100000}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	denied := 0
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := postgres.AdmitInvestigation(context.Background(), "concurrent-"+string(rune('a'+i)), investigation.SourceHTTPAlert, policy)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accepted++
				return
			}
			var admissionErr *investigation.AdmissionError
			if errors.As(err, &admissionErr) && admissionErr.Reason == "concurrency_limit" {
				denied++
				return
			}
			t.Errorf("unexpected admission error: %v", err)
		}()
	}
	wg.Wait()
	if accepted != 1 || denied != 7 {
		t.Fatalf("accepted=%d denied=%d", accepted, denied)
	}
}

func TestAdmissionEnforcesSettledDailyTokenLimit(t *testing.T) {
	postgres := openIntegrationPostgres(t)
	ctx := context.Background()
	incidentID := uuid.NewString()
	if _, err := postgres.pool.Exec(ctx, `INSERT INTO nerv.incidents (id, workflow_id, source, question, status) VALUES ($1, $2, $3, 'test', 'completed')`, incidentID, "settled-workflow", investigation.SourceHTTPAsk); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.pool.Exec(ctx, `
INSERT INTO nerv.findings (incident_id, workflow_id, answer, decision, budget, created_at)
VALUES ($1, $2, 'done', '{}'::jsonb, '{"total_tokens":120,"cost_usd":0.25}'::jsonb, now())`, incidentID, "settled-workflow"); err != nil {
		t.Fatal(err)
	}
	policy := investigation.AdmissionPolicy{MaxConcurrent: 2, LeaseTTL: time.Minute, AskPerMinute: 10, AlertPerMinute: 10, DetectorPerMinute: 10, DailyMaxTokens: 100}
	snapshot, err := postgres.AdmitInvestigation(ctx, "daily-limited", investigation.SourceHTTPAsk, policy)
	var denied *investigation.AdmissionError
	if !errors.As(err, &denied) || denied.Reason != "daily_token_limit" {
		t.Fatalf("daily admission snapshot=%+v err=%v", snapshot, err)
	}
	if snapshot.DailyTokens != 120 {
		t.Fatalf("daily tokens = %d, want 120", snapshot.DailyTokens)
	}

	policy.DailyMaxTokens = 1000
	policy.DailyMaxCostUSD = 0.20
	snapshot, err = postgres.AdmitInvestigation(ctx, "daily-cost-limited", investigation.SourceHTTPAsk, policy)
	if !errors.As(err, &denied) || denied.Reason != "daily_cost_limit" {
		t.Fatalf("daily cost admission snapshot=%+v err=%v", snapshot, err)
	}
	if snapshot.DailyCost != 0.25 {
		t.Fatalf("daily cost = %f, want 0.25", snapshot.DailyCost)
	}
}

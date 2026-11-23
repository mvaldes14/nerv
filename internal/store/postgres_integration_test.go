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

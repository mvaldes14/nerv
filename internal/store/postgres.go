package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mvaldes14/nerv/internal/investigation"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Postgres, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) AdmitInvestigation(ctx context.Context, workflowID, source string, policy investigation.AdmissionPolicy) (investigation.AdmissionSnapshot, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("begin admission transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('nerv-investigation-admission'))`); err != nil {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("lock investigation admission: %w", err)
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `DELETE FROM nerv.investigation_rate_windows WHERE window_start < $1`, now.Add(-48*time.Hour)); err != nil {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("prune investigation rate windows: %w", err)
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var snapshot investigation.AdmissionSnapshot
	if err := tx.QueryRow(ctx, `
SELECT
    count(*) FILTER (WHERE released_at IS NULL AND expires_at > $1),
    COALESCE((SELECT sum(CASE WHEN budget->>'total_tokens' ~ '^[0-9]+$' THEN (budget->>'total_tokens')::bigint ELSE 0 END)
              FROM nerv.findings WHERE created_at >= $2), 0),
    COALESCE((SELECT sum(CASE WHEN budget->>'cost_usd' ~ '^[0-9]+([.][0-9]+)?$' THEN (budget->>'cost_usd')::double precision ELSE 0 END)
              FROM nerv.findings WHERE created_at >= $2), 0)
FROM nerv.investigation_admission_leases`, now, dayStart).Scan(&snapshot.Active, &snapshot.DailyTokens, &snapshot.DailyCost); err != nil {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("read investigation admission usage: %w", err)
	}

	var releasedAt *time.Time
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT expires_at, released_at FROM nerv.investigation_admission_leases WHERE workflow_id = $1`, workflowID).Scan(&expiresAt, &releasedAt)
	if err == nil && (releasedAt != nil || expiresAt.After(now)) {
		if err := tx.Commit(ctx); err != nil {
			return investigation.AdmissionSnapshot{}, fmt.Errorf("commit idempotent investigation admission: %w", err)
		}
		return snapshot, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("read existing investigation admission: %w", err)
	}
	existing := err == nil

	deny := func(reason string, retryAfter time.Duration) (investigation.AdmissionSnapshot, error) {
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		snapshot.RetryAfter = retryAfter
		return snapshot, &investigation.AdmissionError{Reason: reason, RetryAfter: retryAfter}
	}
	if policy.DailyMaxTokens > 0 && snapshot.DailyTokens >= int64(policy.DailyMaxTokens) {
		return deny("daily_token_limit", dayStart.Add(24*time.Hour).Sub(now))
	}
	if policy.DailyMaxCostUSD > 0 && snapshot.DailyCost >= policy.DailyMaxCostUSD {
		return deny("daily_cost_limit", dayStart.Add(24*time.Hour).Sub(now))
	}
	if snapshot.Active >= policy.MaxConcurrent {
		var nextExpiry time.Time
		if err := tx.QueryRow(ctx, `SELECT min(expires_at) FROM nerv.investigation_admission_leases WHERE released_at IS NULL AND expires_at > $1`, now).Scan(&nextExpiry); err != nil {
			return investigation.AdmissionSnapshot{}, fmt.Errorf("read next admission lease expiry: %w", err)
		}
		return deny("concurrency_limit", nextExpiry.Sub(now))
	}

	windowStart := now.Truncate(time.Minute)
	limit := policy.RateLimit(source)
	var accepted int
	err = tx.QueryRow(ctx, `SELECT accepted FROM nerv.investigation_rate_windows WHERE source = $1 AND window_start = $2`, source, windowStart).Scan(&accepted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("read investigation rate window: %w", err)
	}
	if !existing && accepted >= limit {
		return deny("rate_limit", windowStart.Add(time.Minute).Sub(now))
	}
	if !existing {
		if _, err := tx.Exec(ctx, `
INSERT INTO nerv.investigation_rate_windows (source, window_start, accepted, updated_at)
VALUES ($1, $2, 1, now())
ON CONFLICT (source, window_start) DO UPDATE SET accepted = nerv.investigation_rate_windows.accepted + 1, updated_at = now()`, source, windowStart); err != nil {
			return investigation.AdmissionSnapshot{}, fmt.Errorf("increment investigation rate window: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO nerv.investigation_admission_leases (workflow_id, source, admitted_at, expires_at, released_at)
VALUES ($1, $2, $3, $4, NULL)
ON CONFLICT (workflow_id) DO UPDATE SET source = EXCLUDED.source, admitted_at = EXCLUDED.admitted_at, expires_at = EXCLUDED.expires_at, released_at = NULL`, workflowID, source, now, now.Add(policy.LeaseTTL)); err != nil {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("save investigation admission lease: %w", err)
	}
	snapshot.Active++
	if err := tx.Commit(ctx); err != nil {
		return investigation.AdmissionSnapshot{}, fmt.Errorf("commit investigation admission: %w", err)
	}
	return snapshot, nil
}

func (p *Postgres) ReleaseInvestigation(ctx context.Context, workflowID string) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin admission release transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('nerv-investigation-admission'))`); err != nil {
		return 0, fmt.Errorf("lock investigation admission release: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE nerv.investigation_admission_leases SET released_at = now() WHERE workflow_id = $1 AND released_at IS NULL`, workflowID); err != nil {
		return 0, fmt.Errorf("release investigation admission lease: %w", err)
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM nerv.investigation_admission_leases WHERE released_at IS NULL AND expires_at > now()`).Scan(&active); err != nil {
		return 0, fmt.Errorf("count active investigation admission leases: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit investigation admission release: %w", err)
	}
	return active, nil
}

type TerminalInvestigationReconciliation struct {
	IncidentsFailed int64
	LeasesReleased  int64
}

func (p *Postgres) ReconcileTerminalInvestigations(ctx context.Context) (TerminalInvestigationReconciliation, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return TerminalInvestigationReconciliation{}, fmt.Errorf("begin terminal investigation reconciliation: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('nerv-investigation-admission'))`); err != nil {
		return TerminalInvestigationReconciliation{}, fmt.Errorf("lock terminal investigation reconciliation: %w", err)
	}
	incidentResult, err := tx.Exec(ctx, `
UPDATE nerv.incidents i
SET status = 'failed', updated_at = now()
FROM dbos.workflow_status w
WHERE i.workflow_id = w.workflow_uuid
  AND i.status = 'running'
  AND w.status IN ('ERROR', 'CANCELLED', 'MAX_RECOVERY_ATTEMPTS_EXCEEDED')`)
	if err != nil {
		return TerminalInvestigationReconciliation{}, fmt.Errorf("reconcile terminal incident status: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE nerv.incident_correlations c
SET active = false, cooldown_until = NULL, updated_at = now()
FROM nerv.incidents i
JOIN dbos.workflow_status w ON w.workflow_uuid = i.workflow_id
WHERE c.incident_id = i.id
  AND c.active = true
  AND i.status = 'failed'
  AND w.status IN ('ERROR', 'CANCELLED', 'MAX_RECOVERY_ATTEMPTS_EXCEEDED')`); err != nil {
		return TerminalInvestigationReconciliation{}, fmt.Errorf("reconcile terminal incident correlation: %w", err)
	}
	leaseResult, err := tx.Exec(ctx, `
UPDATE nerv.investigation_admission_leases l
SET released_at = now()
FROM dbos.workflow_status w
WHERE l.workflow_id = w.workflow_uuid
  AND l.released_at IS NULL
  AND w.status IN ('ERROR', 'CANCELLED', 'MAX_RECOVERY_ATTEMPTS_EXCEEDED')`)
	if err != nil {
		return TerminalInvestigationReconciliation{}, fmt.Errorf("reconcile terminal investigation lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TerminalInvestigationReconciliation{}, fmt.Errorf("commit terminal investigation reconciliation: %w", err)
	}
	return TerminalInvestigationReconciliation{IncidentsFailed: incidentResult.RowsAffected(), LeasesReleased: leaseResult.RowsAffected()}, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('nerv-schema-migration'))`); err != nil {
		return fmt.Errorf("lock NERV schema migration: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('nerv-schema-migration'))`)

	const migration = `
CREATE SCHEMA IF NOT EXISTS nerv;

CREATE TABLE IF NOT EXISTS nerv.incidents (
    id text PRIMARY KEY,
    workflow_id text NOT NULL UNIQUE,
    source text NOT NULL,
    question text NOT NULL,
    target jsonb,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    annotations jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nerv.evidence (
    incident_id text NOT NULL REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    ordinal integer NOT NULL,
    source text NOT NULL,
    summary text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (incident_id, ordinal)
);

CREATE TABLE IF NOT EXISTS nerv.remediation_proposals (
    id text PRIMARY KEY,
    incident_id text NOT NULL REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    action_kind text NOT NULL CHECK (action_kind = 'set_workload_replicas/v1'),
    target_id text NOT NULL CHECK (length(target_id) BETWEEN 1 AND 128),
    target_repository text NOT NULL CHECK (length(target_repository) BETWEEN 3 AND 255),
    target_path text NOT NULL CHECK (length(target_path) BETWEEN 1 AND 1024),
    target_resource jsonb NOT NULL CHECK (
        jsonb_typeof(target_resource) = 'object'
        AND target_resource->>'api_version' = 'apps/v1'
        AND target_resource->>'kind' IN ('Deployment', 'StatefulSet')
    ),
    expected_state jsonb NOT NULL CHECK (
        jsonb_typeof(expected_state) = 'object'
        AND jsonb_typeof(expected_state->'replicas') = 'number'
        AND (expected_state->>'replicas')::numeric BETWEEN 0 AND 1000
        AND trunc((expected_state->>'replicas')::numeric) = (expected_state->>'replicas')::numeric
    ),
    desired_change jsonb NOT NULL CHECK (
        jsonb_typeof(desired_change) = 'object'
        AND jsonb_typeof(desired_change->'replicas') = 'number'
        AND (desired_change->>'replicas')::numeric BETWEEN 0 AND 1000
        AND trunc((desired_change->>'replicas')::numeric) = (desired_change->>'replicas')::numeric
    ),
    summary text NOT NULL CHECK (length(summary) BETWEEN 1 AND 200),
    rationale text NOT NULL CHECK (length(rationale) BETWEEN 1 AND 4000),
    risk_level text NOT NULL CHECK (risk_level IN ('low', 'medium', 'high')),
    risk_detail text NOT NULL CHECK (length(risk_detail) BETWEEN 1 AND 2000),
    rendered_before text NOT NULL,
    rendered_after text NOT NULL,
    rendered_diff text NOT NULL,
    render_format text NOT NULL CHECK (render_format = 'manifest_projection/v1'),
    status text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    decision_actor text,
    decision_auth_subject text,
    decision_comment text,
    decision_at timestamptz,
    submission_status text NOT NULL DEFAULT 'not_submitted' CHECK (submission_status IN ('not_submitted', 'submitting', 'submitted', 'failed')),
    submission_branch text,
    submission_commit_sha text,
    pull_request_number integer,
    pull_request_url text,
    pull_request_state text,
    pull_request_merged boolean NOT NULL DEFAULT false,
    pull_request_checked_at timestamptz,
    pull_request_closed_at timestamptz,
    pull_request_merged_at timestamptz,
    submission_error text,
    submitted_at timestamptz,
    CHECK (expires_at > created_at),
    CHECK (expected_state->>'replicas' <> desired_change->>'replicas'),
    CHECK ((status IN ('approved', 'rejected')) = (decision_at IS NOT NULL AND decision_actor IS NOT NULL AND decision_auth_subject IS NOT NULL)),
    CHECK (decision_actor IS NULL OR length(decision_actor) BETWEEN 1 AND 200),
    CHECK (decision_auth_subject IS NULL OR decision_auth_subject = 'shared-bearer'),
    CHECK (decision_comment IS NULL OR length(decision_comment) <= 2000)
);

CREATE TABLE IF NOT EXISTS nerv.remediation_proposal_evidence (
    proposal_id text NOT NULL REFERENCES nerv.remediation_proposals(id) ON DELETE CASCADE,
    ordinal integer NOT NULL CHECK (ordinal >= 0 AND ordinal < 20),
    incident_evidence_ordinal integer NOT NULL CHECK (incident_evidence_ordinal >= 0),
    source text NOT NULL,
    summary text NOT NULL,
    PRIMARY KEY (proposal_id, ordinal),
    UNIQUE (proposal_id, incident_evidence_ordinal)
);

CREATE TABLE IF NOT EXISTS nerv.findings (
    incident_id text PRIMARY KEY REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    workflow_id text NOT NULL,
    answer text NOT NULL,
    decision jsonb NOT NULL,
    confidence text NOT NULL DEFAULT 'unknown',
    uncertainty jsonb NOT NULL DEFAULT '[]'::jsonb,
    recommended_action text NOT NULL DEFAULT '',
    tools_used jsonb NOT NULL DEFAULT '[]'::jsonb,
    budget jsonb NOT NULL DEFAULT '{}'::jsonb,
    provider text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS nerv.incident_correlations (
    correlation_key text PRIMARY KEY,
    incident_id text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    cooldown_seconds bigint NOT NULL DEFAULT 0,
    cooldown_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nerv.notification_attempts (
    incident_id text NOT NULL REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    sink text NOT NULL,
    external_id text NOT NULL DEFAULT '',
    status text NOT NULL,
    error text NOT NULL DEFAULT '',
    attempted_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nerv.notification_batches (
    id text PRIMARY KEY,
    sink text NOT NULL,
    leader_incident_id text NOT NULL REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    state text NOT NULL DEFAULT 'pending',
    due_at timestamptz NOT NULL,
    external_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nerv.notification_batch_items (
    batch_id text NOT NULL REFERENCES nerv.notification_batches(id) ON DELETE CASCADE,
    incident_id text NOT NULL REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (batch_id, incident_id)
);

CREATE TABLE IF NOT EXISTS nerv.investigation_admission_leases (
    workflow_id text PRIMARY KEY,
    source text NOT NULL,
    admitted_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    released_at timestamptz
);

CREATE TABLE IF NOT EXISTS nerv.investigation_rate_windows (
    source text NOT NULL,
    window_start timestamptz NOT NULL,
    accepted integer NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, window_start)
);

ALTER TABLE nerv.findings ADD COLUMN IF NOT EXISTS confidence text NOT NULL DEFAULT 'unknown';
ALTER TABLE nerv.findings ADD COLUMN IF NOT EXISTS uncertainty jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE nerv.findings ADD COLUMN IF NOT EXISTS recommended_action text NOT NULL DEFAULT '';
ALTER TABLE nerv.findings ADD COLUMN IF NOT EXISTS tools_used jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE nerv.findings ADD COLUMN IF NOT EXISTS budget jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS submission_status text NOT NULL DEFAULT 'not_submitted';
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS submission_branch text;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS submission_commit_sha text;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_number integer;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_url text;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_state text;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_merged boolean NOT NULL DEFAULT false;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_checked_at timestamptz;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_closed_at timestamptz;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS pull_request_merged_at timestamptz;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS submission_error text;
ALTER TABLE nerv.remediation_proposals ADD COLUMN IF NOT EXISTS submitted_at timestamptz;

CREATE INDEX IF NOT EXISTS incidents_status_updated_idx ON nerv.incidents(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS remediation_proposals_incident_created_idx ON nerv.remediation_proposals(incident_id, created_at DESC);
CREATE INDEX IF NOT EXISTS remediation_proposals_pending_expiry_idx ON nerv.remediation_proposals(expires_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS remediation_proposal_evidence_proposal_idx ON nerv.remediation_proposal_evidence(proposal_id, ordinal);
CREATE INDEX IF NOT EXISTS findings_created_at_idx ON nerv.findings(created_at);
CREATE INDEX IF NOT EXISTS incident_correlations_incident_idx ON nerv.incident_correlations(incident_id);
CREATE INDEX IF NOT EXISTS notification_attempts_incident_idx ON nerv.notification_attempts(incident_id, attempted_at DESC);
CREATE INDEX IF NOT EXISTS notification_batches_pending_idx ON nerv.notification_batches(sink, state, created_at);
CREATE INDEX IF NOT EXISTS notification_batch_items_batch_idx ON nerv.notification_batch_items(batch_id, created_at);
CREATE INDEX IF NOT EXISTS investigation_admission_active_idx ON nerv.investigation_admission_leases(expires_at) WHERE released_at IS NULL;
CREATE INDEX IF NOT EXISTS investigation_rate_windows_start_idx ON nerv.investigation_rate_windows(window_start);
`
	if _, err := conn.Exec(ctx, migration); err != nil {
		return fmt.Errorf("migrate NERV schema: %w", err)
	}
	return nil
}

func (p *Postgres) ResolveIncident(ctx context.Context, correlationKey string, cooldown time.Duration) (string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin incident correlation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, correlationKey); err != nil {
		return "", fmt.Errorf("lock incident correlation: %w", err)
	}

	var incidentID string
	var reuse bool
	err = tx.QueryRow(ctx, `
SELECT incident_id, active OR COALESCE(cooldown_until > now(), false)
FROM nerv.incident_correlations
WHERE correlation_key = $1`, correlationKey).Scan(&incidentID, &reuse)
	if err == nil && reuse {
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit incident correlation: %w", err)
		}
		return incidentID, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read incident correlation: %w", err)
	}

	incidentID = uuid.NewString()
	cooldownSeconds := max(int64(cooldown/time.Second), 0)
	_, err = tx.Exec(ctx, `
INSERT INTO nerv.incident_correlations (correlation_key, incident_id, active, cooldown_seconds, cooldown_until, updated_at)
VALUES ($1, $2, true, $3, NULL, now())
ON CONFLICT (correlation_key) DO UPDATE SET
    incident_id = EXCLUDED.incident_id,
    active = true,
    cooldown_seconds = EXCLUDED.cooldown_seconds,
    cooldown_until = NULL,
    updated_at = now()`, correlationKey, incidentID, cooldownSeconds)
	if err != nil {
		return "", fmt.Errorf("save incident correlation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit incident correlation: %w", err)
	}
	return incidentID, nil
}

func (p *Postgres) ReleaseIncident(ctx context.Context, correlationKey, incidentID string) error {
	_, err := p.pool.Exec(ctx, `
DELETE FROM nerv.incident_correlations c
WHERE c.correlation_key = $1 AND c.incident_id = $2
  AND NOT EXISTS (SELECT 1 FROM nerv.incidents i WHERE i.id = c.incident_id)`, correlationKey, incidentID)
	if err != nil {
		return fmt.Errorf("release incident correlation: %w", err)
	}
	return nil
}

func (p *Postgres) SaveIncident(ctx context.Context, workflowID string, req investigation.Request, status string) error {
	target, err := json.Marshal(req.Target)
	if err != nil {
		return fmt.Errorf("encode target: %w", err)
	}
	labels, err := json.Marshal(req.Labels)
	if err != nil {
		return fmt.Errorf("encode labels: %w", err)
	}
	annotations, err := json.Marshal(req.Annotations)
	if err != nil {
		return fmt.Errorf("encode annotations: %w", err)
	}
	_, err = p.pool.Exec(ctx, `
INSERT INTO nerv.incidents (id, workflow_id, source, question, target, labels, annotations, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, updated_at = now()`,
		req.ID, workflowID, req.Source, req.Question, target, labels, annotations, status)
	if err != nil {
		return fmt.Errorf("save incident: %w", err)
	}
	return nil
}

func (p *Postgres) SaveEvidence(ctx context.Context, incidentID string, evidence []investigation.Evidence) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin evidence transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	for i, item := range evidence {
		_, err = tx.Exec(ctx, `
INSERT INTO nerv.evidence (incident_id, ordinal, source, summary)
VALUES ($1, $2, $3, $4)
ON CONFLICT (incident_id, ordinal) DO UPDATE SET source = EXCLUDED.source, summary = EXCLUDED.summary`,
			incidentID, i, item.Source, item.Summary)
		if err != nil {
			return fmt.Errorf("save evidence item %d: %w", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit evidence: %w", err)
	}
	return nil
}

func (p *Postgres) SaveFinding(ctx context.Context, finding investigation.Finding) error {
	if strings.TrimSpace(finding.Confidence) == "" {
		finding.Confidence = "unknown"
	}
	decision, err := json.Marshal(finding.Decision)
	if err != nil {
		return fmt.Errorf("encode decision: %w", err)
	}
	uncertainty, err := json.Marshal(finding.Uncertainty)
	if err != nil {
		return fmt.Errorf("encode uncertainty: %w", err)
	}
	toolsUsed, err := json.Marshal(finding.ToolsUsed)
	if err != nil {
		return fmt.Errorf("encode tools used: %w", err)
	}
	budget, err := json.Marshal(finding.Budget)
	if err != nil {
		return fmt.Errorf("encode budget: %w", err)
	}
	_, err = p.pool.Exec(ctx, `
INSERT INTO nerv.findings (incident_id, workflow_id, answer, decision, confidence, uncertainty, recommended_action, tools_used, budget, provider, model, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (incident_id) DO UPDATE SET
    answer = EXCLUDED.answer,
    decision = EXCLUDED.decision,
    confidence = EXCLUDED.confidence,
    uncertainty = EXCLUDED.uncertainty,
    recommended_action = EXCLUDED.recommended_action,
    tools_used = EXCLUDED.tools_used,
    budget = EXCLUDED.budget,
    provider = EXCLUDED.provider,
    model = EXCLUDED.model,
    created_at = EXCLUDED.created_at`,
		finding.ID, finding.WorkflowID, finding.Answer, decision, finding.Confidence, uncertainty, finding.RecommendedAction, toolsUsed, budget, finding.Provider, finding.Model, finding.CreatedAt)
	if err != nil {
		return fmt.Errorf("save finding: %w", err)
	}
	return nil
}

func (p *Postgres) SetStatus(ctx context.Context, incidentID, status string) error {
	result, err := p.pool.Exec(ctx, `UPDATE nerv.incidents SET status = $2, updated_at = now() WHERE id = $1`, incidentID, status)
	if err != nil {
		return fmt.Errorf("set incident status: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("incident %q not found", incidentID)
	}
	switch status {
	case "completed":
		_, err = p.pool.Exec(ctx, `
UPDATE nerv.incident_correlations
SET active = false,
    cooldown_until = now() + cooldown_seconds * interval '1 second',
    updated_at = now()
WHERE incident_id = $1`, incidentID)
		if err != nil {
			return fmt.Errorf("complete incident correlation: %w", err)
		}
	case "failed":
		_, err = p.pool.Exec(ctx, `
UPDATE nerv.incident_correlations
SET active = false,
    cooldown_until = NULL,
    updated_at = now()
WHERE incident_id = $1`, incidentID)
		if err != nil {
			return fmt.Errorf("fail incident correlation: %w", err)
		}
	}
	return nil
}

func (p *Postgres) GetInvestigation(ctx context.Context, incidentID string) (investigation.Investigation, error) {
	var detail investigation.Investigation
	var targetJSON, labelsJSON, annotationsJSON, decisionJSON, uncertaintyJSON, toolsUsedJSON, budgetJSON []byte
	var finding investigation.Finding
	var hasFinding bool
	err := p.pool.QueryRow(ctx, `
SELECT i.id, i.workflow_id, i.source, i.question, i.status, i.target, i.labels, i.annotations,
       i.created_at, i.updated_at, f.incident_id IS NOT NULL,
       COALESCE(f.answer, ''), COALESCE(f.decision, '{}'::jsonb), COALESCE(f.confidence, 'unknown'),
       COALESCE(f.uncertainty, '[]'::jsonb), COALESCE(f.recommended_action, ''),
       COALESCE(f.tools_used, '[]'::jsonb), COALESCE(f.budget, '{}'::jsonb), COALESCE(f.provider, ''),
       COALESCE(f.model, ''), COALESCE(f.created_at, i.created_at)
FROM nerv.incidents i
LEFT JOIN nerv.findings f ON f.incident_id = i.id
WHERE i.id = $1`, incidentID).Scan(
		&detail.Incident.ID, &detail.Incident.WorkflowID, &detail.Incident.Source,
		&detail.Incident.Question, &detail.Incident.Status, &targetJSON, &labelsJSON,
		&annotationsJSON, &detail.Incident.CreatedAt, &detail.Incident.UpdatedAt, &hasFinding,
		&finding.Answer, &decisionJSON, &finding.Confidence, &uncertaintyJSON, &finding.RecommendedAction,
		&toolsUsedJSON, &budgetJSON, &finding.Provider, &finding.Model, &finding.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Investigation{}, fmt.Errorf("incident %q not found", incidentID)
	}
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("get investigation: %w", err)
	}
	if err := decodeIncidentContext(&detail.Incident, targetJSON, labelsJSON, annotationsJSON); err != nil {
		return investigation.Investigation{}, err
	}

	rows, err := p.pool.Query(ctx, `SELECT source, summary FROM nerv.evidence WHERE incident_id = $1 ORDER BY ordinal`, incidentID)
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("query evidence: %w", err)
	}
	for rows.Next() {
		var item investigation.Evidence
		if err := rows.Scan(&item.Source, &item.Summary); err != nil {
			rows.Close()
			return investigation.Investigation{}, fmt.Errorf("scan evidence: %w", err)
		}
		detail.Evidence = append(detail.Evidence, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return investigation.Investigation{}, fmt.Errorf("read evidence: %w", err)
	}
	rows.Close()

	if hasFinding {
		finding.ID = detail.Incident.ID
		finding.WorkflowID = detail.Incident.WorkflowID
		finding.Source = detail.Incident.Source
		finding.Target = detail.Incident.Target
		finding.Question = detail.Incident.Question
		finding.Evidence = detail.Evidence
		if err := json.Unmarshal(decisionJSON, &finding.Decision); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decode decision: %w", err)
		}
		if err := json.Unmarshal(uncertaintyJSON, &finding.Uncertainty); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decode uncertainty: %w", err)
		}
		if err := json.Unmarshal(toolsUsedJSON, &finding.ToolsUsed); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decode tools used: %w", err)
		}
		if err := json.Unmarshal(budgetJSON, &finding.Budget); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decode budget: %w", err)
		}
		detail.Finding = &finding
	}

	notifications, err := p.pool.Query(ctx, `
SELECT sink, external_id, status, error, attempted_at
FROM nerv.notification_attempts
WHERE incident_id = $1
ORDER BY attempted_at DESC`, incidentID)
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("query notifications: %w", err)
	}
	defer notifications.Close()
	for notifications.Next() {
		var attempt investigation.NotificationAttempt
		if err := notifications.Scan(&attempt.Sink, &attempt.ExternalID, &attempt.Status, &attempt.Error, &attempt.AttemptedAt); err != nil {
			return investigation.Investigation{}, fmt.Errorf("scan notification: %w", err)
		}
		detail.Notifications = append(detail.Notifications, attempt)
	}
	if err := notifications.Err(); err != nil {
		return investigation.Investigation{}, fmt.Errorf("read notifications: %w", err)
	}
	return detail, nil
}

func (p *Postgres) ListIncidents(ctx context.Context, filter investigation.IncidentFilter) (investigation.IncidentPage, error) {
	query, args := incidentListQuery(filter)
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return investigation.IncidentPage{}, fmt.Errorf("list incidents: %w", err)
	}
	defer rows.Close()

	page := investigation.IncidentPage{Limit: filter.Limit, Offset: filter.Offset}
	for rows.Next() {
		var item investigation.IncidentSummary
		var targetJSON, labelsJSON, annotationsJSON []byte
		if err := rows.Scan(
			&item.ID, &item.WorkflowID, &item.Source, &item.Question, &item.Status,
			&targetJSON, &labelsJSON, &annotationsJSON, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return investigation.IncidentPage{}, fmt.Errorf("scan incident: %w", err)
		}
		if len(page.Items) == filter.Limit {
			page.HasMore = true
			continue
		}
		if err := decodeIncidentContext(&item, targetJSON, labelsJSON, annotationsJSON); err != nil {
			return investigation.IncidentPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return investigation.IncidentPage{}, fmt.Errorf("read incidents: %w", err)
	}
	return page, nil
}

func (p *Postgres) ListIncidentFilterOptions(ctx context.Context) (investigation.IncidentFilterOptions, error) {
	queries := []struct {
		name  string
		query string
		dest  *[]string
	}{
		{name: "sources", query: `SELECT DISTINCT source FROM nerv.incidents WHERE source <> '' ORDER BY source`},
		{name: "statuses", query: `SELECT DISTINCT status FROM nerv.incidents WHERE status <> '' ORDER BY status`},
	}
	options := investigation.IncidentFilterOptions{}
	queries[0].dest = &options.Sources
	queries[1].dest = &options.Statuses
	for _, query := range queries {
		values, err := p.queryDistinctStrings(ctx, query.query)
		if err != nil {
			return investigation.IncidentFilterOptions{}, fmt.Errorf("list incident filter %s: %w", query.name, err)
		}
		*query.dest = values
	}
	var err error
	options.Namespaces, err = p.queryDistinctStrings(ctx, `SELECT DISTINCT target->>'namespace' FROM nerv.incidents WHERE COALESCE(target->>'namespace', '') <> '' ORDER BY 1`)
	if err != nil {
		return investigation.IncidentFilterOptions{}, fmt.Errorf("list incident filter namespaces: %w", err)
	}
	options.Workloads, err = p.queryDistinctStrings(ctx, `SELECT DISTINCT target->>'name' FROM nerv.incidents WHERE COALESCE(target->>'name', '') <> '' ORDER BY 1`)
	if err != nil {
		return investigation.IncidentFilterOptions{}, fmt.Errorf("list incident filter workloads: %w", err)
	}
	return options, nil
}

func (p *Postgres) queryDistinctStrings(ctx context.Context, query string) ([]string, error) {
	rows, err := p.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func incidentListQuery(filter investigation.IncidentFilter) (string, []any) {
	var query strings.Builder
	query.WriteString(`
SELECT i.id, i.workflow_id, i.source, i.question, i.status, i.target, i.labels, i.annotations,
       i.created_at, i.updated_at
FROM nerv.incidents i
WHERE true`)
	args := make([]any, 0, 6)
	add := func(clause string, value any) {
		args = append(args, value)
		query.WriteString(clause)
		query.WriteByte('$')
		query.WriteString(strconv.Itoa(len(args)))
	}
	if filter.Source != "" {
		add(" AND i.source = ", filter.Source)
	}
	if filter.Status != "" {
		add(" AND i.status = ", filter.Status)
	}
	if filter.Namespace != "" {
		add(" AND i.target->>'namespace' = ", filter.Namespace)
	}
	if filter.Workload != "" {
		add(" AND i.target->>'name' = ", filter.Workload)
	}
	args = append(args, filter.Limit+1)
	query.WriteString(" ORDER BY i.created_at DESC, i.id DESC LIMIT $")
	query.WriteString(strconv.Itoa(len(args)))
	args = append(args, filter.Offset)
	query.WriteString(" OFFSET $")
	query.WriteString(strconv.Itoa(len(args)))
	return query.String(), args
}

func decodeIncidentContext(item *investigation.IncidentSummary, targetJSON, labelsJSON, annotationsJSON []byte) error {
	if err := json.Unmarshal(targetJSON, &item.Target); err != nil {
		return fmt.Errorf("decode target: %w", err)
	}
	if err := json.Unmarshal(labelsJSON, &item.Labels); err != nil {
		return fmt.Errorf("decode labels: %w", err)
	}
	if err := json.Unmarshal(annotationsJSON, &item.Annotations); err != nil {
		return fmt.Errorf("decode annotations: %w", err)
	}
	return nil
}

func (p *Postgres) SentNotification(ctx context.Context, incidentID, sink string) (string, bool, error) {
	var externalID string
	err := p.pool.QueryRow(ctx, `
SELECT external_id FROM nerv.notification_attempts
WHERE incident_id = $1 AND sink = $2 AND status = 'sent'
ORDER BY attempted_at DESC LIMIT 1`, incidentID, sink).Scan(&externalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find sent notification: %w", err)
	}
	return externalID, true, nil
}

func (p *Postgres) RecordNotification(ctx context.Context, incidentID, sink, externalID, status, errorMessage string) error {
	_, err := p.pool.Exec(ctx, `
INSERT INTO nerv.notification_attempts (incident_id, sink, external_id, status, error)
VALUES ($1, $2, $3, $4, $5)`, incidentID, sink, externalID, status, errorMessage)
	if err != nil {
		return fmt.Errorf("record notification attempt: %w", err)
	}
	return nil
}

func (p *Postgres) QueueNotificationBatch(ctx context.Context, incidentID, sink string, debounce time.Duration) (string, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin notification batch transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "notification-batch:"+sink); err != nil {
		return "", false, fmt.Errorf("lock notification batch: %w", err)
	}

	var batchID, leaderIncidentID string
	err = tx.QueryRow(ctx, `
SELECT id, leader_incident_id
FROM nerv.notification_batches
WHERE sink = $1 AND state = 'pending'
ORDER BY created_at
LIMIT 1
FOR UPDATE`, sink).Scan(&batchID, &leaderIncidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		batchID = uuid.NewString()
		leaderIncidentID = incidentID
		_, err = tx.Exec(ctx, `
INSERT INTO nerv.notification_batches (id, sink, leader_incident_id, due_at)
VALUES ($1, $2, $3, now() + $4 * interval '1 millisecond')`, batchID, sink, incidentID, debounce.Milliseconds())
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve notification batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO nerv.notification_batch_items (batch_id, incident_id)
VALUES ($1, $2)
ON CONFLICT (batch_id, incident_id) DO NOTHING`, batchID, incidentID); err != nil {
		return "", false, fmt.Errorf("add notification batch item: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE nerv.notification_batches
SET due_at = now() + $2 * interval '1 millisecond', updated_at = now()
WHERE id = $1`, batchID, debounce.Milliseconds()); err != nil {
		return "", false, fmt.Errorf("extend notification batch debounce: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit notification batch: %w", err)
	}
	return batchID, leaderIncidentID == incidentID, nil
}

func (p *Postgres) GetNotificationBatch(ctx context.Context, batchID string) (investigation.NotificationBatch, error) {
	batch := investigation.NotificationBatch{ID: batchID}
	var state string
	if err := p.pool.QueryRow(ctx, `
SELECT due_at, state, external_id
FROM nerv.notification_batches
WHERE id = $1`, batchID).Scan(&batch.DueAt, &state, &batch.ExternalID); err != nil {
		return investigation.NotificationBatch{}, fmt.Errorf("get notification batch: %w", err)
	}
	batch.Sent = state == "sent"
	if batch.Sent || time.Now().Before(batch.DueAt) {
		return batch, nil
	}

	rows, err := p.pool.Query(ctx, `
SELECT i.id, i.workflow_id, i.question, i.target, f.decision->>'action'
FROM nerv.notification_batch_items bi
JOIN nerv.incidents i ON i.id = bi.incident_id
JOIN nerv.findings f ON f.incident_id = i.id
WHERE bi.batch_id = $1
ORDER BY bi.created_at, i.id`, batchID)
	if err != nil {
		return investigation.NotificationBatch{}, fmt.Errorf("list notification batch items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item investigation.NotificationBatchItem
		var targetJSON []byte
		if err := rows.Scan(&item.IncidentID, &item.WorkflowID, &item.Question, &targetJSON, &item.Action); err != nil {
			return investigation.NotificationBatch{}, fmt.Errorf("scan notification batch item: %w", err)
		}
		if err := json.Unmarshal(targetJSON, &item.Target); err != nil {
			return investigation.NotificationBatch{}, fmt.Errorf("decode notification batch target: %w", err)
		}
		batch.Items = append(batch.Items, item)
	}
	if err := rows.Err(); err != nil {
		return investigation.NotificationBatch{}, fmt.Errorf("read notification batch items: %w", err)
	}
	return batch, nil
}

func (p *Postgres) FailNotificationBatch(ctx context.Context, batchID string) error {
	_, err := p.pool.Exec(ctx, `
UPDATE nerv.notification_batches
SET state = 'failed', updated_at = now()
WHERE id = $1 AND state = 'pending'`, batchID)
	if err != nil {
		return fmt.Errorf("fail notification batch: %w", err)
	}
	return nil
}

func (p *Postgres) CompleteNotificationBatch(ctx context.Context, batchID, sink, externalID string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete notification batch transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `
UPDATE nerv.notification_batches
SET state = 'sent', external_id = $2, updated_at = now()
WHERE id = $1 AND state = 'pending'`, batchID, externalID)
	if err != nil {
		return fmt.Errorf("complete notification batch: %w", err)
	}
	if result.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO nerv.notification_attempts (incident_id, sink, external_id, status)
SELECT incident_id, $2, $3, 'sent'
FROM nerv.notification_batch_items
WHERE batch_id = $1`, batchID, sink, externalID); err != nil {
			return fmt.Errorf("record notification batch attempts: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completed notification batch: %w", err)
	}
	return nil
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

func (p *Postgres) migrate(ctx context.Context) error {
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

CREATE TABLE IF NOT EXISTS nerv.findings (
    incident_id text PRIMARY KEY REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    workflow_id text NOT NULL,
    answer text NOT NULL,
    decision jsonb NOT NULL,
    uncertainty jsonb NOT NULL DEFAULT '[]'::jsonb,
    provider text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS nerv.notification_attempts (
    incident_id text NOT NULL REFERENCES nerv.incidents(id) ON DELETE CASCADE,
    sink text NOT NULL,
    external_id text NOT NULL DEFAULT '',
    status text NOT NULL,
    error text NOT NULL DEFAULT '',
    attempted_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS incidents_status_updated_idx ON nerv.incidents(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS notification_attempts_incident_idx ON nerv.notification_attempts(incident_id, attempted_at DESC);
`
	if _, err := p.pool.Exec(ctx, migration); err != nil {
		return fmt.Errorf("migrate NERV schema: %w", err)
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
	decision, err := json.Marshal(finding.Decision)
	if err != nil {
		return fmt.Errorf("encode decision: %w", err)
	}
	uncertainty, err := json.Marshal(finding.Uncertainty)
	if err != nil {
		return fmt.Errorf("encode uncertainty: %w", err)
	}
	_, err = p.pool.Exec(ctx, `
INSERT INTO nerv.findings (incident_id, workflow_id, answer, decision, uncertainty, provider, model, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (incident_id) DO UPDATE SET
    answer = EXCLUDED.answer,
    decision = EXCLUDED.decision,
    uncertainty = EXCLUDED.uncertainty,
    provider = EXCLUDED.provider,
    model = EXCLUDED.model,
    created_at = EXCLUDED.created_at`,
		finding.ID, finding.WorkflowID, finding.Answer, decision, uncertainty, finding.Provider, finding.Model, finding.CreatedAt)
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
	return nil
}

func (p *Postgres) GetFinding(ctx context.Context, incidentID string) (investigation.Finding, string, error) {
	var finding investigation.Finding
	var status string
	var targetJSON, decisionJSON, uncertaintyJSON []byte
	err := p.pool.QueryRow(ctx, `
SELECT i.id, i.workflow_id, i.question, i.status, i.target,
       COALESCE(f.answer, ''), COALESCE(f.decision, '{}'::jsonb),
       COALESCE(f.uncertainty, '[]'::jsonb), COALESCE(f.provider, ''),
       COALESCE(f.model, ''), COALESCE(f.created_at, i.created_at)
FROM nerv.incidents i
LEFT JOIN nerv.findings f ON f.incident_id = i.id
WHERE i.id = $1`, incidentID).Scan(
		&finding.ID, &finding.WorkflowID, &finding.Question, &status, &targetJSON,
		&finding.Answer, &decisionJSON, &uncertaintyJSON, &finding.Provider,
		&finding.Model, &finding.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Finding{}, "", fmt.Errorf("incident %q not found", incidentID)
	}
	if err != nil {
		return investigation.Finding{}, "", fmt.Errorf("get finding: %w", err)
	}
	if err := json.Unmarshal(decisionJSON, &finding.Decision); err != nil {
		return investigation.Finding{}, "", fmt.Errorf("decode decision: %w", err)
	}
	if err := json.Unmarshal(uncertaintyJSON, &finding.Uncertainty); err != nil {
		return investigation.Finding{}, "", fmt.Errorf("decode uncertainty: %w", err)
	}

	rows, err := p.pool.Query(ctx, `SELECT source, summary FROM nerv.evidence WHERE incident_id = $1 ORDER BY ordinal`, incidentID)
	if err != nil {
		return investigation.Finding{}, "", fmt.Errorf("query evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item investigation.Evidence
		if err := rows.Scan(&item.Source, &item.Summary); err != nil {
			return investigation.Finding{}, "", fmt.Errorf("scan evidence: %w", err)
		}
		finding.Evidence = append(finding.Evidence, item)
	}
	return finding, status, rows.Err()
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

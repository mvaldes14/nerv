package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mvaldes14/nerv/internal/remediation"
)

const proposalColumns = `
id, incident_id, action_kind, target_id, target_repository, target_path, target_resource,
expected_state, desired_change, summary, rationale, risk_level, risk_detail, rendered_before,
rendered_after, rendered_diff, render_format, status, created_at, updated_at, expires_at,
COALESCE(decision_actor, ''), COALESCE(decision_auth_subject, ''), COALESCE(decision_comment, ''), decision_at`

func (p *Postgres) CreateProposal(ctx context.Context, proposal remediation.Proposal, evidenceOrdinals []int) (remediation.Proposal, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("begin remediation proposal transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var status string
	var hasFinding bool
	err = tx.QueryRow(ctx, `
SELECT i.status, EXISTS (SELECT 1 FROM nerv.findings f WHERE f.incident_id = i.id)
FROM nerv.incidents i
WHERE i.id = $1`, proposal.IncidentID).Scan(&status, &hasFinding)
	if errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("%w: incident %q", remediation.ErrNotFound, proposal.IncidentID)
	}
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("read remediation incident: %w", err)
	}
	if status != "completed" || !hasFinding {
		return remediation.Proposal{}, fmt.Errorf("%w: incident must be completed with a finding", remediation.ErrConflict)
	}

	proposal.ID = uuid.NewString()
	resourceJSON, err := json.Marshal(proposal.TargetResource)
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("encode remediation target resource: %w", err)
	}
	expectedJSON, err := json.Marshal(proposal.ExpectedState)
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("encode remediation expected state: %w", err)
	}
	desiredJSON, err := json.Marshal(proposal.DesiredChange)
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("encode remediation desired change: %w", err)
	}
	row := tx.QueryRow(ctx, `
INSERT INTO nerv.remediation_proposals (
    id, incident_id, action_kind, target_id, target_repository, target_path, target_resource,
    expected_state, desired_change, summary, rationale, risk_level, risk_detail, rendered_before,
    rendered_after, rendered_diff, render_format, status, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, 'pending', $18)
RETURNING `+proposalColumns,
		proposal.ID, proposal.IncidentID, proposal.ActionKind, proposal.TargetID, proposal.TargetRepository,
		proposal.TargetPath, resourceJSON, expectedJSON, desiredJSON, proposal.Summary, proposal.Rationale,
		proposal.RiskLevel, proposal.RiskDetail, proposal.RenderedBefore, proposal.RenderedAfter,
		proposal.RenderedDiff, proposal.RenderFormat, proposal.ExpiresAt)
	created, err := scanProposal(row)
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("save remediation proposal: %w", err)
	}
	for ordinal, incidentOrdinal := range evidenceOrdinals {
		var source, summary string
		err := tx.QueryRow(ctx, `SELECT source, summary FROM nerv.evidence WHERE incident_id = $1 AND ordinal = $2`, proposal.IncidentID, incidentOrdinal).Scan(&source, &summary)
		if errors.Is(err, pgx.ErrNoRows) {
			return remediation.Proposal{}, &remediation.ValidationError{Message: fmt.Sprintf("evidence ordinal %d does not exist", incidentOrdinal)}
		}
		if err != nil {
			return remediation.Proposal{}, fmt.Errorf("read incident evidence %d: %w", incidentOrdinal, err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO nerv.remediation_proposal_evidence (proposal_id, ordinal, incident_evidence_ordinal, source, summary)
VALUES ($1, $2, $3, $4, $5)`, proposal.ID, ordinal, incidentOrdinal, source, summary); err != nil {
			return remediation.Proposal{}, fmt.Errorf("save remediation evidence %d: %w", incidentOrdinal, err)
		}
	}
	created.Evidence, err = queryProposalEvidence(ctx, tx, proposal.ID)
	if err != nil {
		return remediation.Proposal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return remediation.Proposal{}, fmt.Errorf("commit remediation proposal: %w", err)
	}
	return created, nil
}

func (p *Postgres) ListProposals(ctx context.Context, incidentID string, limit int) ([]remediation.Proposal, error) {
	if _, err := p.pool.Exec(ctx, `
UPDATE nerv.remediation_proposals
SET status = 'expired', updated_at = now()
WHERE incident_id = $1 AND status = 'pending' AND expires_at <= now()`, incidentID); err != nil {
		return nil, fmt.Errorf("expire remediation proposals: %w", err)
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM nerv.incidents WHERE id = $1)`, incidentID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check remediation incident: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: incident %q", remediation.ErrNotFound, incidentID)
	}
	rows, err := p.pool.Query(ctx, `SELECT `+proposalColumns+` FROM nerv.remediation_proposals WHERE incident_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, incidentID, limit)
	if err != nil {
		return nil, fmt.Errorf("query remediation proposals: %w", err)
	}
	proposals := make([]remediation.Proposal, 0)
	for rows.Next() {
		proposal, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan remediation proposal: %w", err)
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read remediation proposals: %w", err)
	}
	rows.Close()
	for i := range proposals {
		proposals[i].Evidence, err = queryProposalEvidence(ctx, p.pool, proposals[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return proposals, nil
}

func (p *Postgres) GetProposal(ctx context.Context, proposalID string) (remediation.Proposal, error) {
	if _, err := p.pool.Exec(ctx, `
UPDATE nerv.remediation_proposals
SET status = 'expired', updated_at = now()
WHERE id = $1 AND status = 'pending' AND expires_at <= now()`, proposalID); err != nil {
		return remediation.Proposal{}, fmt.Errorf("expire remediation proposal: %w", err)
	}
	proposal, err := scanProposal(p.pool.QueryRow(ctx, `SELECT `+proposalColumns+` FROM nerv.remediation_proposals WHERE id = $1`, proposalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal %q", remediation.ErrNotFound, proposalID)
	}
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("get remediation proposal: %w", err)
	}
	proposal.Evidence, err = queryProposalEvidence(ctx, p.pool, proposal.ID)
	if err != nil {
		return remediation.Proposal{}, err
	}
	return proposal, nil
}

func (p *Postgres) DecideProposal(ctx context.Context, proposalID, status, actor, authSubject, comment string) (remediation.Proposal, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("begin remediation decision transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	row := tx.QueryRow(ctx, `
UPDATE nerv.remediation_proposals
SET status = $2, decision_actor = $3, decision_auth_subject = $4, decision_comment = $5,
    decision_at = now(), updated_at = now()
WHERE id = $1 AND status = 'pending' AND expires_at > now()
RETURNING `+proposalColumns, proposalID, status, actor, authSubject, comment)
	proposal, err := scanProposal(row)
	if err == nil {
		proposal.Evidence, err = queryProposalEvidence(ctx, tx, proposal.ID)
		if err != nil {
			return remediation.Proposal{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return remediation.Proposal{}, fmt.Errorf("commit remediation decision: %w", err)
		}
		return proposal, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("decide remediation proposal: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE nerv.remediation_proposals
SET status = 'expired', updated_at = now()
WHERE id = $1 AND status = 'pending' AND expires_at <= now()`, proposalID); err != nil {
		return remediation.Proposal{}, fmt.Errorf("expire remediation proposal during decision: %w", err)
	}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM nerv.remediation_proposals WHERE id = $1`, proposalID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal %q", remediation.ErrNotFound, proposalID)
	}
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("read remediation decision conflict: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return remediation.Proposal{}, fmt.Errorf("commit remediation decision conflict: %w", err)
	}
	return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal is %s", remediation.ErrConflict, currentStatus)
}

type proposalScanner interface {
	Scan(dest ...any) error
}

func scanProposal(row proposalScanner) (remediation.Proposal, error) {
	var proposal remediation.Proposal
	var resourceJSON, expectedJSON, desiredJSON []byte
	if err := row.Scan(
		&proposal.ID, &proposal.IncidentID, &proposal.ActionKind, &proposal.TargetID,
		&proposal.TargetRepository, &proposal.TargetPath, &resourceJSON, &expectedJSON, &desiredJSON,
		&proposal.Summary, &proposal.Rationale, &proposal.RiskLevel, &proposal.RiskDetail,
		&proposal.RenderedBefore, &proposal.RenderedAfter, &proposal.RenderedDiff, &proposal.RenderFormat,
		&proposal.Status, &proposal.CreatedAt, &proposal.UpdatedAt, &proposal.ExpiresAt,
		&proposal.DecisionActor, &proposal.DecisionAuthSubject, &proposal.DecisionComment, &proposal.DecisionAt,
	); err != nil {
		return remediation.Proposal{}, err
	}
	if err := json.Unmarshal(resourceJSON, &proposal.TargetResource); err != nil {
		return remediation.Proposal{}, fmt.Errorf("decode remediation target resource: %w", err)
	}
	if err := json.Unmarshal(expectedJSON, &proposal.ExpectedState); err != nil {
		return remediation.Proposal{}, fmt.Errorf("decode remediation expected state: %w", err)
	}
	if err := json.Unmarshal(desiredJSON, &proposal.DesiredChange); err != nil {
		return remediation.Proposal{}, fmt.Errorf("decode remediation desired change: %w", err)
	}
	return proposal, nil
}

type evidenceQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryProposalEvidence(ctx context.Context, querier evidenceQuerier, proposalID string) ([]remediation.Evidence, error) {
	rows, err := querier.Query(ctx, `
SELECT ordinal, incident_evidence_ordinal, source, summary
FROM nerv.remediation_proposal_evidence
WHERE proposal_id = $1
ORDER BY ordinal`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("query remediation proposal evidence: %w", err)
	}
	defer rows.Close()
	var evidence []remediation.Evidence
	for rows.Next() {
		var item remediation.Evidence
		if err := rows.Scan(&item.Ordinal, &item.IncidentEvidenceOrdinal, &item.Source, &item.Summary); err != nil {
			return nil, fmt.Errorf("scan remediation proposal evidence: %w", err)
		}
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read remediation proposal evidence: %w", err)
	}
	return evidence, nil
}

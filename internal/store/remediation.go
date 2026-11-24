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
COALESCE(decision_actor, ''), COALESCE(decision_auth_subject, ''), COALESCE(decision_comment, ''), decision_at,
submission_status, COALESCE(submission_branch, ''), COALESCE(submission_commit_sha, ''),
COALESCE(pull_request_number, 0), COALESCE(pull_request_url, ''), COALESCE(pull_request_state, ''),
pull_request_merged, pull_request_checked_at, pull_request_closed_at, pull_request_merged_at,
COALESCE(submission_error, ''), submitted_at`

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

func (p *Postgres) ListAllProposals(ctx context.Context, status string, limit int) ([]remediation.Proposal, error) {
	if _, err := p.pool.Exec(ctx, `
UPDATE nerv.remediation_proposals
SET status = 'expired', updated_at = now()
WHERE status = 'pending' AND expires_at <= now()`); err != nil {
		return nil, fmt.Errorf("expire remediation proposals: %w", err)
	}
	rows, err := p.pool.Query(ctx, `
SELECT `+proposalColumns+`
FROM nerv.remediation_proposals
WHERE $1 = '' OR status = $1
ORDER BY
    CASE status WHEN 'pending' THEN 0 WHEN 'approved' THEN 1 ELSE 2 END,
    created_at DESC, id DESC
LIMIT $2`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("query remediation proposal queue: %w", err)
	}
	defer rows.Close()
	proposals := make([]remediation.Proposal, 0)
	for rows.Next() {
		proposal, scanErr := scanProposal(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan remediation proposal queue: %w", scanErr)
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read remediation proposal queue: %w", err)
	}
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

func (p *Postgres) BeginProposalSubmission(ctx context.Context, proposalID string) (remediation.Proposal, error) {
	row := p.pool.QueryRow(ctx, `
UPDATE nerv.remediation_proposals
SET submission_status = 'submitting', submission_error = NULL, updated_at = now()
WHERE id = $1 AND status = 'approved' AND (
    submission_status IN ('not_submitted', 'failed')
    OR (submission_status = 'submitting' AND updated_at < now() - interval '10 minutes')
)
RETURNING `+proposalColumns, proposalID)
	proposal, err := scanProposal(row)
	if err == nil {
		proposal.Evidence, err = queryProposalEvidence(ctx, p.pool, proposal.ID)
		return proposal, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("begin remediation submission: %w", err)
	}
	var status, submissionStatus string
	var existing remediation.Proposal
	existing, err = p.GetProposal(ctx, proposalID)
	if err != nil {
		return remediation.Proposal{}, err
	}
	status, submissionStatus = existing.Status, existing.SubmissionStatus
	if status != remediation.StatusApproved {
		return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal is %s", remediation.ErrConflict, status)
	}
	if submissionStatus == remediation.SubmissionSubmitted {
		return existing, nil
	}
	return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal submission is %s", remediation.ErrConflict, submissionStatus)
}

func (p *Postgres) CompleteProposalSubmission(ctx context.Context, proposalID string, result remediation.SubmissionResult) (remediation.Proposal, error) {
	proposal, err := scanProposal(p.pool.QueryRow(ctx, `
UPDATE nerv.remediation_proposals
SET submission_status = 'submitted', submission_branch = $2, submission_commit_sha = $3,
    pull_request_number = $4, pull_request_url = $5, pull_request_state = 'open',
    pull_request_merged = false, pull_request_checked_at = now(), submission_error = NULL,
    submitted_at = now(), updated_at = now()
WHERE id = $1 AND status = 'approved' AND submission_status = 'submitting'
RETURNING `+proposalColumns, proposalID, result.Branch, result.CommitSHA, result.PullRequestNumber, result.PullRequestURL))
	if errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal submission cannot be completed", remediation.ErrConflict)
	}
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("complete remediation submission: %w", err)
	}
	proposal.Evidence, err = queryProposalEvidence(ctx, p.pool, proposal.ID)
	return proposal, err
}

func (p *Postgres) UpdateProposalPullRequestStatus(ctx context.Context, proposalID string, status remediation.PullRequestStatus) (remediation.Proposal, error) {
	proposal, err := scanProposal(p.pool.QueryRow(ctx, `
UPDATE nerv.remediation_proposals
SET pull_request_state = $2, pull_request_merged = $3, pull_request_checked_at = now(),
    pull_request_closed_at = $4, pull_request_merged_at = $5, updated_at = now()
WHERE id = $1 AND submission_status = 'submitted'
RETURNING `+proposalColumns, proposalID, status.State, status.Merged, status.ClosedAt, status.MergedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return remediation.Proposal{}, fmt.Errorf("%w: remediation proposal has no submitted pull request", remediation.ErrConflict)
	}
	if err != nil {
		return remediation.Proposal{}, fmt.Errorf("update remediation pull request status: %w", err)
	}
	proposal.Evidence, err = queryProposalEvidence(ctx, p.pool, proposal.ID)
	return proposal, err
}

func (p *Postgres) FailProposalSubmission(ctx context.Context, proposalID, message string) error {
	command, err := p.pool.Exec(ctx, `
UPDATE nerv.remediation_proposals
SET submission_status = 'failed', submission_error = $2, updated_at = now()
WHERE id = $1 AND status = 'approved' AND submission_status = 'submitting'`, proposalID, message)
	if err != nil {
		return fmt.Errorf("fail remediation submission: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: remediation proposal submission cannot be failed", remediation.ErrConflict)
	}
	return nil
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
		&proposal.SubmissionStatus, &proposal.SubmissionBranch, &proposal.SubmissionCommitSHA,
		&proposal.PullRequestNumber, &proposal.PullRequestURL, &proposal.PullRequestState,
		&proposal.PullRequestMerged, &proposal.PullRequestCheckedAt, &proposal.PullRequestClosedAt, &proposal.PullRequestMergedAt,
		&proposal.SubmissionError, &proposal.SubmittedAt,
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

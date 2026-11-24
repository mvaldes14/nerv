package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	ActionSetWorkloadReplicas = "set_workload_replicas/v1"
	RenderManifestProjection  = "manifest_projection/v1"

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"

	SubmissionNotSubmitted = "not_submitted"
	SubmissionSubmitting   = "submitting"
	SubmissionSubmitted    = "submitted"
	SubmissionFailed       = "failed"

	DefaultTTL = 24 * time.Hour
	MinTTL     = 5 * time.Minute
	MaxTTL     = 7 * 24 * time.Hour

	MaxProposals        = 50
	MaxEvidence         = 20
	MaxSummaryLength    = 200
	MaxRationaleLength  = 4000
	MaxRiskDetailLength = 2000
	MinReplicas         = 0
	MaxReplicas         = 1000

	RiskLevelLow    = "low"
	RiskLevelMedium = "medium"
	RiskLevelHigh   = "high"
)

var (
	ErrNotFound = errors.New("remediation proposal not found")
	ErrConflict = errors.New("remediation proposal conflict")

	tracer            = otel.Tracer("github.com/mvaldes14/nerv/internal/remediation")
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

type Resource struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

type Target struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Resource Resource `json:"resource"`
}

type Config struct {
	Repository string   `json:"repository"`
	Targets    []Target `json:"targets"`
}

func ParseConfig(repository, targetsJSON string) (Config, error) {
	config := Config{Repository: strings.TrimSpace(repository)}
	targetsJSON = strings.TrimSpace(targetsJSON)
	if config.Repository == "" && targetsJSON == "" {
		return config, nil
	}
	if config.Repository == "" || targetsJSON == "" {
		return Config{}, fmt.Errorf("NERV_GITOPS_REPOSITORY and NERV_GITOPS_TARGETS_JSON must be configured together")
	}
	decoder := json.NewDecoder(strings.NewReader(targetsJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config.Targets); err != nil {
		return Config{}, fmt.Errorf("decode NERV_GITOPS_TARGETS_JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Config{}, fmt.Errorf("decode NERV_GITOPS_TARGETS_JSON: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.Repository == "" && len(c.Targets) == 0 {
		return nil
	}
	if !repositoryPattern.MatchString(c.Repository) {
		return fmt.Errorf("NERV_GITOPS_REPOSITORY must be an owner/repository name")
	}
	if len(c.Targets) == 0 {
		return fmt.Errorf("NERV_GITOPS_TARGETS_JSON must contain at least one target")
	}
	seen := make(map[string]struct{}, len(c.Targets))
	for i, target := range c.Targets {
		if target.ID == "" || len(target.ID) > 128 {
			return fmt.Errorf("GitOps target %d id must contain 1 to 128 characters", i)
		}
		if _, exists := seen[target.ID]; exists {
			return fmt.Errorf("GitOps target id %q is duplicated", target.ID)
		}
		seen[target.ID] = struct{}{}
		if err := validateTargetPath(target.Path); err != nil {
			return fmt.Errorf("GitOps target %q path: %w", target.ID, err)
		}
		if target.Resource.APIVersion != "apps/v1" {
			return fmt.Errorf("GitOps target %q api_version must be apps/v1", target.ID)
		}
		if target.Resource.Kind != "Deployment" && target.Resource.Kind != "StatefulSet" {
			return fmt.Errorf("GitOps target %q kind must be Deployment or StatefulSet", target.ID)
		}
		if strings.TrimSpace(target.Resource.Namespace) == "" || strings.TrimSpace(target.Resource.Name) == "" {
			return fmt.Errorf("GitOps target %q namespace and name are required", target.ID)
		}
		if len(target.Resource.Namespace) > 253 || len(target.Resource.Name) > 253 {
			return fmt.Errorf("GitOps target %q namespace and name must not exceed 253 characters", target.ID)
		}
	}
	return nil
}

func validateTargetPath(value string) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	if strings.Contains(value, `\`) {
		return errors.New("must use forward slashes")
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return errors.New("must be relative")
	}
	segments := strings.Split(value, "/")
	for _, segment := range segments {
		if segment == "" {
			return errors.New("must not contain empty segments")
		}
		if segment == ".." {
			return errors.New("must not contain parent traversal")
		}
	}
	if cleaned := path.Clean(value); cleaned != value || cleaned == "." {
		return errors.New("must be a clean relative path")
	}
	ext := strings.ToLower(path.Ext(value))
	if ext != ".yaml" && ext != ".yml" {
		return errors.New("must end in .yaml or .yml")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

type ReplicaInput struct {
	Replicas *int `json:"replicas"`
}

type ReplicaState struct {
	Replicas int `json:"replicas"`
}

type RiskInput struct {
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

type CreateRequest struct {
	ActionKind       string       `json:"action_kind"`
	TargetID         string       `json:"target_id"`
	Expected         ReplicaInput `json:"expected"`
	Desired          ReplicaInput `json:"desired"`
	Summary          string       `json:"summary"`
	Rationale        string       `json:"rationale"`
	Risk             RiskInput    `json:"risk"`
	EvidenceOrdinals []int        `json:"evidence_ordinals"`
	TTLSeconds       int          `json:"ttl_seconds,omitempty"`
}

type DecisionRequest struct {
	Actor   string `json:"actor"`
	Comment string `json:"comment"`
}

type Evidence struct {
	Ordinal                 int    `json:"ordinal"`
	IncidentEvidenceOrdinal int    `json:"incident_evidence_ordinal"`
	Source                  string `json:"source"`
	Summary                 string `json:"summary"`
}

type SubmissionRequest struct {
	ProposalID       string
	Repository       string
	Path             string
	Resource         Resource
	ExpectedReplicas int
	DesiredReplicas  int
	Summary          string
	Rationale        string
}

type SubmissionResult struct {
	Branch            string
	CommitSHA         string
	PullRequestNumber int
	PullRequestURL    string
}

type Submitter interface {
	Submit(context.Context, SubmissionRequest) (SubmissionResult, error)
}

type Proposal struct {
	ID                  string       `json:"id"`
	IncidentID          string       `json:"incident_id"`
	ActionKind          string       `json:"action_kind"`
	TargetID            string       `json:"target_id"`
	TargetRepository    string       `json:"target_repository"`
	TargetPath          string       `json:"target_path"`
	TargetResource      Resource     `json:"target_resource"`
	ExpectedState       ReplicaState `json:"expected_state"`
	DesiredChange       ReplicaState `json:"desired_change"`
	Summary             string       `json:"summary"`
	Rationale           string       `json:"rationale"`
	RiskLevel           string       `json:"risk_level"`
	RiskDetail          string       `json:"risk_detail"`
	RenderedBefore      string       `json:"rendered_before"`
	RenderedAfter       string       `json:"rendered_after"`
	RenderedDiff        string       `json:"rendered_diff"`
	RenderFormat        string       `json:"render_format"`
	Status              string       `json:"status"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
	ExpiresAt           time.Time    `json:"expires_at"`
	DecisionActor       string       `json:"decision_actor,omitempty"`
	DecisionAuthSubject string       `json:"decision_auth_subject,omitempty"`
	DecisionComment     string       `json:"decision_comment,omitempty"`
	DecisionAt          *time.Time   `json:"decision_at,omitempty"`
	SubmissionStatus    string       `json:"submission_status"`
	SubmissionBranch    string       `json:"submission_branch,omitempty"`
	SubmissionCommitSHA string       `json:"submission_commit_sha,omitempty"`
	PullRequestNumber   int          `json:"pull_request_number,omitempty"`
	PullRequestURL      string       `json:"pull_request_url,omitempty"`
	SubmissionError     string       `json:"submission_error,omitempty"`
	SubmittedAt         *time.Time   `json:"submitted_at,omitempty"`
	Evidence            []Evidence   `json:"evidence"`
}

type Store interface {
	CreateProposal(ctx context.Context, proposal Proposal, evidenceOrdinals []int) (Proposal, error)
	ListProposals(ctx context.Context, incidentID string, limit int) ([]Proposal, error)
	ListAllProposals(ctx context.Context, status string, limit int) ([]Proposal, error)
	GetProposal(ctx context.Context, proposalID string) (Proposal, error)
	DecideProposal(ctx context.Context, proposalID, status, actor, authSubject, comment string) (Proposal, error)
	BeginProposalSubmission(ctx context.Context, proposalID string) (Proposal, error)
	CompleteProposalSubmission(ctx context.Context, proposalID string, result SubmissionResult) (Proposal, error)
	FailProposalSubmission(ctx context.Context, proposalID, message string) error
}

type Service struct {
	config    Config
	targets   map[string]Target
	store     Store
	submitter Submitter
}

func NewService(config Config, store Store, submitters ...Submitter) (*Service, error) {
	if store == nil {
		return nil, errors.New("remediation store is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	targets := make(map[string]Target, len(config.Targets))
	for _, target := range config.Targets {
		targets[target.ID] = target
	}
	var submitter Submitter
	if len(submitters) > 0 {
		submitter = submitters[0]
	}
	return &Service{config: config, targets: targets, store: store, submitter: submitter}, nil
}

func (s *Service) Create(ctx context.Context, incidentID string, req CreateRequest) (Proposal, error) {
	ctx, span := tracer.Start(ctx, "remediation.create")
	defer span.End()

	target, expected, desired, ttl, err := s.validateCreate(incidentID, req)
	if err != nil {
		recordError(span, err)
		return Proposal{}, err
	}
	before, err := renderManifest(target.Resource, expected)
	if err != nil {
		recordError(span, err)
		return Proposal{}, err
	}
	after, err := renderManifest(target.Resource, desired)
	if err != nil {
		recordError(span, err)
		return Proposal{}, err
	}
	proposal := Proposal{
		IncidentID:       incidentID,
		ActionKind:       req.ActionKind,
		TargetID:         target.ID,
		TargetRepository: s.config.Repository,
		TargetPath:       target.Path,
		TargetResource:   target.Resource,
		ExpectedState:    ReplicaState{Replicas: expected},
		DesiredChange:    ReplicaState{Replicas: desired},
		Summary:          strings.TrimSpace(req.Summary),
		Rationale:        strings.TrimSpace(req.Rationale),
		RiskLevel:        req.Risk.Level,
		RiskDetail:       strings.TrimSpace(req.Risk.Detail),
		RenderedBefore:   before,
		RenderedAfter:    after,
		RenderedDiff:     renderDiff(target.Path, before, after),
		RenderFormat:     RenderManifestProjection,
		Status:           StatusPending,
		ExpiresAt:        time.Now().UTC().Add(ttl),
	}
	created, err := s.store.CreateProposal(ctx, proposal, req.EvidenceOrdinals)
	if err != nil {
		recordError(span, err)
		return Proposal{}, err
	}
	return created, nil
}

func (s *Service) List(ctx context.Context, incidentID string) ([]Proposal, error) {
	ctx, span := tracer.Start(ctx, "remediation.list")
	defer span.End()
	if strings.TrimSpace(incidentID) == "" {
		err := invalid("incident id is required")
		recordError(span, err)
		return nil, err
	}
	proposals, err := s.store.ListProposals(ctx, incidentID, MaxProposals)
	if err != nil {
		recordError(span, err)
	}
	return proposals, err
}

func (s *Service) ListAll(ctx context.Context, status string) ([]Proposal, error) {
	ctx, span := tracer.Start(ctx, "remediation.list_all")
	defer span.End()
	status = strings.TrimSpace(status)
	if status != "" && status != StatusPending && status != StatusApproved && status != StatusRejected && status != StatusExpired {
		err := invalid("status must be pending, approved, rejected, or expired")
		recordError(span, err)
		return nil, err
	}
	proposals, err := s.store.ListAllProposals(ctx, status, MaxProposals)
	if err != nil {
		recordError(span, err)
	}
	return proposals, err
}

func (s *Service) Get(ctx context.Context, proposalID string) (Proposal, error) {
	ctx, span := tracer.Start(ctx, "remediation.get")
	defer span.End()
	proposal, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		recordError(span, err)
	}
	return proposal, err
}

func (s *Service) Approve(ctx context.Context, proposalID string, req DecisionRequest, authSubject string) (Proposal, error) {
	return s.decide(ctx, proposalID, StatusApproved, req, authSubject)
}

func (s *Service) Reject(ctx context.Context, proposalID string, req DecisionRequest, authSubject string) (Proposal, error) {
	return s.decide(ctx, proposalID, StatusRejected, req, authSubject)
}

func (s *Service) Submit(ctx context.Context, proposalID string) (Proposal, error) {
	ctx, span := tracer.Start(ctx, "remediation.submit")
	defer span.End()
	if s.submitter == nil {
		err := invalid("GitHub App submission is not configured")
		recordError(span, err)
		return Proposal{}, err
	}
	existing, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		recordError(span, err)
		return Proposal{}, err
	}
	if existing.SubmissionStatus == SubmissionSubmitted {
		return existing, nil
	}
	proposal, err := s.store.BeginProposalSubmission(ctx, proposalID)
	if err != nil {
		recordError(span, err)
		return Proposal{}, err
	}
	result, err := s.submitter.Submit(ctx, SubmissionRequest{
		ProposalID: proposal.ID, Repository: proposal.TargetRepository, Path: proposal.TargetPath,
		Resource: proposal.TargetResource, ExpectedReplicas: proposal.ExpectedState.Replicas,
		DesiredReplicas: proposal.DesiredChange.Replicas, Summary: proposal.Summary, Rationale: proposal.Rationale,
	})
	if err != nil {
		message := err.Error()
		if len(message) > 2000 {
			message = message[:2000]
		}
		if failErr := s.store.FailProposalSubmission(ctx, proposalID, message); failErr != nil {
			err = errors.Join(err, failErr)
		}
		recordError(span, err)
		return Proposal{}, err
	}
	proposal, err = s.store.CompleteProposalSubmission(ctx, proposalID, result)
	if err != nil {
		recordError(span, err)
	}
	return proposal, err
}

func (s *Service) decide(ctx context.Context, proposalID, status string, req DecisionRequest, authSubject string) (Proposal, error) {
	ctx, span := tracer.Start(ctx, "remediation.decide")
	defer span.End()
	actor := strings.TrimSpace(req.Actor)
	comment := strings.TrimSpace(req.Comment)
	if actor == "" || len(actor) > 200 {
		err := invalid("actor must contain 1 to 200 characters")
		recordError(span, err)
		return Proposal{}, err
	}
	if len(comment) > 2000 {
		err := invalid("comment must not exceed 2000 characters")
		recordError(span, err)
		return Proposal{}, err
	}
	proposal, err := s.store.DecideProposal(ctx, proposalID, status, actor, authSubject, comment)
	if err != nil {
		recordError(span, err)
	}
	return proposal, err
}

func (s *Service) validateCreate(incidentID string, req CreateRequest) (Target, int, int, time.Duration, error) {
	if strings.TrimSpace(incidentID) == "" {
		return Target{}, 0, 0, 0, invalid("incident id is required")
	}
	if len(s.targets) == 0 {
		return Target{}, 0, 0, 0, invalid("GitOps remediation is not configured")
	}
	if req.ActionKind != ActionSetWorkloadReplicas {
		return Target{}, 0, 0, 0, invalid("action_kind must be %s", ActionSetWorkloadReplicas)
	}
	target, ok := s.targets[req.TargetID]
	if !ok {
		return Target{}, 0, 0, 0, invalid("target_id is not allowlisted")
	}
	if req.Expected.Replicas == nil || req.Desired.Replicas == nil {
		return Target{}, 0, 0, 0, invalid("expected.replicas and desired.replicas are required")
	}
	expected, desired := *req.Expected.Replicas, *req.Desired.Replicas
	if expected < MinReplicas || expected > MaxReplicas || desired < MinReplicas || desired > MaxReplicas {
		return Target{}, 0, 0, 0, invalid("replicas must be between %d and %d", MinReplicas, MaxReplicas)
	}
	if expected == desired {
		return Target{}, 0, 0, 0, invalid("expected.replicas and desired.replicas must differ")
	}
	if err := boundedRequired("summary", req.Summary, MaxSummaryLength); err != nil {
		return Target{}, 0, 0, 0, err
	}
	if err := boundedRequired("rationale", req.Rationale, MaxRationaleLength); err != nil {
		return Target{}, 0, 0, 0, err
	}
	if req.Risk.Level != RiskLevelLow && req.Risk.Level != RiskLevelMedium && req.Risk.Level != RiskLevelHigh {
		return Target{}, 0, 0, 0, invalid("risk.level must be low, medium, or high")
	}
	if err := boundedRequired("risk.detail", req.Risk.Detail, MaxRiskDetailLength); err != nil {
		return Target{}, 0, 0, 0, err
	}
	if len(req.EvidenceOrdinals) == 0 || len(req.EvidenceOrdinals) > MaxEvidence {
		return Target{}, 0, 0, 0, invalid("evidence_ordinals must contain 1 to %d items", MaxEvidence)
	}
	seen := make(map[int]struct{}, len(req.EvidenceOrdinals))
	for _, ordinal := range req.EvidenceOrdinals {
		if ordinal < 0 {
			return Target{}, 0, 0, 0, invalid("evidence ordinals must be non-negative")
		}
		if _, exists := seen[ordinal]; exists {
			return Target{}, 0, 0, 0, invalid("evidence ordinals must be unique")
		}
		seen[ordinal] = struct{}{}
	}
	ttl := DefaultTTL
	if req.TTLSeconds != 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return Target{}, 0, 0, 0, invalid("ttl_seconds must be between %d and %d", int(MinTTL.Seconds()), int(MaxTTL.Seconds()))
	}
	return target, expected, desired, ttl, nil
}

func boundedRequired(name, value string, maximum int) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > maximum {
		return invalid("%s must contain 1 to %d characters", name, maximum)
	}
	return nil
}

func renderManifest(resource Resource, replicas int) (string, error) {
	if replicas < MinReplicas || replicas > MaxReplicas {
		return "", invalid("replicas must be between %d and %d", MinReplicas, MaxReplicas)
	}
	return fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  replicas: %d\n", resource.APIVersion, resource.Kind, resource.Name, resource.Namespace, replicas), nil
}

func renderDiff(targetPath, before, after string) string {
	beforeLines := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
	afterLines := strings.Split(strings.TrimSuffix(after, "\n"), "\n")
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- a/%s\n+++ b/%s\n@@ -1,%d +1,%d @@\n", targetPath, targetPath, len(beforeLines), len(afterLines))
	for i := range beforeLines {
		if beforeLines[i] == afterLines[i] {
			fmt.Fprintf(&builder, " %s\n", beforeLines[i])
			continue
		}
		fmt.Fprintf(&builder, "-%s\n+%s\n", beforeLines[i], afterLines[i])
	}
	return builder.String()
}

func recordError(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

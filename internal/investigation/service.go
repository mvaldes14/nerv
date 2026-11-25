package investigation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
	"github.com/mvaldes14/nerv/internal/telemetry"
)

const (
	SourceHTTPAsk            = "http.ask"
	SourceCLI                = "cli"
	SourceHTTPAlert          = "http.alert"
	SourceKubernetesDetector = "kubernetes.detector"
)

type Request struct {
	ID             string            `json:"id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Question       string            `json:"question"`
	Source         string            `json:"source"`
	Target         *Target           `json:"target,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Annotations    map[string]string `json:"annotations,omitempty"`
	TraceParent    string            `json:"traceparent,omitempty"`
	TraceState     string            `json:"tracestate,omitempty"`
}

type Target struct {
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Container string `json:"container,omitempty"`
}

type Evidence struct {
	Source  string `json:"source"`
	Summary string `json:"summary"`
}

type Decision struct {
	Action         string `json:"action"`
	Reason         string `json:"reason,omitempty"`
	ReviewRequired bool   `json:"review_required,omitempty"`
	Provider       string `json:"provider,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
}

type Generation struct {
	Answer            string         `json:"answer"`
	Confidence        string         `json:"confidence,omitempty"`
	Uncertainty       []string       `json:"uncertainty,omitempty"`
	RecommendedAction string         `json:"recommended_action,omitempty"`
	ToolsUsed         []string       `json:"tools_used,omitempty"`
	Evidence          []Evidence     `json:"evidence,omitempty"`
	Provider          string         `json:"provider"`
	Model             string         `json:"model"`
	Budget            BudgetMetadata `json:"budget"`
}

type Finding struct {
	ID                string         `json:"id"`
	WorkflowID        string         `json:"workflow_id"`
	Source            string         `json:"source"`
	Target            *Target        `json:"target,omitempty"`
	Question          string         `json:"question"`
	Answer            string         `json:"answer"`
	Evidence          []Evidence     `json:"evidence"`
	Decision          Decision       `json:"decision"`
	Confidence        string         `json:"confidence"`
	Uncertainty       []string       `json:"uncertainty,omitempty"`
	RecommendedAction string         `json:"recommended_action,omitempty"`
	ToolsUsed         []string       `json:"tools_used,omitempty"`
	Budget            BudgetMetadata `json:"budget"`
	Provider          string         `json:"provider"`
	Model             string         `json:"model"`
	CreatedAt         time.Time      `json:"created_at"`
}

type Accepted struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
}

type IncidentFilter struct {
	Limit     int
	Offset    int
	Source    string
	Status    string
	Namespace string
	Workload  string
}

type IncidentSummary struct {
	ID          string            `json:"id"`
	WorkflowID  string            `json:"workflow_id"`
	Source      string            `json:"source"`
	Question    string            `json:"question"`
	Status      string            `json:"status"`
	Target      *Target           `json:"target,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type IncidentPage struct {
	Items   []IncidentSummary `json:"items"`
	Limit   int               `json:"limit"`
	Offset  int               `json:"offset"`
	HasMore bool              `json:"has_more"`
}

type IncidentFilterOptions struct {
	Sources    []string `json:"sources"`
	Statuses   []string `json:"statuses"`
	Namespaces []string `json:"namespaces"`
	Workloads  []string `json:"workloads"`
}

type NotificationAttempt struct {
	Sink        string    `json:"sink"`
	ExternalID  string    `json:"external_id,omitempty"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	AttemptedAt time.Time `json:"attempted_at"`
}

type NotificationBatchItem struct {
	IncidentID string
	WorkflowID string
	Question   string
	Action     string
	Target     *Target
}

type NotificationBatch struct {
	ID         string
	DueAt      time.Time
	ExternalID string
	Sent       bool
	Items      []NotificationBatchItem
}

type Investigation struct {
	Incident      IncidentSummary       `json:"incident"`
	Finding       *Finding              `json:"finding,omitempty"`
	Evidence      []Evidence            `json:"evidence"`
	Notifications []NotificationAttempt `json:"notifications"`
}

const (
	DefaultIncidentLimit = 50
	MaxIncidentLimit     = 100
	MaxIncidentOffset    = 10000
)

type LLM interface {
	Generate(ctx context.Context, prompt string, evidence []Evidence) (answer string, provider string, model string, err error)
}

type Collector interface {
	Collect(ctx context.Context, req Request) ([]Evidence, error)
}

type DecisionEngine interface {
	Evaluate(ctx context.Context, req Request, evidence []Evidence) (Decision, error)
}

type AdmissionPolicy struct {
	MaxConcurrent     int
	LeaseTTL          time.Duration
	AskPerMinute      int
	AlertPerMinute    int
	DetectorPerMinute int
	DailyMaxTokens    int
	DailyMaxCostUSD   float64
}

func (p AdmissionPolicy) RateLimit(source string) int {
	switch source {
	case SourceHTTPAsk, SourceCLI:
		return p.AskPerMinute
	case SourceHTTPAlert:
		return p.AlertPerMinute
	case SourceKubernetesDetector:
		return p.DetectorPerMinute
	default:
		return p.AlertPerMinute
	}
}

type AdmissionSnapshot struct {
	Active      int
	DailyTokens int64
	DailyCost   float64
	RetryAfter  time.Duration
}

type AdmissionError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *AdmissionError) Error() string {
	return "investigation admission denied: " + e.Reason
}

var ErrInvestigationNotRunning = errors.New("investigation is not running")

type Store interface {
	Ping(ctx context.Context) error
	SaveIncident(ctx context.Context, workflowID string, req Request, status string) error
	SaveEvidence(ctx context.Context, incidentID string, evidence []Evidence) error
	SaveFinding(ctx context.Context, finding Finding) error
	SetStatus(ctx context.Context, incidentID, status string) error
	GetInvestigation(ctx context.Context, incidentID string) (Investigation, error)
	ListIncidents(ctx context.Context, filter IncidentFilter) (IncidentPage, error)
	ListIncidentFilterOptions(ctx context.Context) (IncidentFilterOptions, error)
	ResolveIncident(ctx context.Context, correlationKey string, cooldown time.Duration) (string, error)
	ReleaseIncident(ctx context.Context, correlationKey, incidentID string) error
	AdmitInvestigation(ctx context.Context, workflowID, source string, policy AdmissionPolicy) (AdmissionSnapshot, error)
	ReleaseInvestigation(ctx context.Context, workflowID string) (int, error)
	FailRunningInvestigation(ctx context.Context, incidentID, workflowID string) (int, error)
}

type Notifier interface {
	Send(ctx context.Context, finding Finding) (string, error)
}

type Dependencies struct {
	DBOS             dbos.Context
	LLM              LLM
	Collector        Collector
	Tools            ToolExecutor
	AgentBudget      AgentBudget
	Decision         DecisionEngine
	Store            Store
	Notifier         Notifier
	AgentProvider    string
	AgentModel       string
	IncidentCooldown time.Duration
	AdmissionPolicy  AdmissionPolicy
}

type Service struct {
	dbos             dbos.Context
	llm              LLM
	collector        Collector
	tools            ToolExecutor
	agentBudget      AgentBudget
	decision         DecisionEngine
	store            Store
	notifier         Notifier
	agentProvider    string
	agentModel       string
	incidentCooldown time.Duration
	admissionPolicy  AdmissionPolicy
	listWorkflows    func(dbos.Client, ...dbos.ListWorkflowsOption) ([]dbos.WorkflowStatus, error)
	cancelWorkflow   func(dbos.Client, string, ...dbos.CancelWorkflowOption) error
}

func NewService(deps Dependencies) (*Service, error) {
	if deps.DBOS == nil {
		return nil, fmt.Errorf("DBOS context is required")
	}
	if deps.LLM == nil || deps.Store == nil {
		return nil, fmt.Errorf("LLM and store are required")
	}

	decision := deps.Decision
	if decision == nil {
		decision = defaultDecisionEngine{}
	}
	notifier := deps.Notifier
	if notifier == nil {
		notifier = noopNotifier{}
	}
	budget := deps.AgentBudget
	if budget.MaxRounds == 0 {
		budget = AgentBudget{MaxRounds: 4, MaxToolCalls: 6, Timeout: 75 * time.Second, MaxTokens: 12000, ToolConcurrency: 2, ModelTimeout: 30 * time.Second, ToolTimeout: 30 * time.Second, MaxEvidenceBytes: 32 * 1024}
	}
	admission := deps.AdmissionPolicy
	if admission.MaxConcurrent == 0 {
		admission = AdmissionPolicy{MaxConcurrent: 2, LeaseTTL: 10 * time.Minute, AskPerMinute: 6, AlertPerMinute: 30, DetectorPerMinute: 30, DailyMaxTokens: 240000}
	}
	s := &Service{
		dbos:             deps.DBOS,
		llm:              deps.LLM,
		collector:        deps.Collector,
		tools:            deps.Tools,
		agentBudget:      budget,
		decision:         decision,
		store:            deps.Store,
		notifier:         notifier,
		agentProvider:    deps.AgentProvider,
		agentModel:       deps.AgentModel,
		incidentCooldown: deps.IncidentCooldown,
		admissionPolicy:  admission,
		listWorkflows:    dbos.ListWorkflows,
		cancelWorkflow:   dbos.CancelWorkflow,
	}
	dbos.RegisterWorkflow(deps.DBOS, s.Investigate,
		dbos.WithInstance(s),
		dbos.WithWorkflowName("nerv.investigate"),
		dbos.WithMaxRecoveryAttempts(5),
	)
	return s, nil
}

func (s *Service) ConfigName() string { return "nerv-investigation-service" }

func (s *Service) Accept(ctx context.Context, req Request) (Accepted, error) {
	req = normalizeRequest(req)
	req.TraceParent, req.TraceState = telemetry.CaptureTraceContext(ctx)
	if err := validateRequest(req); err != nil {
		return Accepted{}, err
	}
	if req.ID == "" {
		if req.IdempotencyKey == "" {
			req.ID = uuid.NewString()
		} else {
			incidentID, err := s.store.ResolveIncident(ctx, req.IdempotencyKey, s.incidentCooldown)
			if err != nil {
				return Accepted{}, fmt.Errorf("resolve correlated incident: %w", err)
			}
			req.ID = incidentID
		}
	}
	workflowID := workflowID(req)
	snapshot, err := s.store.AdmitInvestigation(ctx, workflowID, req.Source, s.admissionPolicy)
	if err != nil {
		if req.IdempotencyKey != "" {
			_ = s.store.ReleaseIncident(ctx, req.IdempotencyKey, req.ID)
		}
		var denied *AdmissionError
		if errors.As(err, &denied) {
			telemetry.RecordAdmission(ctx, req.Source, "denied", denied.Reason, snapshot.Active, s.admissionPolicy.MaxConcurrent, snapshot.DailyTokens, s.admissionPolicy.DailyMaxTokens, snapshot.DailyCost, s.admissionPolicy.DailyMaxCostUSD)
			return Accepted{}, denied
		}
		return Accepted{}, fmt.Errorf("admit investigation: %w", err)
	}
	telemetry.RecordAdmission(ctx, req.Source, "accepted", "", snapshot.Active, s.admissionPolicy.MaxConcurrent, snapshot.DailyTokens, s.admissionPolicy.DailyMaxTokens, snapshot.DailyCost, s.admissionPolicy.DailyMaxCostUSD)
	handle, err := dbos.RunWorkflow(s.dbos, s.Investigate, req,
		dbos.WithRunInstance(s),
		dbos.WithWorkflowID(workflowID),
		dbos.WithWorkflowAttributes(map[string]any{
			"incident_id": req.ID,
			"source":      req.Source,
		}),
	)
	if err != nil {
		_, _ = s.store.ReleaseInvestigation(ctx, workflowID)
		if req.IdempotencyKey != "" {
			_ = s.store.ReleaseIncident(ctx, req.IdempotencyKey, req.ID)
		}
		return Accepted{}, fmt.Errorf("start investigation workflow: %w", err)
	}
	return Accepted{ID: req.ID, WorkflowID: handle.GetWorkflowID(), Status: "accepted"}, nil
}

func (s *Service) Ask(ctx context.Context, req Request) (Finding, error) {
	accepted, err := s.Accept(ctx, req)
	if err != nil {
		return Finding{}, err
	}
	handle, err := dbos.RetrieveWorkflow[Finding](s.dbos, accepted.WorkflowID)
	if err != nil {
		return Finding{}, fmt.Errorf("retrieve investigation workflow: %w", err)
	}

	opts := []dbos.GetResultOption{}
	if deadline, ok := ctx.Deadline(); ok {
		opts = append(opts, dbos.WithHandleTimeout(time.Until(deadline)))
	}
	finding, err := handle.GetResult(opts...)
	if err != nil {
		return Finding{}, fmt.Errorf("wait for investigation: %w", err)
	}
	return finding, nil
}

func (s *Service) Get(ctx context.Context, id string) (Investigation, error) {
	return s.store.GetInvestigation(ctx, id)
}

func (s *Service) Cancel(ctx context.Context, id string) (Investigation, error) {
	detail, err := s.store.GetInvestigation(ctx, id)
	if err != nil {
		return Investigation{}, err
	}
	if detail.Incident.Status != "running" {
		return Investigation{}, ErrInvestigationNotRunning
	}
	workflowID := detail.Incident.WorkflowID
	workflows, err := s.listWorkflows(s.dbos,
		dbos.WithFilterWorkflowIDs(workflowID),
		dbos.WithFilterLoadInput(false),
		dbos.WithFilterLoadOutput(false),
	)
	if err != nil {
		return Investigation{}, fmt.Errorf("read workflow status: %w", err)
	}
	if len(workflows) == 0 {
		return Investigation{}, fmt.Errorf("workflow %q not found", workflowID)
	}
	switch workflows[0].Status {
	case dbos.WorkflowStatusError, dbos.WorkflowStatusCancelled, dbos.WorkflowStatusMaxRecoveryAttemptsExceeded:
		// Workflow is already terminal; repair stale NERV state below.
	case dbos.WorkflowStatusSuccess:
		return Investigation{}, fmt.Errorf("workflow %q already completed successfully", workflowID)
	default:
		if err := s.cancelWorkflow(s.dbos, workflowID); err != nil {
			return Investigation{}, fmt.Errorf("cancel workflow: %w", err)
		}
	}
	active, err := s.store.FailRunningInvestigation(ctx, id, workflowID)
	if err != nil {
		return Investigation{}, err
	}
	telemetry.RecordAdmissionActive(ctx, active, s.admissionPolicy.MaxConcurrent)
	return s.store.GetInvestigation(ctx, id)
}

func (s *Service) List(ctx context.Context, filter IncidentFilter) (IncidentPage, error) {
	filter.Source = strings.TrimSpace(filter.Source)
	filter.Status = strings.TrimSpace(filter.Status)
	filter.Namespace = strings.TrimSpace(filter.Namespace)
	filter.Workload = strings.TrimSpace(filter.Workload)
	if filter.Limit <= 0 {
		filter.Limit = DefaultIncidentLimit
	} else if filter.Limit > MaxIncidentLimit {
		filter.Limit = MaxIncidentLimit
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	} else if filter.Offset > MaxIncidentOffset {
		filter.Offset = MaxIncidentOffset
	}
	return s.store.ListIncidents(ctx, filter)
}

func (s *Service) FilterOptions(ctx context.Context) (IncidentFilterOptions, error) {
	options, err := s.store.ListIncidentFilterOptions(ctx)
	if err != nil {
		return IncidentFilterOptions{}, err
	}
	options.Sources = mergeOptions([]string{SourceHTTPAsk, SourceCLI, SourceHTTPAlert, SourceKubernetesDetector}, options.Sources)
	options.Statuses = mergeOptions([]string{"running", "completed", "failed"}, options.Statuses)
	return options, nil
}

func mergeOptions(preferred, discovered []string) []string {
	seen := make(map[string]struct{}, len(preferred)+len(discovered))
	merged := make([]string, 0, len(preferred)+len(discovered))
	for _, values := range [][]string{preferred, discovered} {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged
}

func (s *Service) Ready(ctx context.Context) error {
	return s.store.Ping(ctx)
}

func (s *Service) Investigate(ctx dbos.Context, req Request) (finding Finding, workflowErr error) {
	workflowID, _ := dbos.GetWorkflowID(ctx)
	agentCtx, agentSpan := telemetry.StartWorkflow(context.Background(), s.agentProvider, s.agentModel, req.TraceParent, req.TraceState)
	incidentPersisted := false
	incidentCompleted := false
	defer func() {
		if workflowErr != nil {
			if incidentPersisted && !incidentCompleted {
				_, cleanupErr := dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
					return "failed", s.store.SetStatus(stepCtx, req.ID, "failed")
				}, dbos.WithStepName("fail-incident"), dbos.WithStepMaxRetries(3))
				if cleanupErr != nil {
					workflowErr = errors.Join(workflowErr, fmt.Errorf("mark investigation failed: %w", cleanupErr))
				}
			}
			_, releaseErr := dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
				active, err := s.store.ReleaseInvestigation(stepCtx, workflowID)
				if err == nil {
					telemetry.RecordAdmissionActive(stepCtx, active, s.admissionPolicy.MaxConcurrent)
				}
				return "released", err
			}, dbos.WithStepName("release-admission-lease-on-error"), dbos.WithStepMaxRetries(3))
			if releaseErr != nil {
				workflowErr = errors.Join(workflowErr, fmt.Errorf("release failed investigation admission: %w", releaseErr))
			}
		}
		agentSpan.End(workflowErr, "")
	}()

	_, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return "persisted", s.store.SaveIncident(stepCtx, workflowID, req, "running")
	}, dbos.WithStepName("persist-incident"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}
	incidentPersisted = true

	evidence, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) ([]Evidence, error) {
		base := []Evidence{{Source: req.Source, Summary: triggerSummary(req)}}
		if _, bounded := s.llm.(AgentLLM); bounded || s.collector == nil {
			return base, nil
		}
		collected, collectErr := s.collector.Collect(telemetry.WithSpan(stepCtx, agentCtx), req)
		if collectErr != nil {
			base = append(base, Evidence{Source: "kubernetes", Summary: "evidence collection failed: " + collectErr.Error()})
			return base, nil
		}
		return append(base, collected...), nil
	}, dbos.WithStepName("prepare-trigger-evidence"), dbos.WithStepMaxRetries(2))
	if err != nil {
		return Finding{}, err
	}

	_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return "persisted", s.store.SaveEvidence(stepCtx, req.ID, evidence)
	}, dbos.WithStepName("persist-trigger-evidence"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}

	decision, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) (Decision, error) {
		return s.decision.Evaluate(stepCtx, req, evidence)
	}, dbos.WithStepName("decision-gateway"), dbos.WithStepMaxRetries(2))
	if err != nil {
		return Finding{}, err
	}

	var generation Generation
	if decision.Action == "suppress" {
		generation = Generation{Answer: suppressionAnswer(decision), Confidence: "unknown", Provider: decision.Provider}
	} else if agentLLM, ok := s.llm.(AgentLLM); ok {
		generation, err = s.runAgentLoop(ctx, agentCtx, agentLLM, req, decision, evidence[0])
		if err != nil {
			return Finding{}, err
		}
		evidence = append(evidence, generation.Evidence...)
		_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
			return "persisted", s.store.SaveEvidence(stepCtx, req.ID, evidence)
		}, dbos.WithStepName("persist-agent-evidence"), dbos.WithStepMaxRetries(3))
		if err != nil {
			return Finding{}, err
		}
	} else {
		generation, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (Generation, error) {
			answer, provider, model, generateErr := s.llm.Generate(telemetry.WithSpan(stepCtx, agentCtx), buildPrompt(req, decision), evidence)
			return Generation{Answer: answer, Confidence: "unknown", Provider: provider, Model: model}, generateErr
		}, dbos.WithStepName("agentgateway-llm"), dbos.WithStepMaxRetries(2))
		if err != nil {
			return Finding{}, err
		}
	}

	finding, err = dbos.RunAsStep(ctx, func(context.Context) (Finding, error) {
		confidence := generation.Confidence
		if confidence == "" {
			confidence = "unknown"
		}
		return Finding{
			ID:                req.ID,
			WorkflowID:        workflowID,
			Source:            req.Source,
			Target:            req.Target,
			Question:          req.Question,
			Answer:            generation.Answer,
			Evidence:          evidence,
			Decision:          decision,
			Confidence:        confidence,
			Uncertainty:       generation.Uncertainty,
			RecommendedAction: generation.RecommendedAction,
			ToolsUsed:         generation.ToolsUsed,
			Budget:            generation.Budget,
			Provider:          generation.Provider,
			Model:             generation.Model,
			CreatedAt:         time.Now().UTC(),
		}, nil
	}, dbos.WithStepName("build-finding"))
	if err != nil {
		return Finding{}, err
	}

	_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return "persisted", s.store.SaveFinding(stepCtx, finding)
	}, dbos.WithStepName("persist-finding"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}

	_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return s.notifier.Send(stepCtx, finding)
	},
		dbos.WithStepName("notify-gotify"),
		dbos.WithStepMaxRetries(5),
		dbos.WithStepBaseInterval(time.Second),
		dbos.WithStepMaxInterval(30*time.Second),
	)
	if err != nil {
		return Finding{}, err
	}

	_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return "completed", s.store.SetStatus(stepCtx, req.ID, "completed")
	}, dbos.WithStepName("complete-incident"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}
	incidentCompleted = true

	_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		active, releaseErr := s.store.ReleaseInvestigation(stepCtx, workflowID)
		if releaseErr == nil {
			telemetry.RecordAdmissionActive(stepCtx, active, s.admissionPolicy.MaxConcurrent)
		}
		return "released", releaseErr
	}, dbos.WithStepName("release-admission-lease"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}
	return finding, nil
}

func suppressionAnswer(decision Decision) string {
	lines := []string{"Investigation suppressed by policy."}
	reason := policyMetadataValue(decision.Reason, 500)
	provider := policyMetadataValue(decision.Provider, 200)
	requestID := policyMetadataValue(decision.RequestID, 200)
	if reason != "" {
		lines = append(lines, "Reason: "+reason)
	}
	if provider != "" {
		lines = append(lines, "Decision provider: "+provider)
	}
	if requestID != "" {
		lines = append(lines, "Decision request ID: "+requestID)
	}
	if len(lines) > 1 {
		lines = append(lines, "Policy decision metadata does not identify or link an existing incident.")
	}
	return strings.Join(lines, "\n")
}

func policyMetadataValue(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

type defaultDecisionEngine struct{}

func (defaultDecisionEngine) Evaluate(context.Context, Request, []Evidence) (Decision, error) {
	return Decision{Action: "investigate", Reason: "default NERV policy", Provider: "nerv.local"}, nil
}

type noopNotifier struct{}

func (noopNotifier) Send(context.Context, Finding) (string, error) { return "disabled", nil }

func validateRequest(req Request) error {
	if req.Question == "" {
		return fmt.Errorf("question is required")
	}
	if len([]rune(req.Question)) > 4000 {
		return fmt.Errorf("question exceeds 4000 characters")
	}
	if req.Target != nil {
		fields := []struct{ name, value string }{
			{name: "target namespace", value: req.Target.Namespace},
			{name: "target kind", value: req.Target.Kind},
			{name: "target name", value: req.Target.Name},
			{name: "target container", value: req.Target.Container},
		}
		for _, field := range fields {
			if len([]rune(field.value)) > 253 {
				return fmt.Errorf("%s exceeds 253 characters", field.name)
			}
		}
	}
	return nil
}

func normalizeRequest(req Request) Request {
	req.Question = strings.TrimSpace(req.Question)
	req.Source = strings.TrimSpace(req.Source)
	if req.Source == "" {
		req.Source = "unknown"
	}
	return req
}

func workflowID(req Request) string { return "incident-" + req.ID }

func buildPrompt(req Request, decision Decision) string {
	return fmt.Sprintf("Investigate this Kubernetes/SRE incident using only supplied evidence. Decision action: %s. Question: %s. Return concise diagnosis, evidence, uncertainty, and recommended next action.", decision.Action, req.Question)
}

func triggerSummary(req Request) string {
	parts := []string{req.Question}
	if req.Target != nil {
		parts = append(parts, fmt.Sprintf("target=%s/%s namespace=%s", req.Target.Kind, req.Target.Name, req.Target.Namespace))
	}
	return strings.Join(parts, "; ")
}

// CorrelationKey gives watcher and external intake one identity when target and
// deterministic failure class match.
func CorrelationKey(target *Target, failure string) string {
	if target == nil {
		return ""
	}
	kind := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target.Kind)), "s")
	return strings.Join([]string{
		"k8s",
		strings.ToLower(strings.TrimSpace(target.Namespace)),
		kind,
		strings.ToLower(strings.TrimSpace(target.Name)),
		strings.ToLower(strings.TrimSpace(target.Container)),
		strings.ToLower(strings.TrimSpace(failure)),
	}, ":")
}

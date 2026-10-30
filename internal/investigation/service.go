package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

type Request struct {
	ID             string            `json:"id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Question       string            `json:"question"`
	Source         string            `json:"source"`
	Target         *Target           `json:"target,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Annotations    map[string]string `json:"annotations,omitempty"`
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
	Answer   string `json:"answer"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Finding struct {
	ID          string     `json:"id"`
	WorkflowID  string     `json:"workflow_id"`
	Question    string     `json:"question"`
	Answer      string     `json:"answer"`
	Evidence    []Evidence `json:"evidence"`
	Decision    Decision   `json:"decision"`
	Uncertainty []string   `json:"uncertainty,omitempty"`
	Provider    string     `json:"provider"`
	Model       string     `json:"model"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Accepted struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
}

type LLM interface {
	Generate(ctx context.Context, prompt string, evidence []Evidence) (answer string, provider string, model string, err error)
}

type Collector interface {
	Collect(ctx context.Context, req Request) ([]Evidence, error)
}

type DecisionEngine interface {
	Evaluate(ctx context.Context, req Request, evidence []Evidence) (Decision, error)
}

type Store interface {
	Ping(ctx context.Context) error
	SaveIncident(ctx context.Context, workflowID string, req Request, status string) error
	SaveEvidence(ctx context.Context, incidentID string, evidence []Evidence) error
	SaveFinding(ctx context.Context, finding Finding) error
	SetStatus(ctx context.Context, incidentID, status string) error
	GetFinding(ctx context.Context, incidentID string) (Finding, string, error)
}

type Notifier interface {
	Send(ctx context.Context, finding Finding) (string, error)
}

type Dependencies struct {
	DBOS      dbos.Context
	LLM       LLM
	Collector Collector
	Decision  DecisionEngine
	Store     Store
	Notifier  Notifier
}

type Service struct {
	dbos      dbos.Context
	llm       LLM
	collector Collector
	decision  DecisionEngine
	store     Store
	notifier  Notifier
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
	s := &Service{
		dbos:      deps.DBOS,
		llm:       deps.LLM,
		collector: deps.Collector,
		decision:  decision,
		store:     deps.Store,
		notifier:  notifier,
	}
	dbos.RegisterWorkflow(deps.DBOS, s.Investigate,
		dbos.WithInstance(s),
		dbos.WithWorkflowName("nerv.investigate"),
		dbos.WithMaxRecoveryAttempts(5),
	)
	return s, nil
}

func (s *Service) ConfigName() string { return "nerv-investigation-service" }

func (s *Service) Accept(req Request) (Accepted, error) {
	req = normalizeRequest(req)
	if req.Question == "" {
		return Accepted{}, fmt.Errorf("question is required")
	}
	workflowID := workflowID(req)
	handle, err := dbos.RunWorkflow(s.dbos, s.Investigate, req,
		dbos.WithRunInstance(s),
		dbos.WithWorkflowID(workflowID),
		dbos.WithWorkflowAttributes(map[string]any{
			"incident_id": req.ID,
			"source":      req.Source,
		}),
	)
	if err != nil {
		return Accepted{}, fmt.Errorf("start investigation workflow: %w", err)
	}
	return Accepted{ID: req.ID, WorkflowID: handle.GetWorkflowID(), Status: "accepted"}, nil
}

func (s *Service) Ask(ctx context.Context, req Request) (Finding, error) {
	accepted, err := s.Accept(req)
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

func (s *Service) Get(ctx context.Context, id string) (Finding, string, error) {
	return s.store.GetFinding(ctx, id)
}

func (s *Service) Ready(ctx context.Context) error {
	return s.store.Ping(ctx)
}

func (s *Service) Investigate(ctx dbos.Context, req Request) (Finding, error) {
	workflowID, _ := dbos.GetWorkflowID(ctx)

	_, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return "persisted", s.store.SaveIncident(stepCtx, workflowID, req, "running")
	}, dbos.WithStepName("persist-incident"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}

	evidence, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) ([]Evidence, error) {
		base := []Evidence{{Source: req.Source, Summary: triggerSummary(req)}}
		if s.collector == nil {
			return base, nil
		}
		collected, collectErr := s.collector.Collect(stepCtx, req)
		if collectErr != nil {
			base = append(base, Evidence{Source: "kubernetes", Summary: "evidence collection failed: " + collectErr.Error()})
			return base, nil
		}
		return append(base, collected...), nil
	}, dbos.WithStepName("collect-evidence"), dbos.WithStepMaxRetries(2))
	if err != nil {
		return Finding{}, err
	}

	_, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		return "persisted", s.store.SaveEvidence(stepCtx, req.ID, evidence)
	}, dbos.WithStepName("persist-evidence"), dbos.WithStepMaxRetries(3))
	if err != nil {
		return Finding{}, err
	}

	decision, err := dbos.RunAsStep(ctx, func(stepCtx context.Context) (Decision, error) {
		return s.decision.Evaluate(stepCtx, req, evidence)
	}, dbos.WithStepName("decision-gateway"), dbos.WithStepMaxRetries(2))
	if err != nil {
		return Finding{}, err
	}

	generation := Generation{Answer: "Investigation suppressed by policy.", Provider: decision.Provider}
	if decision.Action != "suppress" {
		generation, err = dbos.RunAsStep(ctx, func(stepCtx context.Context) (Generation, error) {
			answer, provider, model, generateErr := s.llm.Generate(stepCtx, buildPrompt(req, decision), evidence)
			return Generation{Answer: answer, Provider: provider, Model: model}, generateErr
		}, dbos.WithStepName("agentgateway-llm"), dbos.WithStepMaxRetries(2))
		if err != nil {
			return Finding{}, err
		}
	}

	finding, err := dbos.RunAsStep(ctx, func(context.Context) (Finding, error) {
		return Finding{
			ID:         req.ID,
			WorkflowID: workflowID,
			Question:   req.Question,
			Answer:     generation.Answer,
			Evidence:   evidence,
			Decision:   decision,
			Provider:   generation.Provider,
			Model:      generation.Model,
			CreatedAt:  time.Now().UTC(),
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
	return finding, nil
}

type defaultDecisionEngine struct{}

func (defaultDecisionEngine) Evaluate(context.Context, Request, []Evidence) (Decision, error) {
	return Decision{Action: "investigate", Reason: "default NERV policy"}, nil
}

type noopNotifier struct{}

func (noopNotifier) Send(context.Context, Finding) (string, error) { return "disabled", nil }

func normalizeRequest(req Request) Request {
	req.Question = strings.TrimSpace(req.Question)
	req.Source = strings.TrimSpace(req.Source)
	if req.Source == "" {
		req.Source = "unknown"
	}
	if req.ID == "" {
		if req.IdempotencyKey != "" {
			req.ID = stableID(req.Source, req.IdempotencyKey)
		} else {
			req.ID = uuid.NewString()
		}
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

func stableID(source, value string) string {
	h := sha256.Sum256([]byte(source + "\x00" + value))
	return hex.EncodeToString(h[:16])
}

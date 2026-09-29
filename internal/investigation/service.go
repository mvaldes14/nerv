package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Request struct {
	Question string `json:"question"`
	Source   string `json:"source"`
}

type Evidence struct {
	Source  string `json:"source"`
	Summary string `json:"summary"`
}

type Finding struct {
	ID          string     `json:"id"`
	Question    string     `json:"question"`
	Answer      string     `json:"answer"`
	Evidence    []Evidence `json:"evidence"`
	Uncertainty []string   `json:"uncertainty,omitempty"`
	Provider    string     `json:"provider"`
	Model       string     `json:"model"`
	CreatedAt   time.Time  `json:"created_at"`
}

type LLM interface {
	Generate(ctx context.Context, prompt string, evidence []Evidence) (answer string, provider string, model string, err error)
}

type Dependencies struct {
	LLM LLM
}

type Service struct {
	llm LLM
}

func NewService(deps Dependencies) *Service {
	return &Service{llm: deps.LLM}
}

func (s *Service) Ask(ctx context.Context, req Request) (Finding, error) {
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return Finding{}, fmt.Errorf("question is required")
	}

	evidenceSet := []Evidence{
		{
			Source:  req.Source,
			Summary: "ad-hoc question; no webhook evidence attached",
		},
	}

	answer, provider, model, err := s.llm.Generate(ctx, buildPrompt(question), evidenceSet)
	if err != nil {
		return Finding{}, fmt.Errorf("generate finding: %w", err)
	}

	return Finding{
		ID:        stableID(req.Source, question),
		Question:  question,
		Answer:    answer,
		Evidence:  evidenceSet,
		Provider:  provider,
		Model:     model,
		CreatedAt: time.Now().UTC(),
		Uncertainty: []string{
			"scaffold only: real LLM calls and webhook-backed investigations are not implemented yet",
		},
	}, nil
}

func buildPrompt(question string) string {
	return "Investigate this SRE question using only supplied evidence: " + question
}

func stableID(source, question string) string {
	h := sha256.Sum256([]byte(source + "\x00" + question))
	return hex.EncodeToString(h[:8])
}

package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type EchoProvider struct {
	provider string
	model    string
}

func NewEchoProvider(provider, model string) EchoProvider {
	if provider == "" {
		provider = "echo"
	}
	if model == "" {
		model = "echo-v0"
	}
	return EchoProvider{provider: provider, model: model}
}

func (p EchoProvider) Generate(ctx context.Context, prompt string, evidence []investigation.Evidence) (string, string, string, error) {
	select {
	case <-ctx.Done():
		return "", p.provider, p.model, ctx.Err()
	default:
	}

	summaries := make([]string, 0, len(evidence))
	for _, item := range evidence {
		summaries = append(summaries, fmt.Sprintf("%s: %s", item.Source, item.Summary))
	}

	answer := "NERV scaffold response. Replace the echo provider with a real LLM adapter and MCP-backed observability tools."
	if len(summaries) > 0 {
		answer += " Evidence collected: " + strings.Join(summaries, "; ") + "."
	}
	_ = prompt
	return answer, p.provider, p.model, nil
}

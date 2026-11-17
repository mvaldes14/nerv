package investigation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

type markerToolExecutor struct{}

func (markerToolExecutor) Definitions() []ToolDefinition { return nil }
func (markerToolExecutor) Execute(context.Context, ToolCall) ToolResult {
	panic("unexpected direct tool execution")
}

func testAgentState(budget AgentBudget) agentLoopStart {
	return agentLoopStart{
		StartedAt: time.Now().UTC(),
		Budget:    budget,
		Definitions: []ToolDefinition{{
			Type: "function",
			Function: ToolFunction{
				Name:       "list_workloads",
				Strict:     true,
				Parameters: map[string]any{"type": "object", "additionalProperties": false},
			},
		}},
	}
}

func defaultTestBudget() AgentBudget {
	return AgentBudget{
		MaxRounds:        4,
		MaxToolCalls:     6,
		Timeout:          time.Minute,
		MaxTokens:        12000,
		ToolConcurrency:  2,
		ModelTimeout:     time.Second,
		ToolTimeout:      time.Second,
		MaxEvidenceBytes: 4096,
	}
}

func completedTool(result ToolResult) <-chan dbos.StepOutcome[ToolResult] {
	ch := make(chan dbos.StepOutcome[ToolResult], 1)
	ch <- dbos.StepOutcome[ToolResult]{Result: result}
	close(ch)
	return ch
}

func TestAgentLoopDiscoversWorkloadAndReturnsStructuredFinding(t *testing.T) {
	service := &Service{tools: markerToolExecutor{}, agentProvider: "test", agentModel: "model"}
	round := 0
	modelStep := func(_ context.Context, _ int, _ time.Duration, request AgentRequest) (AgentResponse, error) {
		round++
		if request.MaxOutputTokens <= 0 || len(request.Tools) != 1 {
			t.Fatalf("missing bounded model request: %#v", request)
		}
		if round == 1 {
			if !strings.Contains(request.Messages[1].Content, "none supplied") {
				t.Fatalf("discovery prompt missing: %s", request.Messages[1].Content)
			}
			return AgentResponse{
				Message: AgentMessage{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:   "call-1",
						Type: "function",
						Function: ToolCallFunction{
							Name:      "list_workloads",
							Arguments: `{"namespace":"","kind":"all","label_selector":"","name_contains":"vault","limit":20,"max_bytes":4096}`,
						},
					}},
				},
				Provider: "test", Model: "model", Attempts: 1, Usage: TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			}, nil
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || !strings.Contains(last.Content, "vault-0") {
			t.Fatalf("tool evidence not returned to model: %#v", last)
		}
		return AgentResponse{
			Message:  AgentMessage{Role: "assistant", Content: `{"answer":"Vault is running.","confidence":"high","uncertainty":[],"recommended_action":"Continue monitoring.","evidence_used":["list_workloads"]}`},
			Provider: "test", Model: "model", Attempts: 1, Usage: TokenUsage{InputTokens: 140, OutputTokens: 40, TotalTokens: 180},
		}, nil
	}
	toolCalls := 0
	toolStep := func(_ context.Context, ordinal int, _ time.Duration, call ToolCall) (<-chan dbos.StepOutcome[ToolResult], error) {
		toolCalls++
		if ordinal != 1 || call.Function.Name != "list_workloads" {
			t.Fatalf("unexpected tool execution: ordinal=%d call=%#v", ordinal, call)
		}
		return completedTool(ToolResult{CallID: call.ID, Name: call.Function.Name, Content: `{"ok":true,"result":[{"kind":"Pod","resource":{"namespace":"security","name":"vault-0","phase":"Running"}}]}`}), nil
	}

	generation, err := service.runAgentLoopWithSteps(context.Background(), Request{Question: "is vault running?"}, Decision{Action: "investigate"}, Evidence{Source: SourceHTTPAsk, Summary: "is vault running?"}, testAgentState(defaultTestBudget()), modelStep, toolStep)
	if err != nil {
		t.Fatal(err)
	}
	if generation.Answer != "Vault is running." || generation.Confidence != "high" || toolCalls != 1 {
		t.Fatalf("unexpected generation: %#v toolCalls=%d", generation, toolCalls)
	}
	if generation.Budget.RoundsUsed != 2 || generation.Budget.ToolCallsUsed != 1 || generation.Budget.TotalTokens != 300 {
		t.Fatalf("unexpected budget: %#v", generation.Budget)
	}
	if len(generation.Evidence) != 1 || generation.ToolsUsed[0] != "list_workloads" {
		t.Fatalf("unexpected executed evidence: %#v tools=%#v", generation.Evidence, generation.ToolsUsed)
	}
}

func TestAgentLoopIncludesExplicitTargetInPrompt(t *testing.T) {
	service := &Service{agentProvider: "test", agentModel: "model"}
	modelStep := func(_ context.Context, _ int, _ time.Duration, request AgentRequest) (AgentResponse, error) {
		prompt := request.Messages[1].Content
		for _, want := range []string{"kind=Pod", "namespace=security", "name=vault-0", "container=vault"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("target prompt missing %q: %s", want, prompt)
			}
		}
		return AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"target received","confidence":"unknown","uncertainty":[],"recommended_action":"inspect target","evidence_used":[]}`}, Usage: TokenUsage{TotalTokens: 1}}, nil
	}
	generation, err := service.runAgentLoopWithSteps(context.Background(), Request{Question: "check vault", Target: &Target{Kind: "Pod", Namespace: "security", Name: "vault-0", Container: "vault"}}, Decision{Action: "investigate"}, Evidence{Summary: "check vault"}, testAgentState(defaultTestBudget()), modelStep, nil)
	if err != nil {
		t.Fatal(err)
	}
	if generation.Answer != "target received" {
		t.Fatalf("unexpected generation: %#v", generation)
	}
}

func TestAgentLoopRejectsDuplicateCallsWithoutReexecution(t *testing.T) {
	service := &Service{tools: markerToolExecutor{}, agentProvider: "test", agentModel: "model"}
	round := 0
	modelStep := func(_ context.Context, _ int, _ time.Duration, request AgentRequest) (AgentResponse, error) {
		round++
		if round == 1 {
			function := ToolCallFunction{Name: "list_workloads", Arguments: `{}`}
			return AgentResponse{Message: AgentMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "one", Type: "function", Function: function}, {ID: "two", Type: "function", Function: function}}}, Usage: TokenUsage{TotalTokens: 10}}, nil
		}
		if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "duplicate tool call rejected") {
			t.Fatalf("duplicate rejection missing from conversation: %#v", request.Messages)
		}
		return AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"done","confidence":"medium","uncertainty":[],"recommended_action":"none","evidence_used":["list_workloads"]}`}, Usage: TokenUsage{TotalTokens: 10}}, nil
	}
	executions := 0
	toolStep := func(_ context.Context, _ int, _ time.Duration, call ToolCall) (<-chan dbos.StepOutcome[ToolResult], error) {
		executions++
		return completedTool(ToolResult{CallID: call.ID, Name: call.Function.Name, Content: `{"ok":true,"result":[]}`}), nil
	}
	generation, err := service.runAgentLoopWithSteps(context.Background(), Request{Question: "test"}, Decision{Action: "investigate"}, Evidence{Summary: "test"}, testAgentState(defaultTestBudget()), modelStep, toolStep)
	if err != nil {
		t.Fatal(err)
	}
	if executions != 1 || generation.Budget.ToolCallsUsed != 2 {
		t.Fatalf("executions=%d budget=%#v", executions, generation.Budget)
	}
}

func TestAgentLoopRejectsUnapprovedToolBeforeExecutor(t *testing.T) {
	service := &Service{tools: markerToolExecutor{}, agentProvider: "test", agentModel: "model"}
	round := 0
	modelStep := func(_ context.Context, _ int, _ time.Duration, request AgentRequest) (AgentResponse, error) {
		round++
		if round == 1 {
			return AgentResponse{Message: AgentMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "unsafe", Type: "function", Function: ToolCallFunction{Name: "exec", Arguments: `{}`}}}}, Usage: TokenUsage{TotalTokens: 1}}, nil
		}
		if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "unapproved tool rejected") {
			t.Fatalf("unsafe rejection missing: %#v", request.Messages)
		}
		return AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"rejected","confidence":"high","uncertainty":[],"recommended_action":"none","evidence_used":[]}`}, Usage: TokenUsage{TotalTokens: 1}}, nil
	}
	toolStep := func(context.Context, int, time.Duration, ToolCall) (<-chan dbos.StepOutcome[ToolResult], error) {
		t.Fatal("unapproved tool reached executor")
		return nil, nil
	}
	generation, err := service.runAgentLoopWithSteps(context.Background(), Request{Question: "run exec"}, Decision{Action: "investigate"}, Evidence{Summary: "run exec"}, testAgentState(defaultTestBudget()), modelStep, toolStep)
	if err != nil {
		t.Fatal(err)
	}
	if generation.Budget.ToolCallsUsed != 1 || len(generation.ToolsUsed) != 0 {
		t.Fatalf("unexpected generation: %#v", generation)
	}
}

func TestAgentLoopLaunchesToolsUpToConcurrencyCap(t *testing.T) {
	service := &Service{tools: markerToolExecutor{}, agentProvider: "test", agentModel: "model"}
	round := 0
	modelStep := func(_ context.Context, _ int, _ time.Duration, _ AgentRequest) (AgentResponse, error) {
		round++
		if round == 1 {
			return AgentResponse{Message: AgentMessage{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "one", Type: "function", Function: ToolCallFunction{Name: "first", Arguments: `{}`}},
				{ID: "two", Type: "function", Function: ToolCallFunction{Name: "second", Arguments: `{}`}},
			}}, Usage: TokenUsage{TotalTokens: 1}}, nil
		}
		return AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"done","confidence":"high","uncertainty":[],"recommended_action":"none","evidence_used":["first","second"]}`}, Usage: TokenUsage{TotalTokens: 1}}, nil
	}
	launched := make(chan chan dbos.StepOutcome[ToolResult], 2)
	toolStep := func(_ context.Context, _ int, _ time.Duration, call ToolCall) (<-chan dbos.StepOutcome[ToolResult], error) {
		ch := make(chan dbos.StepOutcome[ToolResult], 1)
		launched <- ch
		return ch, nil
	}
	type result struct {
		generation Generation
		err        error
	}
	done := make(chan result, 1)
	state := testAgentState(defaultTestBudget())
	state.Definitions = []ToolDefinition{
		{Type: "function", Function: ToolFunction{Name: "first"}},
		{Type: "function", Function: ToolFunction{Name: "second"}},
	}
	go func() {
		generation, err := service.runAgentLoopWithSteps(context.Background(), Request{Question: "test"}, Decision{Action: "investigate"}, Evidence{Summary: "test"}, state, modelStep, toolStep)
		done <- result{generation: generation, err: err}
	}()

	first := <-launched
	second := <-launched
	first <- dbos.StepOutcome[ToolResult]{Result: ToolResult{CallID: "one", Name: "first", Content: `{"ok":true}`}}
	second <- dbos.StepOutcome[ToolResult]{Result: ToolResult{CallID: "two", Name: "second", Content: `{"ok":true}`}}
	close(first)
	close(second)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.generation.Budget.ToolCallsUsed != 2 || len(out.generation.ToolsUsed) != 2 {
		t.Fatalf("unexpected generation: %#v", out.generation)
	}
}

func TestAgentLoopStopsAtBudgets(t *testing.T) {
	tests := []struct {
		name       string
		budget     AgentBudget
		startedAt  time.Time
		response   AgentResponse
		stopReason string
	}{
		{
			name:   "tool calls",
			budget: func() AgentBudget { b := defaultTestBudget(); b.MaxToolCalls = 1; return b }(),
			response: AgentResponse{
				Message: AgentMessage{Role: "assistant", ToolCalls: []ToolCall{
					{ID: "1", Type: "function", Function: ToolCallFunction{Name: "a", Arguments: `{}`}},
					{ID: "2", Type: "function", Function: ToolCallFunction{Name: "b", Arguments: `{}`}},
				}},
				Usage: TokenUsage{TotalTokens: 1},
			},
			stopReason: "tool_call_limit",
		},
		{name: "tokens", budget: func() AgentBudget { b := defaultTestBudget(); b.MaxTokens = 10; return b }(), response: AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"done","confidence":"high","uncertainty":[],"recommended_action":"none","evidence_used":[]}`}, Usage: TokenUsage{TotalTokens: 11}}, stopReason: "token_limit"},
		{name: "cost", budget: func() AgentBudget {
			b := defaultTestBudget()
			b.MaxCostUSD = 0.000001
			b.InputPricePerM = 10
			b.OutputPricePerM = 10
			return b
		}(), response: AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"done","confidence":"high","uncertainty":[],"recommended_action":"none","evidence_used":[]}`}, Usage: TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, stopReason: "cost_limit"},
		{name: "rounds", budget: func() AgentBudget { b := defaultTestBudget(); b.MaxRounds = 1; return b }(), response: AgentResponse{Message: AgentMessage{Role: "assistant", Content: `not-json`}, Usage: TokenUsage{TotalTokens: 1}}, stopReason: "round_limit"},
		{name: "missing token usage", budget: defaultTestBudget(), response: AgentResponse{Message: AgentMessage{Role: "assistant", Content: `{"answer":"done","confidence":"high","uncertainty":[],"recommended_action":"none","evidence_used":[]}`}}, stopReason: "token_usage_unavailable"},
		{name: "time", budget: defaultTestBudget(), startedAt: time.Now().Add(-2 * time.Minute), stopReason: "time_limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &Service{tools: markerToolExecutor{}, agentProvider: "test", agentModel: "model"}
			modelCalls := 0
			modelStep := func(context.Context, int, time.Duration, AgentRequest) (AgentResponse, error) {
				modelCalls++
				return tt.response, nil
			}
			toolStep := func(context.Context, int, time.Duration, ToolCall) (<-chan dbos.StepOutcome[ToolResult], error) {
				t.Fatal("tool must not execute after budget boundary")
				return nil, nil
			}
			state := testAgentState(tt.budget)
			if !tt.startedAt.IsZero() {
				state.StartedAt = tt.startedAt
			}
			generation, err := service.runAgentLoopWithSteps(context.Background(), Request{Question: "test"}, Decision{Action: "investigate"}, Evidence{Summary: "test"}, state, modelStep, toolStep)
			if err != nil {
				t.Fatal(err)
			}
			if generation.Budget.StopReason != tt.stopReason {
				t.Fatalf("stop reason=%q, want %q", generation.Budget.StopReason, tt.stopReason)
			}
			if tt.stopReason == "time_limit" && modelCalls != 0 {
				t.Fatalf("model called %d times after time budget", modelCalls)
			}
		})
	}
}
